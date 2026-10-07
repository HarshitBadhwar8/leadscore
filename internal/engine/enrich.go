package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// enrichCountPrefix is the State key counting a UTC day's enrichment calls:
// enrich_count:<YYYY-MM-DD> (contracts section 4).
const enrichCountPrefix = "enrich_count:"

// Log kinds the Enrich hook writes.
const (
	logEnriched          = "enriched"
	logEnrichRateLimited = "enrich_rate_limited"
)

// enrichMemo is what the Enrich hook learned this run. It lives on the Run,
// not the model, so the redo after a phase 1 ErrTooLarge (which reloads the
// model and runs Enrich again) re-applies these answers instead of buying
// them twice, and still counts the calls toward enrich_count.
type enrichMemo struct {
	answers map[string]api.CompanyFacts // by domain, in the order fetched
	order   []string
	calls   int  // calls made this run, over every attempt
	stopped bool // a rate limit or an error ended enrichment for this run
}

// enrichHook is the Enrich hook (step 4, RFC 6.8). It looks up the domains
// that are due (no lookup yet, or the last lookup or not-found older than
// enrich.max_age), within the per-run and per-day budgets, and writes the
// facts with origin `enrichment`. The engine skips it on a dry run.
//
// Every call made counts toward both budgets, whatever came back (contracts
// section 6), and the day's count goes to phase 1 with the facts it paid for.
// A rate limit stops enrichment for the run and keeps what came back before
// it; it is not a failure. Any other error from the enricher keeps the facts
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
		r.enrich = &enrichMemo{answers: map[string]api.CompanyFacts{}}
	}
	memo := r.enrich

	// Answers bought by an earlier attempt of this run go back in first.
	for _, d := range memo.order {
		writeFacts(m, d, memo.answers[d], now)
	}
	day := enrichCountPrefix + now.Format(time.DateOnly)
	stored := storedCount(m, day)
	cleanEnrichCounts(m, day)
	defer func() {
		if memo.calls > 0 {
			m.SetState(day, strconv.Itoa(stored+memo.calls))
		}
	}()

	budget := min(c.MaxLookupsPerRun-memo.calls, c.MaxLookupsPerDay-stored-memo.calls)
	if memo.stopped || budget <= 0 {
		return nil
	}
	due := dueDomains(m, now, c.MaxAge)
	if len(due) == 0 {
		return nil
	}
	newEnricher, ok := api.EnricherFactory(c.Type)
	if !ok {
		memo.stopped = true
		return fmt.Errorf("enrich type %q is not registered in this build", c.Type)
	}
	enr, err := newEnricher(c.Block)
	if err != nil {
		memo.stopped = true
		return fmt.Errorf("building the %s enricher: %w", c.Type, err)
	}

	var found, missing int
	defer func() {
		if found+missing > 0 {
			r.Model.Put(model.TableLog, model.LogEntry{At: now, RunID: r.ID, Level: "info", Kind: logEnriched,
				Message:       fmt.Sprintf("enrichment: %d found, %d not found, %d calls this run", found, missing, memo.calls),
				RubricVersion: r.Rubric.Version()})
		}
	}()
	for _, d := range due {
		if budget <= 0 || r.PushCtx.Err() != nil {
			return nil
		}
		if _, done := memo.answers[d]; done {
			continue // fetched by an earlier attempt and already written
		}
		facts, err := enr.Enrich(r.Ctx, []string{d}, 1)
		memo.calls++
		budget--
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
			if f.NotFound {
				missing++
			} else {
				found++
			}
		}
		switch {
		case errors.Is(err, api.ErrRateLimited):
			memo.stopped = true
			r.Model.Put(model.TableLog, model.LogEntry{At: now, RunID: r.ID, Level: "warn", Kind: logEnrichRateLimited,
				Message: "enrichment was rate limited; the rest waits for the next run", RubricVersion: r.Rubric.Version()})
			return nil
		case err != nil:
			memo.stopped = true
			return fmt.Errorf("enrichment stopped: %w", err)
		}
	}
	return nil
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
// not_found_at is within maxAge. Never-tried domains go first, then the
// stalest, then by name.
func dueDomains(m *model.Model, now time.Time, maxAge time.Duration) []string {
	last := map[string]time.Time{}
	for _, cf := range m.CompanyFacts {
		if cf.Domain == "" {
			continue
		}
		t := cf.EnrichedAt
		if cf.NotFoundAt.After(t) {
			t = cf.NotFoundAt
		}
		last[cf.Domain] = t
	}
	for _, p := range m.People {
		if p.MergedInto != "" {
			continue
		}
		if d := p.Fields[merge.FieldDomain].Value; d != "" {
			if _, ok := last[d]; !ok {
				last[d] = time.Time{}
			}
		}
	}
	cutoff := now.Add(-maxAge)
	var out []string
	for d, t := range last {
		if t.IsZero() || !t.After(cutoff) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := last[out[i]], last[out[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i] < out[j]
	})
	return out
}

// writeFacts records one answer in Company facts (contracts section 4). A
// not-found sets not_found_at and leaves the facts alone. Found facts are
// written with origin enrichment, never over a companies_tab fact; an empty
// value is "Apollo did not say" and changes nothing; a value equal to the
// stored one changes nothing but the origin (an input value is taken over);
// a changed value moves the old entry to `previous` and sets `at` to the
// fetch time. enriched_at moves on every found answer.
func writeFacts(m *model.Model, domain string, f api.CompanyFacts, now time.Time) {
	at := f.FetchedAt.UTC()
	if at.IsZero() {
		at = now
	}
	cf := cloneFacts(m.CompanyFacts[model.Key(domain)])
	cf.Domain = domain
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
	set("name", f.Name)
	set("region", f.Region)
	set("funding_stage", f.FundingStage)
	if f.Employees != nil {
		set("employees", strconv.Itoa(*f.Employees))
	}
	extra := make([]string, 0, len(f.Extra))
	for k := range f.Extra {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		name := strings.TrimPrefix(k, "company.")
		switch name {
		case "", "name", "region", "funding_stage", "employees", "domain":
			continue // the typed fields carry these
		}
		set(name, f.Extra[k])
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
