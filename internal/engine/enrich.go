package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// enrichCountPrefix is the State key counting a UTC day's enrichment calls:
// enrich_count:<YYYY-MM-DD> (the State table). Only today's key is kept.
const enrichCountPrefix = "enrich_count:"

// Fixed values (the engine defaults). Variables so tests can change them.
var (
	// enrichFailWait is how long a domain whose lookup failed waits before it
	// is tried again, so a domain that always fails cannot take the budget.
	enrichFailWait = 24 * time.Hour
	// enrichMaxFailuresInARow stops the run's enrichment (the Enricher
	// contract).
	enrichMaxFailuresInARow = 3
)

// Log kinds and the problem the Enrich hook writes.
const (
	logEnriched          = "enriched"
	logEnrichRateLimited = "enrich_rate_limited"
	problemEnrichFailed  = "enrich_failed"
)

// enrichMemo is what the Enrich hook did this run. It lives on the Run, not
// the model, because a phase 1 ErrTooLarge reloads the model and runs Enrich
// again, and a second one discards the model: the memo puts back the answers
// (never bought twice), the failure stamps, the day's count, the log lines,
// the warning and the error, so none of them vanish.
type enrichMemo struct {
	ran      bool
	day      string // the State key of the day the run started enriching
	stored   int    // that day's count as loaded, before this run
	answers  map[string]api.CompanyFacts
	order    []string             // answers in the order fetched
	failed   map[string]time.Time // domains whose lookup failed, and when
	calls    int                  // calls made this run, over every attempt
	found    int
	missing  int
	failures int
	stopped  bool  // enrichment is over for this run
	inARow   bool  // it stopped on failures in a row
	err      error // the error that ended it (nil for a rate limit)
	logs     []model.LogEntry
}

// enrichHook is the Enrich hook (step 4, enrichment). It looks up the domains
// that are due, within the per-run and per-day budgets, and writes the facts
// with origin `enrichment`. The engine skips it on a dry run.
//
// Every call made counts toward both budgets, whatever came back (the vendor
// rules), and the day's count goes to phase 1 with the facts it paid for. A
// rate limit stops enrichment for the run and keeps what came back before it;
// it is not a failure. A lookup that fails stamps enrich_failed_at, so that
// domain waits a day; three failures in a row stop the run's enrichment; either
// raises the warning enrich_failed. Any error from the enricher keeps the facts
// so far and fails the step.
//
// The enricher is asked about one domain per call, so the count is exact for
// any enricher, and a deadline or Stop between two calls stops new calls.
func enrichHook(r *Run) error {
	c := r.Config.Enrich
	if c == nil {
		return nil
	}
	m, now := r.Model, r.Now()
	if r.enrich == nil {
		day := enrichCountPrefix + now.Format(time.DateOnly)
		r.enrich = &enrichMemo{day: day, stored: storedCount(m, day),
			answers: map[string]api.CompanyFacts{}, failed: map[string]time.Time{}}
	}
	memo := r.enrich
	if memo.ran {
		memo.replay(r) // the redo after ErrTooLarge, on a reloaded model
	}
	memo.ran = true
	defer memo.report(r)
	if memo.stopped {
		return memo.err
	}

	budget := min(c.MaxLookupsPerRun-memo.calls, c.MaxLookupsPerDay-memo.stored-memo.calls)
	if budget <= 0 {
		return nil
	}
	due := dueDomains(m, now, c.MaxAge)
	if len(due) == 0 {
		return nil
	}
	newEnricher, ok := api.EnricherFactory(c.Type)
	if !ok {
		return memo.stop(fmt.Errorf("enrich type %q is not registered in this build", c.Type))
	}
	enr, err := newEnricher(c.Block)
	if err != nil {
		return memo.stop(fmt.Errorf("building the %s enricher: %w", c.Type, err))
	}

	// A retry wait ends at Stop or the deadline; a call in flight runs on.
	ctx := apollo.WithWaitStop(r.Ctx, r.PushCtx)
	inARow := 0
	for _, d := range due {
		if budget <= 0 || r.PushCtx.Err() != nil {
			return nil
		}
		if _, done := memo.answers[d]; done {
			continue // fetched by an earlier attempt and already written
		}
		facts, err := enr.Enrich(ctx, []string{d}, 1)
		memo.calls++
		budget--
		answered := false
		for _, f := range facts {
			if f.Domain != "" && merge.NormalizeDomain(f.Domain) != d {
				continue // an answer for a domain not asked about
			}
			// The run's clock is the fetch time, so `at` and the max age
			// read one clock.
			f.FetchedAt = r.Now()
			memo.answers[d] = f
			memo.order = append(memo.order, d)
			writeFacts(m, d, f, now)
			answered = true
			if f.NotFound {
				memo.missing++
			} else {
				memo.found++
			}
		}
		switch {
		case errors.Is(err, apollo.ErrWaitStopped):
			memo.stopped = true // Stop or the deadline during a retry wait: not a rate limit, not a failure
			return nil
		case errors.Is(err, api.ErrRateLimited):
			memo.stopped = true
			memo.log(r, "warn", logEnrichRateLimited, "enrichment was rate limited; the rest waits for the next run")
			return nil
		case err != nil:
			return memo.stop(fmt.Errorf("enrichment stopped: %w", err))
		case answered:
			inARow = 0
		default:
			at := r.Now()
			memo.failed[d] = at
			writeFailed(m, d, at)
			memo.failures++
			if inARow++; inARow >= enrichMaxFailuresInARow {
				memo.stopped, memo.inARow = true, true
				return nil
			}
		}
	}
	return nil
}

func (memo *enrichMemo) stop(err error) error {
	memo.stopped, memo.err = true, err
	return err
}

// log writes a log line and keeps it, so a redo can put it back.
func (memo *enrichMemo) log(r *Run, level, kind, msg string) {
	e := model.LogEntry{At: r.Now(), RunID: r.ID, Level: level, Kind: kind, Message: msg, RubricVersion: r.Rubric.Version()}
	memo.logs = append(memo.logs, e)
	r.Model.Put(model.TableLog, e)
}

// replay puts this run's enrichment back into a reloaded or discarded model:
// the answers and failure stamps (no call is made again) and the log lines.
// report then writes the count, the summary line and the warning.
func (memo *enrichMemo) replay(r *Run) {
	m, now := r.Model, r.Now()
	for _, d := range memo.order {
		writeFacts(m, d, memo.answers[d], now)
	}
	for d, at := range memo.failed {
		if _, answered := memo.answers[d]; !answered {
			writeFailed(m, d, at)
		}
	}
	for _, e := range memo.logs {
		m.Put(model.TableLog, e)
	}
}

// report records what the run's enrichment did so far: the day's count (and
// drops other days' counts), the summary log line, and the enrich_failed
// warning. Each is written once per model, so a redo writes them again.
func (memo *enrichMemo) report(r *Run) {
	m := r.Model
	cleanEnrichCounts(m, memo.day)
	if memo.calls > 0 {
		m.SetState(memo.day, strconv.Itoa(memo.stored+memo.calls))
	}
	if memo.found+memo.missing+memo.failures > 0 {
		m.Put(model.TableLog, model.LogEntry{At: r.Now(), RunID: r.ID, Level: "info", Kind: logEnriched,
			Message: fmt.Sprintf("enrichment: %d found, %d not found, %d failed, %d calls this run",
				memo.found, memo.missing, memo.failures, memo.calls),
			RubricVersion: r.Rubric.Version()})
	}
	if memo.failures > 0 {
		msg := fmt.Sprintf("%d company lookups failed this run; each waits a day before it is tried again", memo.failures)
		if memo.inARow {
			msg += fmt.Sprintf(", and enrichment stopped for this run after %d failures in a row", enrichMaxFailuresInARow)
		}
		r.Problem(problemEnrichFailed, msg, "if it repeats, check the enrichment vendor's status and the enrich block", true)
	}
}

// keepEnrichment is called after a second phase 1 ErrTooLarge discards the
// model: the calls were made, so their facts and the day's count go into the
// model again and phase 2 saves them.
func keepEnrichment(r *Run) {
	if r.enrich == nil || !r.enrich.ran {
		return
	}
	r.enrich.replay(r)
	r.enrich.report(r)
}

// storedCount is the day's committed enrichment count; an unreadable value
// counts as the whole day spent rather than as zero.
func storedCount(m *model.Model, day string) int {
	v := m.StateValue(day)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 1 << 30
	}
	return n
}

// cleanEnrichCounts deletes the counts of other days: only today's is read.
func cleanEnrichCounts(m *model.Model, today string) {
	for k := range m.State {
		if s := string(k); strings.HasPrefix(s, enrichCountPrefix) && s != today {
			m.Delete(model.TableState, []string{s})
		}
	}
}

// dueDomains lists the domains to look up, in the order to look them up:
// every live lead's company domain and every Company facts row (a domain seen
// only through a company visit has one), when neither enriched_at nor
// not_found_at is within maxAge and no lookup failed within the last day.
// A personal mail provider's domain, or a name with no dot, is never looked
// up. Never-tried domains go first, then the least recently tried (a failure
// counts as a try), then by name.
func dueDomains(m *model.Model, now time.Time, maxAge time.Duration) []string {
	type seen struct{ answered, tried time.Time }
	all := map[string]seen{}
	for _, cf := range m.CompanyFacts {
		if cf.Domain == "" {
			continue
		}
		s := seen{answered: cf.EnrichedAt}
		if cf.NotFoundAt.After(s.answered) {
			s.answered = cf.NotFoundAt
		}
		s.tried = s.answered
		if cf.EnrichFailedAt.After(s.tried) {
			s.tried = cf.EnrichFailedAt
		}
		if !cf.EnrichFailedAt.IsZero() && cf.EnrichFailedAt.After(now.Add(-enrichFailWait)) {
			continue // failed within the last day
		}
		all[cf.Domain] = s
	}
	for _, p := range m.People {
		if p.MergedInto != "" {
			continue
		}
		d := p.Fields[merge.FieldDomain].Value
		if _, known := m.CompanyFacts[model.Key(d)]; d != "" && !known {
			all[d] = seen{}
		}
	}
	cutoff := now.Add(-maxAge)
	var out []string
	for d, s := range all {
		if !strings.Contains(d, ".") || merge.IsPersonalProvider(d) {
			continue
		}
		if s.answered.IsZero() || !s.answered.After(cutoff) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := all[out[i]].tried, all[out[j]].tried
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i] < out[j]
	})
	return out
}

// writeFailed stamps a failed lookup on the domain; its facts stay as they are.
func writeFailed(m *model.Model, domain string, at time.Time) {
	cf := cloneFacts(m.CompanyFacts[model.Key(domain)])
	cf.Domain = domain
	cf.EnrichFailedAt = at
	m.Put(model.TableCompanyFacts, cf)
}

// writeFacts records one answer in Company facts (the store tables). A
// not-found sets not_found_at and leaves the facts alone. Found facts are
// written with origin enrichment, never over a companies_tab fact:
//   - a field the vendor left out (empty) keeps the stored fact;
//   - an Extra key with an empty value is a value the vendor sent that cannot
//     be used (an unknown funding label): the stored fact moves to previous;
//   - a value equal to the stored one changes nothing but the origin (an
//     input value is taken over);
//   - a changed value moves the old entry to previous and sets at to the
//     fetch time.
//
// Extra keys are applied after the typed fields; an exact company.<name> key
// replaces <name> before anything is written, so it wins. enriched_at moves on
// every found answer, and an answer clears enrich_failed_at.
func writeFacts(m *model.Model, domain string, f api.CompanyFacts, now time.Time) {
	at := f.FetchedAt.UTC()
	if at.IsZero() {
		at = now
	}
	cf := cloneFacts(m.CompanyFacts[model.Key(domain)])
	cf.Domain = domain
	cf.EnrichFailedAt = time.Time{}
	if f.NotFound {
		cf.NotFoundAt = at
		m.Put(model.TableCompanyFacts, cf)
		return
	}
	cf.EnrichedAt, cf.NotFoundAt = at, time.Time{}
	set := func(name, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		cur, has := cf.Facts[name]
		switch {
		case has && cur.Origin == merge.OriginCompaniesTab:
		case has && cur.Value == v:
			cur.Origin = merge.OriginEnrichment
			cf.Facts[name] = cur
		case has:
			cf.Previous[name] = cur
			cf.Facts[name] = model.Fact{Value: v, Origin: merge.OriginEnrichment, At: at}
		default:
			cf.Facts[name] = model.Fact{Value: v, Origin: merge.OriginEnrichment, At: at}
		}
	}
	drop := func(name string) {
		if cur, has := cf.Facts[name]; has && cur.Origin != merge.OriginCompaniesTab {
			cf.Previous[name] = cur
			delete(cf.Facts, name)
		}
	}
	typed := map[string]string{"name": f.Name, "region": f.Region, "funding_stage": f.FundingStage}
	if f.Employees != nil {
		typed["employees"] = strconv.Itoa(*f.Employees)
	}
	for _, name := range []string{"name", "region", "funding_stage", "employees"} {
		set(name, typed[name])
	}
	// One value per name, an exact company.<name> key over <name>, then one
	// write per name, so `previous` holds the stored value and a plain key can
	// never undo its prefixed twin.
	extra := map[string]string{}
	for k, v := range f.Extra {
		if !strings.HasPrefix(k, "company.") {
			extra[k] = strings.TrimSpace(v)
		}
	}
	for k, v := range f.Extra {
		if name, ok := strings.CutPrefix(k, "company."); ok {
			extra[name] = strings.TrimSpace(v)
		}
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := extra[name]
		if name == "" || name == "domain" {
			continue
		}
		if _, isTyped := typed[name]; isTyped || name == "employees" {
			if v == "" && strings.TrimSpace(typed[name]) == "" {
				drop(name) // the vendor sent a value for it that cannot be used
			}
			continue // otherwise the typed field carries it
		}
		if v == "" {
			drop(name)
		} else {
			set(name, v)
		}
	}
	m.Put(model.TableCompanyFacts, cf)
}

func cloneFacts(c model.CompanyFact) model.CompanyFact {
	f := make(map[string]model.Fact, len(c.Facts))
	for k, v := range c.Facts {
		f[k] = v
	}
	p := make(map[string]model.Fact, len(c.Previous))
	for k, v := range c.Previous {
		p[k] = v
	}
	c.Facts, c.Previous = f, p
	return c
}
