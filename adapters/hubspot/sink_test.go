package hubspot_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// portal starts a fake portal and returns it with a sinks.hubspot block
// pointed at it.
func portal(t *testing.T) (*fakehub.Server, api.Config) {
	t.Helper()
	t.Setenv(hubspot.TokenVariable, fakehub.Token)
	f := fakehub.New()
	srv := f.Serve(t)
	return f, api.Config{"base_url": srv.URL, "_http_client": srv.Client(),
		"pipeline": fakehub.PipelineLabel, "stage": fakehub.StageOpenName}
}

func newSink(t *testing.T, cfg api.Config) *hubspot.Sink {
	t.Helper()
	s, err := hubspot.NewSink(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The sink passes the public conformance suite against the fake portal:
// a step replayed after a crash, or called twice, leaves one contact and
// one deal, and each failure kind maps to its error.
func TestSinkConforms(t *testing.T) {
	f, cfg := portal(t)
	sinktest.Run(t, sinktest.Harness{
		New:    func(api.Config) (api.Sink, error) { return hubspot.NewSink(cfg) },
		Vendor: f,
		Dests:  []string{"contacts", "deals"},
	})
}

func lead(id, domain string, emails ...string) api.LeadRef {
	return api.LeadRef{ID: api.LeadID(id), Emails: emails, FullName: "Ana Van Example", Title: "Head of Ops", Domain: domain,
		Fields: map[string]string{"company.name": "Acme"}}
}

func req(l api.LeadRef, dest, step string, prior map[string]string) api.StepRequest {
	return api.StepRequest{Key: api.StepKey{LeadID: l.ID, LaneID: "warm", Step: step}, Dest: dest, Lead: l, Prior: prior}
}

func TestSteps(t *testing.T) {
	_, cfg := portal(t)
	s := newSink(t, cfg)
	if got := s.Steps("contacts"); len(got) != 1 || got[0] != "contact" {
		t.Errorf("contacts: %v", got)
	}
	if got := s.Steps("deals"); len(got) != 2 || got[0] != "contact" || got[1] != "deal" {
		t.Errorf("deals: %v", got)
	}
	if got := s.Steps("tickets"); got != nil {
		t.Errorf("an unknown destination has steps %v", got)
	}
}

// A new contact carries the lead id and the context properties; empty
// values are not sent.
func TestContactCreateCarriesProperties(t *testing.T) {
	f, cfg := portal(t)
	l := lead("lead-1", "acme.example", "ANA@acme.example")
	l.Verdict = &api.Verdict{Values: map[string]any{"tier": float64(1), "priority": "A"}, AccountScore: 30, ContactScore: 12.5,
		Reasons: []string{"title head", "fit"}}
	id, err := newSink(t, cfg).Do(context.Background(), req(l, "contacts", "contact", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"email": "ana@acme.example", "firstname": "Ana", "lastname": "Van Example", "jobtitle": "Head of Ops",
		"company": "Acme", "leadscore_lead_id": "lead-1", "leadscore_lane": "warm", "leadscore_tier": "1",
		"leadscore_priority": "A", "leadscore_score": "42.5", "leadscore_reasons": "title head; fit"}
	for k, v := range want {
		if got := f.Prop("contacts", id, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// A contact already holding one of the lead's emails is reused untouched.
func TestContactFoundByEmailIsReusedUntouched(t *testing.T) {
	f, cfg := portal(t)
	existing := f.AddContact("ana.old@acme.example", map[string]string{"jobtitle": "set by sales"})
	l := lead("lead-1", "acme.example", "ana@acme.example", "Ana.Old@acme.example")
	id, err := newSink(t, cfg).Do(context.Background(), req(l, "contacts", "contact", nil))
	if err != nil || id != existing {
		t.Fatalf("id %q err %v, want the existing %s", id, err, existing)
	}
	if f.Count("contact") != 0 || f.Prop("contacts", id, "jobtitle") != "set by sales" || f.Prop("contacts", id, "leadscore_lead_id") != "" {
		t.Error("the existing contact was changed or a new one created")
	}
}

// Search lags: neither the lead id nor the email search shows a contact
// just created, so the create answers 409; its existing id counts as found.
func TestContactConflictCountsAsFound(t *testing.T) {
	f, cfg := portal(t)
	f.SetLag(true)
	existing := f.AddContact("ana@acme.example", nil)
	id, err := newSink(t, cfg).Do(context.Background(), req(lead("lead-1", "acme.example", "ana@acme.example"), "contacts", "contact", nil))
	if err != nil || id != existing || f.Count("contact") != 0 {
		t.Fatalf("id %q err %v (existing %s), %d created", id, err, existing, f.Count("contact"))
	}
}

// A crash, then an email correction: the contact is found by the lead id,
// not created again under the new email.
func TestContactFoundByLeadIDAfterAnEmailCorrection(t *testing.T) {
	f, cfg := portal(t)
	s := newSink(t, cfg)
	first, err := s.Do(context.Background(), req(lead("lead-1", "acme.example", "ana@acme.exmaple"), "contacts", "contact", nil))
	if err != nil {
		t.Fatal(err)
	}
	again, err := newSink(t, cfg).Do(context.Background(), req(lead("lead-1", "acme.example", "ana@acme.example"), "contacts", "contact", nil))
	if err != nil || again != first || f.Count("contact") != 1 {
		t.Errorf("after the correction: %q (first %q), %v, %d contacts", again, first, err, f.Count("contact"))
	}
}

// Error mapping (contracts section 6): an invalid email is a refusal; a
// 401 or 403 stops the sink for the run with no attempt counted (reported as
// a rate limit: no lead is at fault); vendor messages never reach the error
// text.
func TestContactErrors(t *testing.T) {
	_, cfg := portal(t)
	_, err := newSink(t, cfg).Do(context.Background(), req(lead("lead-1", "acme.example", "not-an-email"), "contacts", "contact", nil))
	if !errors.Is(err, api.ErrRefused) || strings.Contains(err.Error(), "not-an-email") {
		t.Errorf("invalid email: %v", err)
	}
	_, err = newSink(t, cfg).Do(context.Background(), req(lead("lead-2", "acme.example"), "contacts", "contact", nil))
	if !errors.Is(err, api.ErrRefused) {
		t.Errorf("no email: %v", err)
	}
	t.Setenv(hubspot.TokenVariable, "wrong")
	_, err = newSink(t, cfg).Do(context.Background(), req(lead("lead-3", "acme.example", "a@acme.example"), "contacts", "contact", nil))
	if !errors.Is(err, api.ErrRateLimited) || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "correlationId=") {
		t.Errorf("401: %v", err)
	}
}

func contactFor(t *testing.T, s *hubspot.Sink, l api.LeadRef) string {
	t.Helper()
	id, err := s.Do(context.Background(), req(l, "deals", "contact", nil))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The deal is created with its name, pipeline, stage, domain and the
// contact association in one call.
func TestDealCreatedWithDomainAndAssociation(t *testing.T) {
	f, cfg := portal(t)
	s := newSink(t, cfg)
	l := lead("lead-1", "Acme.example", "ana@acme.example")
	c := contactFor(t, s, l)
	id, err := s.Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": c}))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"dealname": "acme.example", "pipeline": fakehub.PipelineID, "dealstage": fakehub.StageOpen,
		"leadscore_company_domain": "acme.example", "leadscore_lane": "warm"} {
		if got := f.Prop("deals", id, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := f.Associated("deals", id, "contacts"); len(got) != 1 || got[0] != c {
		t.Errorf("associated %v", got)
	}
	creates := 0
	for _, r := range f.Requests() {
		if r == "POST /crm/v3/objects/deals" {
			creates++
		}
		if strings.HasPrefix(r, "PUT ") {
			t.Errorf("a separate association call for a new deal: %s", r)
		}
	}
	if creates != 1 {
		t.Errorf("%d creates", creates)
	}
}

// Two leads at one company: the second deal step gets the first's deal in
// Related and is associated with it.
func TestSecondLeadReusesTheDealFromRelated(t *testing.T) {
	f, cfg := portal(t)
	f.SetLag(true) // the first deal is not searchable yet: Related alone finds it
	s := newSink(t, cfg)
	ana, ben := lead("lead-a", "acme.example", "ana@acme.example"), lead("lead-b", "acme.example", "ben@acme.example")
	d1, err := s.Do(context.Background(), req(ana, "deals", "deal", map[string]string{"contact": contactFor(t, s, ana)}))
	if err != nil {
		t.Fatal(err)
	}
	cb := contactFor(t, s, ben)
	r := req(ben, "deals", "deal", map[string]string{"contact": cb})
	r.Related = []api.LedgerRef{{Key: api.StepKey{LeadID: ana.ID, LaneID: "warm", Step: "deal"}, Dest: "deals", VendorID: d1, State: "done"}}
	d2, err := newSink(t, cfg).Do(context.Background(), r)
	if err != nil || d2 != d1 || f.Count("deal") != 1 {
		t.Fatalf("second deal %q (first %q), %v, %d deals", d2, d1, err, f.Count("deal"))
	}
	if got := f.Associated("deals", d1, "contacts"); len(got) != 2 {
		t.Errorf("the deal's contacts %v", got)
	}
}

// A deal step whose call created the deal and then timed out is retried
// with nothing in Related and no stored deal; search has not indexed the
// deal. The retry finds it through the contact's associations: one deal.
func TestDealRetryAfterATimeoutReusesTheDeal(t *testing.T) {
	f, cfg := portal(t)
	f.SetLag(true)
	s := newSink(t, cfg)
	l := lead("lead-1", "acme.example", "ana@acme.example")
	r := req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)})
	f.FailAfter("deal", sinktest.Transient)
	if _, err := s.Do(context.Background(), r); !errors.Is(err, api.ErrTransient) {
		t.Fatalf("setup: %v", err)
	}
	id, err := newSink(t, cfg).Do(context.Background(), r)
	if err != nil || f.Count("deal") != 1 || id != f.IDs("deals")[0] {
		t.Errorf("retry: %q %v, %d deals", id, err, f.Count("deal"))
	}
}

// Another lead's deal step after a timed-out one reuses the company's deal
// found by its domain once search shows it, or by CompanyDealID.
func TestDealFoundByDomainOrCompanyDealID(t *testing.T) {
	f, cfg := portal(t)
	byDomain := f.AddDeal(fakehub.StageLater, map[string]string{"leadscore_company_domain": "acme.example"})
	s := newSink(t, cfg)
	ben := lead("lead-b", "acme.example", "ben@acme.example")
	id, err := s.Do(context.Background(), req(ben, "deals", "deal", map[string]string{"contact": contactFor(t, s, ben)}))
	if err != nil || id != byDomain {
		t.Errorf("by domain: %q %v, want %s", id, err, byDomain)
	}
	stored := f.AddDeal(fakehub.StageOpen, nil) // a salesperson's deal, found by a lookup
	cara := lead("lead-c", "beta.example", "cara@beta.example")
	cara.CompanyDealID = stored
	id, err = s.Do(context.Background(), req(cara, "deals", "deal", map[string]string{"contact": contactFor(t, s, cara)}))
	if err != nil || id != stored || f.Count("deal") != 0 {
		t.Errorf("by CompanyDealID: %q %v, want %s; %d created", id, err, stored, f.Count("deal"))
	}
}

// Only open deals are reused: a won or lost deal, whether in Related, the
// stored deal, or found by domain, is skipped and a new deal opened.
func TestClosedDealsAreNeverReused(t *testing.T) {
	for _, stage := range []string{fakehub.StageWon, fakehub.StageLost} {
		t.Run(stage, func(t *testing.T) {
			f, cfg := portal(t)
			closed := f.AddDeal(stage, map[string]string{"leadscore_company_domain": "acme.example"})
			s := newSink(t, cfg)
			l := lead("lead-1", "acme.example", "ana@acme.example")
			l.CompanyDealID = closed
			r := req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)})
			r.Related = []api.LedgerRef{{Key: api.StepKey{LeadID: "x", LaneID: "warm", Step: "deal"}, Dest: "deals", VendorID: closed, State: "done"}}
			id, err := s.Do(context.Background(), r)
			if err != nil || id == closed || f.Count("deal") != 1 {
				t.Errorf("got %q %v; the closed deal is %s; %d created", id, err, closed, f.Count("deal"))
			}
		})
	}
}

// A read that fails stops the deal step before any create: a deal is
// never opened on an incomplete answer.
func TestDealReadFailureCreatesNothing(t *testing.T) {
	for _, call := range []string{"POST /crm/v3/objects/deals/search", "POST /crm/v4/associations/contacts/deals", "POST /crm/v3/objects/deals/batch/read"} {
		t.Run(call, func(t *testing.T) {
			f, cfg := portal(t)
			f.AddDeal(fakehub.StageLost, map[string]string{"leadscore_company_domain": "acme.example"}) // so the batch read runs
			s := newSink(t, cfg)
			l := lead("lead-1", "acme.example", "ana@acme.example")
			r := req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)})
			f.FailNext(call, "errors/server_error", 1)
			if _, err := s.Do(context.Background(), r); !errors.Is(err, api.ErrTransient) || f.Count("deal") != 0 {
				t.Errorf("err %v, %d deals", err, f.Count("deal"))
			}
		})
	}
}

// A pipeline or stage name the portal does not have makes the deal step
// wait (ErrTransient), and creates nothing.
func TestDealWaitsOnAnUnknownStage(t *testing.T) {
	f, cfg := portal(t)
	cfg["stage"] = "No such stage"
	s := newSink(t, cfg)
	l := lead("lead-1", "acme.example", "ana@acme.example")
	_, err := s.Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)}))
	if !errors.Is(err, api.ErrTransient) || f.Count("deal") != 0 {
		t.Errorf("err %v", err)
	}
	cfg["stage"] = "Closed won"
	_, err = newSink(t, cfg).Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": "1"}))
	if !errors.Is(err, api.ErrTransient) || !strings.Contains(err.Error(), "closed stage") {
		t.Errorf("a closed stage for new deals: %v", err)
	}
}

func TestConfigRefusals(t *testing.T) {
	_, cfg := portal(t)
	noClient := api.Config{"base_url": cfg["base_url"]}
	if _, err := hubspot.NewSink(noClient); err == nil || !strings.Contains(err.Error(), "tests only") {
		t.Errorf("base_url without a test client: %v", err)
	}
	if _, err := hubspot.NewSink(api.Config{"pipeline": "Sales Pipeline"}); err == nil {
		t.Error("a pipeline without a stage was accepted")
	}
	if _, err := hubspot.NewSink(api.Config{"property_prefix": "Bad-Prefix"}); err == nil {
		t.Error("a bad prefix was accepted")
	}
	t.Setenv(hubspot.TokenVariable, "")
	if _, err := hubspot.NewLookup(api.Config{}); err == nil || !strings.Contains(err.Error(), "HUBSPOT_TOKEN") {
		t.Errorf("no token: %v", err)
	}
}

// A candidate deal at a stage no pipeline lists may be the company's open
// deal (the lookup holds the company for it): the deal step waits rather
// than open a second deal.
func TestDealWaitsOnAnUnknownStageCandidate(t *testing.T) {
	for _, where := range []string{"related", "stored", "domain"} {
		t.Run(where, func(t *testing.T) {
			f, cfg := portal(t)
			ghost := f.AddDeal(fakehub.StageOpen, map[string]string{"leadscore_company_domain": "acme.example"})
			f.SetProp("deals", ghost, "pipeline", "otherpipe")
			f.SetProp("deals", ghost, "dealstage", "ghoststage")
			s := newSink(t, cfg)
			l := lead("lead-1", "acme.example", "ana@acme.example")
			r := req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)})
			switch where {
			case "related":
				r.Related = []api.LedgerRef{{Key: api.StepKey{LeadID: "x", LaneID: "warm", Step: "deal"}, Dest: "deals", VendorID: ghost, State: "done"}}
			case "stored":
				r.Lead.CompanyDealID = ghost
			}
			_, err := s.Do(context.Background(), r)
			if !errors.Is(err, api.ErrTransient) || !strings.Contains(err.Error(), "at a stage no pipeline lists") || f.Count("deal") != 0 {
				t.Errorf("err %v, %d deals created", err, f.Count("deal"))
			}
		})
	}
}

// The tiers go in order: a known open deal is reused without reading the
// contact's deals or searching.
func TestDealKnownTierFirst(t *testing.T) {
	f, cfg := portal(t)
	d := f.AddDeal(fakehub.StageOpen, nil)
	s := newSink(t, cfg)
	l := lead("lead-1", "acme.example", "ana@acme.example")
	l.CompanyDealID = d
	r := req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)})
	before := len(f.Requests())
	if id, err := s.Do(context.Background(), r); err != nil || id != d {
		t.Fatalf("%q %v", id, err)
	}
	for _, q := range f.Requests()[before:] {
		if strings.Contains(q, "/associations/contacts/deals") || strings.HasSuffix(q, "/deals/search") {
			t.Errorf("read past the known tier: %s", q)
		}
	}
}

// A deal linked to the lead's contact but carrying another company's domain
// (the person's old employer) is never reused for this company.
func TestDealOnTheContactForAnotherCompanyIsNotReused(t *testing.T) {
	f, cfg := portal(t)
	s := newSink(t, cfg)
	l := lead("lead-1", "acme.example", "ana@acme.example")
	c := contactFor(t, s, l)
	old := f.AddDeal(fakehub.StageOpen, map[string]string{"leadscore_company_domain": "oldjob.example"})
	f.Associate("contacts", c, "deals", old)
	id, err := s.Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": c}))
	if err != nil || id == old || f.Count("deal") != 1 {
		t.Errorf("got %q %v; the old employer's deal is %s", id, err, old)
	}
}

// The review probe: a salesperson's deal on the contact, with no domain
// property and no company link, is reused, not doubled.
func TestDealReusesAHandMadeDealOnTheContact(t *testing.T) {
	f, cfg := portal(t)
	s := newSink(t, cfg)
	l := lead("lead-1", "acme.example", "ana@acme.example")
	c := contactFor(t, s, l)
	hand := f.AddDeal(fakehub.StageOpen, nil)
	f.Associate("contacts", c, "deals", hand)
	id, err := s.Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": c}))
	if err != nil || id != hand || f.Count("deal") != 0 {
		t.Errorf("got %q %v, %d created; want the hand-made %s", id, err, f.Count("deal"), hand)
	}
}

// The lead's domain is normalized before it names, marks and finds the deal.
func TestDealNormalizesTheDomain(t *testing.T) {
	f, cfg := portal(t)
	s := newSink(t, cfg)
	l := lead("lead-1", "WWW.Acme.example", "ana@acme.example")
	id, err := s.Do(context.Background(), req(l, "deals", "deal", map[string]string{"contact": contactFor(t, s, l)}))
	if err != nil {
		t.Fatal(err)
	}
	if f.Prop("deals", id, "leadscore_company_domain") != "acme.example" || f.Prop("deals", id, "dealname") != "acme.example" {
		t.Errorf("deal props %q %q", f.Prop("deals", id, "leadscore_company_domain"), f.Prop("deals", id, "dealname"))
	}
	other := lead("lead-2", "acme.example", "bo@acme.example")
	again, err := newSink(t, cfg).Do(context.Background(), req(other, "deals", "deal", map[string]string{"contact": contactFor(t, s, other)}))
	if err != nil || again != id {
		t.Errorf("found by domain: %q %v, want %s", again, err, id)
	}
}
