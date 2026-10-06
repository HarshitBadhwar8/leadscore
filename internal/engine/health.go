package engine

import (
	"strings"

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
	if healthy {
		put(healthResult, "last_success_at", model.FormatTime(x.startAt))
	}
	put(healthResult, "run_id", r.ID)
	put(healthResult, "rubric_version", r.Rubric.Version())
	if s, err := x.cfg.Get("schedule"); err == nil {
		put(healthResult, "schedule", s)
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
