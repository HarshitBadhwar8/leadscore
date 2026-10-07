package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

const testAPIKey = "test-key-123"

// enrichInstall is an install whose enrich block points at a fresh fake, with
// leads at the given domains (one lead each).
func enrichInstall(t *testing.T, enrich string, domains ...string) (*install, *fakeapollo.Server, func(*api.RunOptions, *settings)) {
	t.Helper()
	return enrichInstallOn(t, "", enrich, domains...)
}

// enrichInstallOn is enrichInstall with a store line (empty: SQLite).
func enrichInstallOn(t *testing.T, store, enrich string, domains ...string) (*install, *fakeapollo.Server, func(*api.RunOptions, *settings)) {
	t.Helper()
	t.Setenv("APOLLO_API_KEY", testAPIKey)
	fake := fakeapollo.New(testAPIKey)
	fake.SetRetryAfter("0")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	in := newInstall(t, store+leadsCSV+"enrich: { type: apollo, base_url: "+srv.URL+enrich+" }\n", testRubric)
	lines := []string{"Email,Name,Title"}
	for i, d := range domains {
		lines = append(lines, fmt.Sprintf("p%d@%s,P %d,Clerk", i, d, i))
	}
	in.write("leads.csv", csvText(lines...))
	client := func(_ *api.RunOptions, s *settings) { s.client = srv.Client() }
	return in, fake, client
}

func at(now time.Time) func(*api.RunOptions, *settings) {
	return func(_ *api.RunOptions, s *settings) { s.now = func() time.Time { return now } }
}

// companyFacts reads Company facts as stored: domain -> the row.
func companyFacts(in *install) map[string]api.Row {
	out := map[string]api.Row{}
	for _, r := range in.rows(model.TableCompanyFacts) {
		out[r["domain"]] = r
	}
	return out
}

type storedFact struct {
	Value  string `json:"value"`
	Origin string `json:"origin"`
	At     string `json:"at"`
}

func factsOf(t *testing.T, row api.Row, col string) map[string]storedFact {
	t.Helper()
	out := map[string]storedFact{}
	if row[col] == "" {
		return out
	}
	if err := json.Unmarshal([]byte(row[col]), &out); err != nil {
		t.Fatalf("%s %q: %v", col, row[col], err)
	}
	return out
}

func intp(n int) *int { return &n }

// Proof: the per-run budget caps one run's calls, the per-day budget caps
// the day across runs, and every call counts toward enrich_count.
func TestEnrichBudgets(t *testing.T) {
	domains := []string{"a.example", "b.example", "c.example", "d.example", "e.example"}
	in, fake, client := enrichInstall(t, ", max_lookups_per_run: 2, max_lookups_per_day: 3", domains...)
	for _, d := range domains {
		fake.AddOrg(d, fakeapollo.Org{Name: strings.ToUpper(d[:1]) + " Co"})
	}
	day := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	if _, out, err := in.run(DefaultHooks(), client, at(day)); err != nil {
		t.Fatalf("run 1: %v\n%s", err, out)
	}
	if got := fake.EnrichCalls(); !slices.Equal(got, []string{"a.example", "b.example"}) {
		t.Fatalf("run 1 called %v, want the first two domains (per-run budget 2)", got)
	}
	if got := in.state("enrich_count:2026-10-07"); got != "2" {
		t.Errorf("enrich_count after run 1 = %q, want 2", got)
	}

	if _, out, err := in.run(DefaultHooks(), client, at(day.Add(time.Hour))); err != nil {
		t.Fatalf("run 2: %v\n%s", err, out)
	}
	if got := fake.EnrichCalls(); len(got) != 3 || got[2] != "c.example" {
		t.Fatalf("calls after run 2 = %v, want one more (per-day budget 3)", got)
	}
	if got := in.state("enrich_count:2026-10-07"); got != "3" {
		t.Errorf("enrich_count after run 2 = %q, want 3", got)
	}

	// The day is spent: no call at all.
	if _, _, err := in.run(DefaultHooks(), client, at(day.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.EnrichCalls()); n != 3 {
		t.Fatalf("a run on a spent day made calls: %d in all", n)
	}

	// The next UTC day has a fresh budget, and the old day's count is dropped.
	if _, _, err := in.run(DefaultHooks(), client, at(day.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got := fake.EnrichCalls(); len(got) != 5 || got[3] != "d.example" || got[4] != "e.example" {
		t.Fatalf("calls after the next day's run = %v", got)
	}
	if in.state("enrich_count:2026-10-08") != "2" || in.state("enrich_count:2026-10-07") != "" {
		t.Errorf("counts: today %q, yesterday %q", in.state("enrich_count:2026-10-08"), in.state("enrich_count:2026-10-07"))
	}
	if cf := companyFacts(in); factsOf(t, cf["e.example"], "facts")["name"].Value != "E Co" {
		t.Errorf("e.example facts: %v", cf["e.example"])
	}
}

// Proof: a rate limit keeps the facts fetched before it, counts every call
// made (the rate-limited one too), and is not a failed step.
func TestEnrichRateLimitKeepsPartialFacts(t *testing.T) {
	in, fake, client := enrichInstall(t, "", "a.example", "b.example", "c.example")
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A Co", Employees: intp(40)})
	fake.AddOrg("b.example", fakeapollo.Org{Name: "B Co"})
	fake.AddOrg("c.example", fakeapollo.Org{Name: "C Co"})
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	fake.RateLimitAfter(1, 1000) // the first call is answered, every attempt after it is 429

	res, out, err := in.run(DefaultHooks(), client, at(now))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if hasKey(res.Problems, "step_failed:enrich") {
		t.Errorf("a rate limit failed the step: %v", res.Problems)
	}
	cf := companyFacts(in)
	if f := factsOf(t, cf["a.example"], "facts"); f["name"].Value != "A Co" || f["employees"].Value != "40" || f["name"].Origin != "enrichment" {
		t.Errorf("a.example facts before the limit were not kept: %v", cf["a.example"])
	}
	if cf["b.example"]["enriched_at"] != "" || cf["c.example"]["enriched_at"] != "" {
		t.Errorf("domains after the limit were written: %v %v", cf["b.example"], cf["c.example"])
	}
	// a.example once, b.example three attempts (retried, then given up): two calls.
	if got := in.state("enrich_count:2026-10-07"); got != "2" {
		t.Errorf("enrich_count = %q, want 2 (one answered, one rate limited)", got)
	}
	if got := fake.EnrichCalls(); len(got) != 4 || got[0] != "a.example" || got[3] != "b.example" {
		t.Errorf("calls %v: c.example must not be tried after the limit", got)
	}
	logged := false
	for _, r := range in.rows(model.TableLog) {
		logged = logged || r["kind"] == logEnrichRateLimited
	}
	if !logged {
		t.Error("no enrich_rate_limited log line")
	}
}

// Proof: a not-found domain is recorded and not looked up again until the
// max age has passed.
func TestEnrichNotFoundWaitsForMaxAge(t *testing.T) {
	in, fake, client := enrichInstall(t, ", max_age: 30d", "ghost.example")
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	if _, _, err := in.run(DefaultHooks(), client, at(now)); err != nil {
		t.Fatal(err)
	}
	cf := companyFacts(in)["ghost.example"]
	if cf["not_found_at"] == "" || cf["enriched_at"] != "" || cf["facts"] != "" && cf["facts"] != "{}" {
		t.Fatalf("not-found row: %v", cf)
	}
	for _, d := range []time.Duration{24 * time.Hour, 29 * 24 * time.Hour} {
		if _, _, err := in.run(DefaultHooks(), client, at(now.Add(d))); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake.EnrichCalls()); n != 1 {
		t.Fatalf("looked up %d times inside the max age, want 1", n)
	}
	fake.AddOrg("ghost.example", fakeapollo.Org{Name: "Ghost Co"})
	if _, _, err := in.run(DefaultHooks(), client, at(now.Add(31*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.EnrichCalls()); n != 2 {
		t.Fatalf("looked up %d times after the max age, want 2", n)
	}
	cf = companyFacts(in)["ghost.example"]
	if factsOf(t, cf, "facts")["name"].Value != "Ghost Co" || cf["not_found_at"] != "" || cf["enriched_at"] == "" {
		t.Errorf("found after the max age: %v", cf)
	}
}

// Proof: a refresh that changes a value moves the old entry to `previous`
// and sets `at` to the fetch time; an unchanged value moves only enriched_at.
func TestEnrichRefreshFillsPrevious(t *testing.T) {
	in, fake, client := enrichInstall(t, ", max_age: 30d", "acme.example")
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme", Employees: intp(100), FundingStage: "Series A", Country: " India "})
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if _, _, err := in.run(DefaultHooks(), client, at(t0)); err != nil {
		t.Fatal(err)
	}
	f0 := factsOf(t, companyFacts(in)["acme.example"], "facts")
	if f0["funding_stage"].Value != "series_a" || f0["region"].Value != "India" || f0["employees"].Value != "100" {
		t.Fatalf("first facts: %v", f0)
	}

	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme", Employees: intp(180), FundingStage: "Series B", Country: "India"})
	t1 := t0.Add(31 * 24 * time.Hour)
	if _, _, err := in.run(DefaultHooks(), client, at(t1)); err != nil {
		t.Fatal(err)
	}
	row := companyFacts(in)["acme.example"]
	facts, prev := factsOf(t, row, "facts"), factsOf(t, row, "previous")
	if facts["employees"].Value != "180" || facts["funding_stage"].Value != "series_b" {
		t.Errorf("refreshed facts: %v", facts)
	}
	if prev["employees"].Value != "100" || prev["funding_stage"].Value != "series_a" {
		t.Errorf("previous = %v, want the replaced values", prev)
	}
	if _, has := prev["name"]; has {
		t.Errorf("an unchanged name moved to previous: %v", prev)
	}
	if facts["employees"].At != model.FormatTime(t1) || facts["name"].At != model.FormatTime(t0) {
		t.Errorf("at: employees %s (want %s), name %s (want unchanged %s)",
			facts["employees"].At, model.FormatTime(t1), facts["name"].At, model.FormatTime(t0))
	}
	if row["enriched_at"] != model.FormatTime(t1) {
		t.Errorf("enriched_at = %s, want %s", row["enriched_at"], model.FormatTime(t1))
	}
}

// Enrichment never overwrites a Companies tab fact, but fills one whose
// cell was emptied (merge moved it to previous).
func TestEnrichNeverOverCompaniesTab(t *testing.T) {
	m := model.New()
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "acme.example",
		Facts: map[string]model.Fact{
			"name":      {Value: "Acme (our name)", Origin: "companies_tab", At: time.Unix(0, 0).UTC()},
			"employees": {Value: "75", Origin: "input", At: time.Unix(0, 0).UTC()},
		},
		Previous: map[string]model.Fact{"region": {Value: "France", Origin: "companies_tab", At: time.Unix(0, 0).UTC()}},
	})
	writeFacts(m, "acme.example", api.CompanyFacts{Domain: "acme.example", Name: "Acme Vendor Name",
		Employees: intp(90), Region: "Germany", FetchedAt: time.Unix(100, 0).UTC()}, time.Unix(100, 0).UTC())
	cf := m.CompanyFacts[model.Key("acme.example")]
	if cf.Facts["name"].Value != "Acme (our name)" || cf.Facts["name"].Origin != "companies_tab" {
		t.Errorf("a Companies tab fact was overwritten: %v", cf.Facts["name"])
	}
	if cf.Facts["employees"].Value != "90" || cf.Facts["employees"].Origin != "enrichment" || cf.Previous["employees"].Value != "75" {
		t.Errorf("an input fact was not replaced by enrichment: %v / %v", cf.Facts["employees"], cf.Previous["employees"])
	}
	if cf.Facts["region"].Value != "Germany" || cf.Previous["region"].Value != "France" {
		t.Errorf("an emptied Companies tab fact was not filled: %v / %v", cf.Facts["region"], cf.Previous["region"])
	}
}

// A dry run makes no enrichment call.
func TestEnrichSkippedOnDryRun(t *testing.T) {
	in, fake, client := enrichInstall(t, "", "acme.example")
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme"})
	if _, out, err := in.run(DefaultHooks(), client, dry); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := len(fake.EnrichCalls()); n != 0 {
		t.Errorf("a dry run made %d enrichment calls", n)
	}
}

// The redo after a phase 1 ErrTooLarge reloads the model and runs Enrich
// again: the answers bought the first time are re-applied, not bought again,
// and still counted.
func TestEnrichNotBoughtTwiceOnTooLargeRedo(t *testing.T) {
	in, fake, client := enrichInstallOn(t, "store: { type: flaky, path: leadscore.db }\n", "", "a.example", "b.example")
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A Co"})
	fake.AddOrg("b.example", fakeapollo.Org{Name: "B Co"})
	flaky.Lock()
	flaky.tooLarge = 1
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.tooLarge = 0; flaky.Unlock() })
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	if _, out, err := in.run(DefaultHooks(), client, at(now)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := fake.EnrichCalls(); len(got) != 2 {
		t.Fatalf("calls %v: the redo bought answers again", got)
	}
	if got := in.state("enrich_count:2026-10-07"); got != "2" {
		t.Errorf("enrich_count = %q, want the first attempt's 2 calls", got)
	}
	// The redo merged half the rows, but both answers were paid for and saved.
	cf := companyFacts(in)
	for _, d := range []string{"a.example", "b.example"} {
		if cf[d]["enriched_at"] == "" {
			t.Errorf("%s: the answer bought before the redo was not saved: %v", d, cf[d])
		}
	}
}

// A refused key stops enrichment after one call and fails the step (the run
// goes on, unhealthy); the in-run apollo-key check names the key.
func TestEnrichBadKeyFailsTheStep(t *testing.T) {
	in, fake, client := enrichInstall(t, "", "a.example", "b.example")
	t.Setenv("APOLLO_API_KEY", "wrong")
	res, out, err := in.run(DefaultHooks(), client)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Healthy || !hasKey(res.Problems, "step_failed:enrich") || !hasKey(res.Problems, "apollo-key:auth") {
		t.Errorf("healthy %v, problems %v", res.Healthy, res.Problems)
	}
	if n := len(fake.EnrichCalls()); n != 1 {
		t.Errorf("%d enrichment calls with a refused key, want 1", n)
	}
	if len(in.rows(model.TableRanked)) != 2 {
		t.Error("the run did not score after enrichment failed")
	}
}

func setTooLarge(t *testing.T, n int) {
	flaky.Lock()
	flaky.tooLarge = n
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.tooLarge = 0; flaky.Unlock() })
}

func logKinds(in *install) map[string]int {
	out := map[string]int{}
	for _, r := range in.rows(model.TableLog) {
		out[r["kind"]]++
	}
	return out
}

// A failed Enrich survives the ErrTooLarge redo: the redo returns the same
// error, so step_failed:enrich stays and the lanes count as judged on
// partial inputs.
func TestEnrichFailureSurvivesTooLargeRedo(t *testing.T) {
	in, fake, client := enrichInstallOn(t, "store: { type: flaky, path: leadscore.db }\n", "", "a.example", "b.example")
	t.Setenv("APOLLO_API_KEY", "wrong")
	setTooLarge(t, 1)
	res, out, err := in.run(DefaultHooks(), client)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !hasKey(res.Problems, "step_failed:enrich") {
		t.Errorf("the enrich failure vanished on the redo: %v", res.Problems)
	}
	if n := len(fake.EnrichCalls()); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}
	if got := in.state("enrich_count:" + time.Now().UTC().Format(time.DateOnly)); got != "1" {
		t.Errorf("enrich_count = %q, want 1", got)
	}
}

// A rate limit's log line, and the summary line, survive the redo.
func TestEnrichRateLimitLogSurvivesTooLargeRedo(t *testing.T) {
	in, fake, client := enrichInstallOn(t, "store: { type: flaky, path: leadscore.db }\n", "", "a.example", "b.example")
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A Co"})
	fake.RateLimitAfter(1, 1000)
	setTooLarge(t, 1)
	res, out, err := in.run(DefaultHooks(), client)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if hasKey(res.Problems, "step_failed:enrich") {
		t.Errorf("a rate limit failed the step: %v", res.Problems)
	}
	k := logKinds(in)
	if k[logEnrichRateLimited] != 1 || k[logEnriched] != 1 {
		t.Errorf("log kinds after the redo: %v", k)
	}
	if n := len(fake.EnrichCalls()); n != 4 {
		t.Errorf("%d calls, want 4 (one answer, three rate-limited attempts)", n)
	}
}

// Phase 1 too large twice: the model is discarded, but the calls were made,
// so phase 2 saves their count and facts.
func TestEnrichCountSurvivesDiscard(t *testing.T) {
	in, fake, client := enrichInstallOn(t, "store: { type: flaky, path: leadscore.db }\n", "", "a.example", "b.example")
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A Co"})
	fake.AddOrg("b.example", fakeapollo.Org{Name: "B Co"})
	setTooLarge(t, 2)
	res, out, err := in.run(DefaultHooks(), client)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !hasKey(res.Problems, "commit_too_large") {
		t.Fatalf("phase 1 was not too large twice: %v", res.Problems)
	}
	if got := in.state("enrich_count:" + time.Now().UTC().Format(time.DateOnly)); got != "2" {
		t.Errorf("enrich_count = %q after %d calls", got, len(fake.EnrichCalls()))
	}
	if cf := companyFacts(in); cf["a.example"]["enriched_at"] == "" || cf["b.example"]["enriched_at"] == "" {
		t.Errorf("facts bought were not saved: %v", cf)
	}
}

// The reviewer's probe: domains that always fail no longer starve the
// budget. A failure waits a day, and three in a row stop the run.
func TestEnrichFailingDomainsDoNotStarveTheBudget(t *testing.T) {
	in, fake, client := enrichInstall(t, ", max_lookups_per_run: 2", "bad1.example", "bad2.example", "good.example")
	fake.Serve("bad1.example", "server_error")
	fake.Serve("bad2.example", "server_error")
	fake.AddOrg("good.example", fakeapollo.Org{Name: "Good"})
	day := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	res, _, err := in.run(DefaultHooks(), client, at(day))
	if err != nil {
		t.Fatal(err)
	}
	if !hasKey(res.Problems, "enrich_failed") || hasKey(res.Problems, "step_failed:enrich") {
		t.Errorf("problems after two failures: %v", res.Problems)
	}
	cf := companyFacts(in)
	if cf["bad1.example"]["enrich_failed_at"] == "" || cf["bad2.example"]["enrich_failed_at"] == "" {
		t.Errorf("failures not stamped: %v", cf)
	}
	for i := 1; i <= 3; i++ {
		if _, _, err := in.run(DefaultHooks(), client, at(day.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	calls := fake.EnrichCalls()
	if len(calls) != 3 || calls[2] != "good.example" {
		t.Fatalf("calls over four runs = %v, want the two bad ones once, then good.example", calls)
	}
	// A day later the failed domains are tried again, after never-tried ones.
	if _, _, err := in.run(DefaultHooks(), client, at(day.Add(25*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if calls := fake.EnrichCalls(); len(calls) != 5 {
		t.Errorf("a day later: calls %v", calls)
	}
}

func TestEnrichStopsAfterThreeFailuresInARow(t *testing.T) {
	in, fake, client := enrichInstall(t, "", "bad1.example", "bad2.example", "bad3.example", "good.example")
	for _, d := range []string{"bad1.example", "bad2.example", "bad3.example"} {
		fake.Serve(d, "server_error")
	}
	fake.AddOrg("good.example", fakeapollo.Org{Name: "Good"})
	res, _, err := in.run(DefaultHooks(), client)
	if err != nil {
		t.Fatal(err)
	}
	if calls := fake.EnrichCalls(); len(calls) != 3 {
		t.Errorf("calls %v: enrichment did not stop after three failures in a row", calls)
	}
	h := in.health()
	if !hasKey(res.Problems, "enrich_failed") || !strings.Contains(h["problem:enrich_failed"], "in a row") {
		t.Errorf("problems %v, Health %v", res.Problems, h["problem:enrich_failed"])
	}
}

// An unknown funding label clears the stored stage (it moves to previous);
// a label Apollo leaves out keeps it.
func TestEnrichUnknownFundingLabelClearsTheStage(t *testing.T) {
	in, fake, client := enrichInstall(t, ", max_age: 1d", "acme.example")
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme", FundingStage: "Series A"})
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if _, _, err := in.run(DefaultHooks(), client, at(t0)); err != nil {
		t.Fatal(err)
	}
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme"})
	if _, _, err := in.run(DefaultHooks(), client, at(t0.Add(48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if f := factsOf(t, companyFacts(in)["acme.example"], "facts"); f["funding_stage"].Value != "series_a" {
		t.Errorf("a missing label dropped the stage: %v", f)
	}
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme", FundingStage: "Private Equity"})
	if _, _, err := in.run(DefaultHooks(), client, at(t0.Add(96*time.Hour))); err != nil {
		t.Fatal(err)
	}
	row := companyFacts(in)["acme.example"]
	if f, p := factsOf(t, row, "facts"), factsOf(t, row, "previous"); f["funding_stage"].Value != "" || p["funding_stage"].Value != "series_a" {
		t.Errorf("an unknown label kept a stale stage: facts %v previous %v", f, p)
	}
}

// An unreadable stored count is the whole day spent: no call.
func TestEnrichUnreadableCountSpendsTheDay(t *testing.T) {
	in, fake, client := enrichInstall(t, "", "a.example")
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A"})
	day := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	err := in.store().Commit(context.Background(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "enrich_count:2026-10-07", "value": "lots"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.run(DefaultHooks(), client, at(day)); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.EnrichCalls()); n != 0 {
		t.Errorf("%d calls on a day whose count cannot be read", n)
	}
}

// Personal mail providers and names with no dot are never looked up; the
// failure wait and the order hold.
func TestDueDomains(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	m := model.New()
	put := func(d string, enriched, failed time.Time) {
		m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: d, EnrichedAt: enriched, EnrichFailedAt: failed})
	}
	put("gmail.com", time.Time{}, time.Time{})
	put("localhost", time.Time{}, time.Time{})
	put("fresh.example", now.Add(-time.Hour), time.Time{})
	put("stale.example", now.Add(-40*24*time.Hour), time.Time{})
	put("failed-today.example", time.Time{}, now.Add(-time.Hour))
	put("failed-long-ago.example", time.Time{}, now.Add(-48*time.Hour))
	put("new.example", time.Time{}, time.Time{})
	got := dueDomains(m, now, 30*24*time.Hour)
	want := []string{"new.example", "stale.example", "failed-long-ago.example"}
	if !slices.Equal(got, want) {
		t.Errorf("due = %v, want %v", got, want)
	}
}

// Extra keys: company.<name> wins over <name>; an empty value clears.
func TestWriteFactsExtraOrder(t *testing.T) {
	m := model.New()
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "a.example",
		Facts: map[string]model.Fact{"old": {Value: "x", Origin: "input"}}})
	writeFacts(m, "a.example", api.CompanyFacts{Extra: map[string]string{
		"company.size": "big", "size": "small", "old": ""}}, time.Unix(10, 0).UTC())
	cf := m.CompanyFacts[model.Key("a.example")]
	if cf.Facts["size"].Value != "big" {
		t.Errorf("size = %v, want the company. key's value", cf.Facts["size"])
	}
	if _, has := cf.Facts["old"]; has || cf.Previous["old"].Value != "x" {
		t.Errorf("an empty Extra value did not clear: %v / %v", cf.Facts, cf.Previous)
	}
}

// The reviewer's probes: company.<name> wins over <name> with one write, so
// previous holds the stored value, and an empty plain key cannot clear the
// prefixed value.
func TestWriteFactsPrefixedKeyWinsOnce(t *testing.T) {
	m := model.New()
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "a.example",
		Facts: map[string]model.Fact{"size": {Value: "stored", Origin: "input"}}})
	writeFacts(m, "a.example", api.CompanyFacts{Extra: map[string]string{"size": "small", "company.size": "big"}}, time.Unix(10, 0).UTC())
	cf := m.CompanyFacts[model.Key("a.example")]
	if cf.Facts["size"].Value != "big" || cf.Previous["size"].Value != "stored" {
		t.Errorf("facts %v previous %v, want big over stored", cf.Facts["size"], cf.Previous["size"])
	}

	m = model.New()
	writeFacts(m, "b.example", api.CompanyFacts{Extra: map[string]string{"size": "", "company.size": "big"}}, time.Unix(10, 0).UTC())
	if got := m.CompanyFacts[model.Key("b.example")].Facts["size"].Value; got != "big" {
		t.Errorf("an empty plain key cleared the prefixed value: %q", got)
	}
}

// Stop or the deadline during a retry wait ends enrichment quietly: no
// rate-limit line, no failed step, no failure stamp.
func TestEnrichWaitStopIsNotARateLimit(t *testing.T) {
	t.Setenv("APOLLO_API_KEY", testAPIKey)
	fake := fakeapollo.New(testAPIKey)
	fake.SetRetryAfter("30")
	fake.RateLimitNext(100)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	rubric, err := rules.Compile([]byte(testRubric))
	if err != nil {
		t.Fatal(err)
	}
	m := model.New()
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "a.example"})
	push, stop := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, stop)
	var problems []string
	r := &Run{ID: "run-1", Ctx: context.Background(), PushCtx: push, Model: m, Rubric: rubric,
		Now:     func() time.Time { return time.Now().UTC() },
		Problem: func(k, _, _ string, _ bool) { problems = append(problems, k) },
		Config: &config.Config{Enrich: &config.Enrich{Type: "apollo", MaxAge: time.Hour, MaxLookupsPerRun: 5, MaxLookupsPerDay: 5,
			Block: api.Config{"base_url": srv.URL, "_http_client": srv.Client()}}}}
	start := time.Now()
	if err := enrichHook(r); err != nil {
		t.Fatalf("a wait stop failed the step: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the wait did not end at the stop: %v", time.Since(start))
	}
	for _, e := range m.Log {
		if e.Kind == logEnrichRateLimited {
			t.Errorf("a wait stop was logged as a rate limit: %v", e)
		}
	}
	if len(problems) != 0 || !m.CompanyFacts[model.Key("a.example")].EnrichFailedAt.IsZero() {
		t.Errorf("problems %v, failure stamp %v", problems, m.CompanyFacts[model.Key("a.example")].EnrichFailedAt)
	}
}
