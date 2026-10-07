package engine

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
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
