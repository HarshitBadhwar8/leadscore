// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hubspot_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
)

func lookup(t *testing.T, cfg api.Config, leads ...api.LeadRef) ([]api.Event, map[api.LeadID]error, error) {
	t.Helper()
	l, err := hubspot.NewLookup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l.Lookup(context.Background(), leads)
}

// evText lists events as "kind domain-or-email deal_id", sorted.
func evText(evs []api.Event) []string {
	var out []string
	for _, e := range evs {
		who := e.Email
		if who == "" {
			who = e.Domain
		}
		if who == "" {
			who = e.LinkedInURL
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s", e.Kind, who, e.Attrs["deal_id"])))
	}
	sort.Strings(out)
	return out
}

func wantEvents(t *testing.T, evs []api.Event, want ...string) {
	t.Helper()
	got := evText(evs)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events\n got %q\nwant %q", got, want)
	}
}

// Every email of the lead's family is read: an opt-out on any of them is
// reported, under that email.
func TestLookupOptOutOnEveryEmail(t *testing.T) {
	f, cfg := portal(t)
	f.AddContact("ana@acme.example", map[string]string{"hs_email_optout": "false"})
	f.AddContact("ana.private@mail.example", map[string]string{"hs_email_optout": "true"})
	l := lead("lead-1", "", "ana@acme.example", "Ana.Private@mail.example")
	evs, failed, err := lookup(t, cfg, l)
	if err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	wantEvents(t, evs, "optout ana.private@mail.example")
}

// The lead's own contact, found by the lead id, counts even after a
// salesperson changed its address: the opt-out goes to the lead's email.
func TestLookupOptOutOnTheLeadsOwnContact(t *testing.T) {
	f, cfg := portal(t)
	f.AddContact("changed@other.example", map[string]string{"leadscore_lead_id": "lead-1", "hs_email_optout": "true"})
	evs, _, err := lookup(t, cfg, lead("lead-1", "", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "optout ana@acme.example")
}

// A company's deals are found through its contacts' companies, through the
// company record found by domain (when no contact leads to one), through
// the deal's domain property, and as the stored deal; one event per company
// for the strongest stage.
func TestLookupDealsPerCompany(t *testing.T) {
	f, cfg := portal(t)

	// acme: a salesperson's open deal on the company Ana's contact belongs to.
	ana := f.AddContact("ana@acme.example", nil)
	acme := f.AddCompany("acme.example")
	f.Associate("contacts", ana, "companies", acme)
	open := f.AddDeal(fakehub.StageLater, nil)
	f.Associate("companies", acme, "deals", open)

	// beta: no contact; the company found by domain has a won deal and a
	// lost one.
	beta := f.AddCompany("beta.example")
	won := f.AddDeal(fakehub.StageWon, nil)
	lost := f.AddDeal(fakehub.StageLost, nil)
	f.Associate("companies", beta, "deals", won)
	f.Associate("companies", beta, "deals", lost)

	// gamma: only a deal the sink opened, found by its domain property.
	ours := f.AddDeal(fakehub.StageOpen, map[string]string{"leadscore_company_domain": "gamma.example"})

	// delta: the stored deal was deleted. epsilon: the stored deal is lost.
	// zeta: nothing at all.
	gone := f.AddDeal(fakehub.StageOpen, nil)
	f.Delete("deals", gone)
	lostStored := f.AddDeal(fakehub.StageLost, nil)

	d := lead("d", "delta.example", "d@delta.example")
	d.CompanyDealID = gone
	e := lead("e", "epsilon.example", "e@epsilon.example")
	e.CompanyDealID = lostStored
	evs, failed, err := lookup(t, cfg,
		lead("a", "acme.example", "ana@acme.example"), lead("b", "beta.example", "b@beta.example"),
		lead("g", "gamma.example", "g@gamma.example"), d, e, lead("z", "zeta.example", "z@zeta.example"))
	if err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	wantEvents(t, evs,
		"deal_open acme.example "+open,
		"deal_won beta.example "+won,
		"deal_open gamma.example "+ours,
		"deal_lost delta.example "+gone,
		"deal_lost epsilon.example "+lostStored,
		"deal_lost zeta.example",
	)
	for _, ev := range evs {
		if ev.Kind == "deal_lost" && ev.Domain == "zeta.example" {
			if _, has := ev.Attrs["deal_id"]; has {
				t.Error("a company with no deal named a deal id")
			}
		}
	}
}

// A deal associated with the lead's own contact (the sink's create makes
// that association in the same call) is found without search, so a deal
// search still lagging does not read as "no deal".
func TestLookupFindsAFreshDealThroughTheContact(t *testing.T) {
	f, cfg := portal(t)
	f.SetLag(true)
	c := f.AddContact("ana@acme.example", map[string]string{"leadscore_lead_id": "a"})
	d := f.AddDeal(fakehub.StageOpen, map[string]string{"leadscore_company_domain": "acme.example"})
	f.Associate("deals", d, "contacts", c)
	evs, _, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "deal_open acme.example "+d)
}

// Any read that fails fails every lead it was for (or the whole lookup,
// when every lead shared it), and a company it touched gets no deal event at
// all: never deal_lost on an incomplete answer.
func TestLookupFailedReadsReleaseNothing(t *testing.T) {
	for _, call := range []string{
		"POST /crm/v3/objects/deals/search",
		"POST /crm/v3/objects/companies/search",
		"POST /crm/v3/objects/deals/batch/read",
		"POST /crm/v4/associations/contacts/companies",
		"POST /crm/v4/associations/contacts/deals",
		"POST /crm/v4/associations/companies/deals",
		"POST /crm/v3/objects/contacts/batch/read",
		"POST /crm/v3/objects/contacts/search",
		"GET /crm/v3/pipelines/deals",
	} {
		t.Run(call, func(t *testing.T) {
			f, cfg := portal(t)
			c := f.AddContact("ana@acme.example", nil)
			co := f.AddCompany("acme.example")
			f.Associate("contacts", c, "companies", co)
			lost := f.AddDeal(fakehub.StageLost, nil)
			f.Associate("companies", co, "deals", lost)
			f.AddCompany("beta.example")
			ana := lead("a", "acme.example", "ana@acme.example")
			ana.CompanyDealID = lost
			// The beta lead has no contact, so only a call every company
			// shares fails it too.
			f.FailNext(call, "errors/server_error", 1)
			evs, failed, err := lookup(t, cfg, ana, lead("b", "beta.example", "bo@beta.example"))
			if err != nil {
				// A read every lead shared failed: the whole lookup failed.
				if evs != nil || failed != nil {
					t.Errorf("a failed lookup returned events %v and failures %v", evText(evs), failed)
				}
				return
			}
			if failed["a"] == nil && failed["b"] == nil {
				t.Fatalf("no lead failed; events %v", evText(evs))
			}
			for _, e := range evs {
				if strings.HasPrefix(e.Kind, "deal_") && failed[map[string]api.LeadID{"acme.example": "a", "beta.example": "b"}[e.Domain]] != nil {
					t.Errorf("a %s for %s, whose lead failed", e.Kind, e.Domain)
				}
			}
		})
	}
}

// A search with more pages than one read follows is partial, and counts as
// failed.
func TestLookupPartialSearchFails(t *testing.T) {
	f, cfg := portal(t)
	f.SetPageSize(1)
	for i := 0; i < 30; i++ {
		f.AddDeal(fakehub.StageLost, map[string]string{"leadscore_company_domain": "acme.example"})
	}
	evs, failed, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"), lead("b", "beta.example", "bo@beta.example"))
	if err == nil && (failed["a"] == nil || failed["b"] == nil) {
		t.Errorf("a partial deal search was read as complete: failed %v, events %v", failed, evText(evs))
	}
	for _, e := range evs {
		if strings.HasPrefix(e.Kind, "deal_") {
			t.Errorf("event %s %s on a partial read", e.Kind, e.Domain)
		}
	}
}

// When every lead failed, the whole lookup fails.
func TestLookupWholeFailure(t *testing.T) {
	_, cfg := portal(t)
	t.Setenv(hubspot.TokenVariable, "wrong")
	evs, failed, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err == nil || len(evs) != 0 || len(failed) != 0 {
		t.Errorf("events %v failed %v err %v", evs, failed, err)
	}
}

// When every lead failed but an opt-out was read, the opt-out still comes
// back (with every lead failed), so it is never lost to a deal read failing.
func TestLookupKeepsOptOutsWhenEveryLeadFailed(t *testing.T) {
	f, cfg := portal(t)
	f.AddContact("ana@acme.example", map[string]string{"hs_email_optout": "true"})
	f.FailNext("POST /crm/v3/objects/deals/search", "errors/server_error", 1)
	evs, failed, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err != nil || failed["a"] == nil {
		t.Fatalf("failed %v err %v", failed, err)
	}
	wantEvents(t, evs, "optout ana@acme.example")
}

// A deal linked to a contact at the company but belonging to another company
// (the person's old employer, no matching domain) is not this company's:
// acme has no deal.
func TestLookupContactDealOfAnotherCompany(t *testing.T) {
	f, cfg := portal(t)
	c := f.AddContact("ana@acme.example", nil)
	oldJob := f.AddCompany("oldjob.example")
	old := f.AddDeal(fakehub.StageOpen, nil)
	f.Associate("companies", oldJob, "deals", old)
	f.Associate("contacts", c, "deals", old)
	evs, _, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "deal_lost acme.example")
}

func doneContact(l api.LeadRef, contactID string) api.LeadRef {
	l.Done = []api.LedgerRef{{Key: api.StepKey{LeadID: l.ID, LaneID: "warm", Step: "contact"}, Dest: "contacts", VendorID: contactID, State: "done"}}
	return l
}

// The contact the ledger holds for a lead was merged in HubSpot into
// another, which opted out: the opt-out counts for the lead, though neither
// the lead's email nor its lead id finds the survivor.
func TestLookupOptOutOnAMergedStoredContact(t *testing.T) {
	f, cfg := portal(t)
	ours := f.AddContact("ana@acme.example", nil)
	survivor := f.AddContact("a.example@other.example", map[string]string{"hs_email_optout": "true"})
	f.Merge(ours, survivor)
	evs, failed, err := lookup(t, cfg, doneContact(lead("a", "", "ana@acme.example"), ours))
	if err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	wantEvents(t, evs, "optout ana@acme.example")

	// A stored contact deleted since is skipped, not a failure.
	gone := f.AddContact("bo@beta.example", nil)
	f.Delete("contacts", gone)
	if _, failed, err := lookup(t, cfg, doneContact(lead("b", "", "bo@beta.example"), gone)); err != nil || len(failed) != 0 {
		t.Errorf("a deleted stored contact: %v %v", failed, err)
	}
}

// A read by email that matches a contact's secondary address answers under
// its primary one. The batch's unmatched emails are read one by one, so the
// opt-out lands on the right email and no lead fails.
func TestLookupSecondaryEmailMatch(t *testing.T) {
	f, cfg := portal(t)
	f.AddContact("primary@other.example", map[string]string{"hs_additional_emails": "ana@acme.example", "hs_email_optout": "true"})
	f.AddContact("bo@beta.example", nil)
	evs, failed, err := lookup(t, cfg, lead("a", "", "ana@acme.example"), lead("b", "", "bo@beta.example"))
	if err != nil || len(failed) != 0 {
		t.Fatalf("failed %v err %v", failed, err)
	}
	wantEvents(t, evs, "optout ana@acme.example")
}

// The review probe: a salesperson's deal on the contact, with no domain
// property and no company link, is the company's deal: held, not released.
// One linked to a company record of another domain is not.
func TestLookupHandMadeContactDealCounts(t *testing.T) {
	f, cfg := portal(t)
	c := f.AddContact("ana@acme.example", nil)
	hand := f.AddDeal(fakehub.StageOpen, nil)
	f.Associate("contacts", c, "deals", hand)
	evs, _, err := lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "deal_open acme.example "+hand)

	f.Associate("deals", hand, "companies", f.AddCompany("oldjob.example"))
	evs, _, err = lookup(t, cfg, lead("a", "acme.example", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "deal_lost acme.example")
}

// Domains compare normalized on both sides, as merge normalizes a lead's:
// case, a scheme, www. and a path do not make a deal another company's.
func TestLookupNormalizesDomains(t *testing.T) {
	f, cfg := portal(t)
	c := f.AddContact("ana@acme.example", nil)
	d := f.AddDeal(fakehub.StageOpen, map[string]string{"leadscore_company_domain": "https://www.Acme.example/"})
	f.Associate("contacts", c, "deals", d)
	evs, _, err := lookup(t, cfg, lead("a", "WWW.ACME.example", "ana@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, evs, "deal_open acme.example "+d)
}
