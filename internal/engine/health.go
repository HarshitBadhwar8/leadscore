package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// Health kinds and result keys (contracts section 4).
const (
	healthResult  = "result"
	healthProblem = "problem"
)

// putHealth records this run's result and its open problems in the model's
// Health table. A run that finished (final) rewrites the problem rows: it keeps
// first_seen_at for a problem still open and deletes every problem not raised
// this run. A run that failed part way keeps the problems it did not get to
// check, since it cannot tell them resolved.
func (x *exec) putHealth(final bool) {
	r, m := x.run, x.run.Model
	now := r.Now()
	healthy := x.healthy()
	put := func(kind, key, value string) {
		row := m.Health[model.K(kind, key)]
		if row.Key == "" || row.FirstSeenAt.IsZero() {
			row.FirstSeenAt = now
		}
		if row.Value == value && row.Kind == kind && row.Key == key {
			return // unchanged; keep updated_at too, so a quiet table writes nothing
		}
		row.Kind, row.Key, row.Value, row.UpdatedAt = kind, key, value, now
		m.Put(model.TableHealth, row)
	}
	result := "unhealthy"
	if healthy {
		result = "healthy"
	}
	put(healthResult, "last_result", result)
	put(healthResult, "last_run_at", model.FormatTime(x.startAt))
	// A success is a healthy run that scored: a run stopped before scoring
	// (run_stopped is only a warning) did not succeed.
	if healthy && x.scored {
		put(healthResult, "last_success_at", model.FormatTime(x.startAt))
	}
	if r.ID != "" {
		put(healthResult, "run_id", r.ID)
	}
	if r.Rubric != nil {
		put(healthResult, "rubric_version", r.Rubric.Version())
	}
	if x.cfg != nil {
		if s, err := x.cfg.Get("schedule"); err == nil {
			put(healthResult, "schedule", s)
		}
	}

	x.mu.Lock()
	raised := make(map[string]problem, len(x.problems))
	for k, p := range x.problems {
		raised[k] = p
	}
	x.mu.Unlock()
	for k, p := range raised {
		put(healthProblem, k, problemValue(p))
	}
	if !final {
		return
	}
	for k, row := range m.Health {
		if row.Kind != healthProblem {
			continue
		}
		if _, open := raised[row.Key]; !open {
			m.Delete(model.TableHealth, k.Parts())
		}
	}
}

// problemValue is the Health value of a problem: the message, then the fix.
func problemValue(p problem) string {
	var b strings.Builder
	if p.warning {
		b.WriteString("warning: ")
	}
	b.WriteString(strings.TrimSuffix(p.message, "."))
	if p.fix != "" {
		b.WriteString(". Fix: " + strings.TrimSuffix(p.fix, "."))
	}
	b.WriteString(".")
	return b.String()
}

// saveFailure writes Health for a run that failed after taking the lease and
// loading the store: the result is unhealthy and run_failed says why. Nothing
// else of the run is saved. A lost lease or a dry run writes nothing.
func (x *exec) saveFailure() {
	r := x.run
	if r.DryRun || x.lost || r.Lease == nil || r.Model == nil {
		return
	}
	x.putHealth(false)
	_ = x.commit("Health", codec.Encode(r.Model, model.TableHealth), false)
}

// lateProblem records a problem found after phase 2 committed (writing
// Ranked, AfterSave) with one more Health write, so Health shows it.
func (x *exec) lateProblem(key, message, fix string) {
	x.problem(key, message, fix, false)
	if x.lost {
		return
	}
	x.putHealth(true)
	_ = x.commit("Health", codec.Encode(x.run.Model, model.TableHealth), false)
}

// crashLeaseTTL bounds the lease RecordCrash takes for its one write.
const crashLeaseTTL = 2 * time.Minute

// RecordCrash writes Health for a run that crashed before it could write it
// itself (a panic that escaped the run, recovered by serve's timer): the
// result unhealthy, last_run_at startAt, and run_failed naming err, keeping
// every other open problem, as a failed run does (contracts section 5.1). It
// writes under the run lease, taken for this write only. When another run
// holds the lease it writes nothing and returns an error wrapping
// api.ErrLeaseHeld: that run writes Health itself.
//
// It reads only the Health table, never the whole store, so it still writes
// when loading the store is what failed (a newer schema, a table that cannot
// be read), and it recovers a panic of its own into its error, so the
// receiver calling it stays up.
func RecordCrash(ctx context.Context, store api.Backend, startAt time.Time, err error) (rerr error) {
	defer func() {
		if p := recover(); p != nil {
			rerr = fmt.Errorf("recording the crash panicked: %v", p)
		}
	}()
	id, uerr := uuid.NewV7()
	if uerr != nil {
		return uerr
	}
	lease, lerr := store.Lease(ctx, id.String(), crashLeaseTTL)
	if lerr != nil {
		return fmt.Errorf("taking the run lease to record the crash: %w", lerr)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = lease.Release(rctx)
	}()
	rows, lerr := store.ReadTable(ctx, model.TableHealth)
	if lerr != nil {
		return fmt.Errorf("reading Health to record the crash: %w", lerr)
	}
	m := model.New()
	if lerr := m.Load(model.TableHealth, rows); lerr != nil {
		return fmt.Errorf("reading Health to record the crash: %w", lerr)
	}
	x := &exec{
		store: store, startAt: startAt.UTC(), problems: map[string]problem{}, failed: true,
		run: &Run{Ctx: ctx, Model: m, Lease: lease, Now: func() time.Time { return time.Now().UTC() }},
	}
	msg := "the run crashed"
	if err != nil {
		msg = "the run stopped: " + logredact.Redact(err.Error())
	}
	x.problem("run_failed", msg, "fix the cause in the message; the next run tries again", false)
	x.putHealth(false)
	return x.commit("Health", codec.Encode(m, model.TableHealth), false)
}
