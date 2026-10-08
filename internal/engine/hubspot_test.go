// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// realHubSpot points the world's hubspot sink and lookup at the real
// HubSpot adapter, talking to a fake portal. It returns the portal and the
// run option that hands the run the portal's HTTP client.
func realHubSpot(w *world) (*fakehub.Server, func(*api.RunOptions, *settings)) {
	w.t.Helper()
	w.t.Setenv("HUBSPOT_TOKEN", fakehub.Token)
	f := fakehub.New()
	srv := f.Serve(w.t)
	w.config("hubspot: {}", fmt.Sprintf("hubspot: { base_url: %q, pipeline: %q, stage: %q }", srv.URL, fakehub.PipelineLabel, fakehub.StageOpenName))
	oldSink, oldLookup := sinkFactory, lookupFactory
	sinkFactory = func(typ string) (func(api.Config) (api.Sink, error), bool) {
		if typ == "hubspot" {
			return api.SinkFactory(typ)
		}
		return oldSink(typ)
	}
	lookupFactory = func(typ string) (func(api.Config) (api.Lookup, error), bool) {
		if typ == "hubspot" {
			return api.LookupFactory(typ)
		}
		return oldLookup(typ)
	}
	w.t.Cleanup(func() { sinkFactory, lookupFactory = oldSink, oldLookup })
	return f, func(_ *api.RunOptions, s *settings) { s.client = srv.Client() }
}

func noHubSpotProblems(t *testing.T, res api.RunResult) {
	t.Helper()
	for _, p := range res.Problems {
		if strings.HasPrefix(p, "hubspot:") || strings.HasPrefix(p, "lookup_failed") || strings.HasPrefix(p, "sink_failed") {
			t.Errorf("problem %s", p)
		}
	}
}

// The HubSpot deals proof: two leads at one company pushed to a deals lane in
// one batch open one deal. The second lead's deal step gets the first one's
// deal in Related and is associated with it; the deal carries the company's
// domain.
func TestHubSpotTwoLeadsOneCompanyOneDeal(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Clerk,acme.example")
	w.pushesOff()
	f, client := realHubSpot(w)
	w.reply("ana@acme.example", "replied_positive")
	w.reply("ben@acme.example", "replied_positive")
	res, out := w.mustRun(client)
	noHubSpotProblems(t, res)
	deals := f.IDs("deals")
	if len(deals) != 1 || f.Count("deal") != 1 {
		t.Fatalf("deals %v, want one\n%s", deals, out)
	}
	a, b := w.push("ana@acme.example", "warm", "deal"), w.push("ben@acme.example", "warm", "deal")
	if a["state"] != stateDone || b["state"] != stateDone || a["vendor_id"] != deals[0] || b["vendor_id"] != deals[0] {
		t.Errorf("rows %v / %v", a, b)
	}
	if got := f.Associated("deals", deals[0], "contacts"); len(got) != 2 {
		t.Errorf("the deal's contacts %v, want both leads", got)
	}
	if d := f.Prop("deals", deals[0], "leadscore_company_domain"); d != "acme.example" {
		t.Errorf("the deal's domain %q", d)
	}
	if n := f.Prop("deals", deals[0], "dealname"); n != "acme.example" {
		t.Errorf("the deal is named %q, want the domain", n)
	}
}

// The HubSpot deals proof: a deal a salesperson closed as lost releases its
// company. Ana's deal holds Acme, so Ben stays out of the cold lane; once
// HubSpot shows the deal closed-lost, the lookup (reading the stored deal by
// id, no search involved) reports deal_lost, Ben is released and pushed; a
// later positive reply at Acme opens a new deal rather than reusing the lost
// one.
func TestHubSpotClosedLostReleasesItsCompany(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.pushesOff()
	f, client := realHubSpot(w)
	w.reply("ana@acme.example", "replied_positive")
	w.mustRun(client)
	deal := w.push("ana@acme.example", "warm", "deal")["vendor_id"]
	if deal == "" || len(w.apollo.Calls()) != 0 {
		t.Fatalf("setup: deal %q, apollo %v", deal, calls(w.apollo))
	}
	res, _ := w.mustRun(client)
	noHubSpotProblems(t, res)
	if s := w.outcome("ben@acme.example")["status"]; s != statusDeal || len(w.apollo.Calls()) != 0 {
		t.Fatalf("Ben is %q while Acme has an open deal; apollo %v", s, calls(w.apollo))
	}

	f.SetStage(deal, fakehub.StageLost)
	w.mustRun(client)
	if o := w.outcome("ben@acme.example"); o["status"] != statusNew || o["deal_stage"] != "lost" || o["deal_id"] != deal {
		t.Errorf("after closed-lost, Ben's outcome %v", o)
	}
	w.mustRun(client)
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ben@acme.example"}) {
		t.Errorf("Ben after the release: %v", got)
	}

	w.reply("ben@acme.example", "replied_positive")
	w.mustRun(client)
	ids := f.IDs("deals")
	if len(ids) != 2 || w.push("ben@acme.example", "warm", "deal")["vendor_id"] != ids[1] {
		t.Errorf("deals %v, Ben's deal row %v: want a new deal, not the lost one", ids, w.push("ben@acme.example", "warm", "deal"))
	}
}

// A deal step whose call timed out after HubSpot created the deal gets no
// deal id, and its retry gets nothing in Related. The retry finds the deal
// through the contact's associations (HubSpot's search has not indexed it
// yet) and reuses it: one deal.
func TestHubSpotTimedOutDealStepRetryReusesTheDeal(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Clerk,acme.example")
	w.rubric("{ field: status, eq: replied_positive }", "{ field: status, in: [replied_positive, deal] }")
	w.pushesOff()
	f, client := realHubSpot(w)
	w.reply("ana@acme.example", "replied_positive")
	f.SetLag(true)
	f.FailAfter("deal", sinktest.Transient)
	w.mustRun(client)
	if r := w.push("ana@acme.example", "warm", "deal"); r["state"] != statePending || r["called_at"] == "" || r["vendor_id"] != "" || f.Count("deal") != 1 {
		t.Fatalf("setup: row %v, %d deals", r, f.Count("deal"))
	}
	w.mustRun(client)
	ids := f.IDs("deals")
	if r := w.push("ana@acme.example", "warm", "deal"); len(ids) != 1 || r["state"] != stateDone || r["vendor_id"] != ids[0] {
		t.Errorf("deals %v, row %v", ids, r)
	}
}

// A deals-by-company lookup that finds no deal soon after a deal step there
// was called with no deal id back is not trusted: HubSpot's search may not
// show that deal yet. The company stays held and its leads wait, logged;
// once the window has passed, the same answer releases the company.
func TestNoDealAnswerSoonAfterADealCallIsNotTrusted(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	start := time.Now()
	w.clock = func() time.Time { return start }
	w.hubspot.Fail("deal", sinktest.Transient) // called; whether HubSpot made the deal is unknown
	w.mustRun()
	if r := w.push("ana@acme.example", "warm", "deal"); r["called_at"] == "" || r["vendor_id"] != "" {
		t.Fatalf("setup: %v", r)
	}
	lk := w.lookup("hubspot")
	lk.DealsByCompany()

	w.clock = func() time.Time { return start.Add(5 * time.Minute) }
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusDeal {
		t.Errorf("five minutes after the deal call, a no-deal answer released Acme: Ben is %q", s)
	}
	if o := w.outcome("ana@acme.example"); o["deal_checked_at"] != "" {
		t.Errorf("the untrusted answer was applied: %v", o)
	}
	if len(w.apollo.Calls()) != 0 {
		t.Errorf("Ben was pushed: %v", calls(w.apollo))
	}
	logged := false
	for _, l := range w.rows(model.TableLog) {
		logged = logged || l["kind"] == logLookupFailed && strings.Contains(l["message"], "may not show in search yet")
	}
	if !logged {
		t.Error("no lookup_failed Log line for the held company")
	}

	// Run 2 cancelled Ana's deal step (she is no longer replied_positive),
	// which wrote updated_at: the window runs from that write, the safe side.
	w.clock = func() time.Time { return start.Add(5*time.Minute + dealSearchLag + time.Minute) }
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusNew {
		t.Errorf("past the window the no-deal answer should release Acme: Ben is %q", s)
	}
}

// The window counts from the deal step's latest call: a step first called
// at T and retried at T+10m keeps a no-deal answer at T+20m untrusted.
func TestDealSearchLagCountsFromTheLatestCall(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.rubric("{ field: status, eq: replied_positive }", "{ field: status, in: [replied_positive, deal] }")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	start := time.Now()
	for i := 0; i < 3; i++ {
		w.hubspot.Fail("deal", sinktest.Transient)
	}
	w.clock = func() time.Time { return start }
	w.mustRun()
	w.clock = func() time.Time { return start.Add(10 * time.Minute) }
	w.mustRun()
	if r := w.push("ana@acme.example", "warm", "deal"); r["attempts"] != "0" && r["attempts"] != "" || len(w.hubspot.Calls()) < 4 {
		t.Fatalf("setup: the deal step should have been called twice: %v %v", r, calls(w.hubspot))
	}
	w.lookup("hubspot").DealsByCompany()
	w.clock = func() time.Time { return start.Add(20 * time.Minute) }
	w.mustRun()
	if o := w.outcome("ana@acme.example"); o["deal_checked_at"] != "" {
		t.Errorf("a no-deal answer 10 minutes after the latest deal call was applied: %v", o)
	}
	if s := w.outcome("ben@acme.example")["status"]; s != statusDeal {
		t.Errorf("Ben is %q, want deal", s)
	}
}

// A lookup gets each lead's done steps for its sink (LeadRef.Done), so the
// HubSpot lookup can read the contact the ledger already holds.
func TestLookupGetsTheLeadsDoneSteps(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Clerk,acme.example")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	w.mustRun()
	contact := w.push("ana@acme.example", "warm", "contact")["vendor_id"]
	if contact == "" {
		t.Fatal("setup: no contact step done")
	}
	lk := w.lookup("hubspot")
	w.mustRun()
	found := false
	for _, call := range lk.Looked() {
		for _, l := range call {
			for _, d := range l.Done {
				found = found || l.ID == w.id("ana@acme.example") && d.Key.Step == "contact" && d.VendorID == contact
			}
		}
	}
	if !found {
		t.Errorf("the lookup did not get Ana's contact step: %v", lk.Looked())
	}
}

// Done for the HubSpot lookup holds a contact step even after its lane was
// changed to push elsewhere, and the contact step of a lead merged into this
// one.
func TestLookupDoneSurvivesLaneRenamesAndMerges(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Clerk,acme.example")
	w.pushesOff()
	// The home address arrives later, so its lead is the newer one, which a
	// same_as merge absorbs.
	w.leads("ana@acme.example,Ana A,Clerk,acme.example", "ana.home@home.example,Ana Home,Clerk,acme.example")
	w.pushesOff()
	w.reply("ana.home@home.example", "replied_positive")
	w.mustRun()
	home := w.id("ana.home@home.example")
	contact := w.push("ana.home@home.example", "warm", "contact")["vendor_id"]
	if contact == "" {
		t.Fatal("setup: no contact step done")
	}
	w.rubric(`push: "hubspot:deals"`, `push: "fake:b"`) // the lane now pushes elsewhere; its done rows stay
	w.override("ana@acme.example", "same_as", "ana.home@home.example", "")
	lk := w.lookup("hubspot")
	w.mustRun()
	found := false
	for _, call := range lk.Looked() {
		for _, l := range call {
			for _, d := range l.Done {
				found = found || l.ID == w.id("ana@acme.example") && d.Key.LeadID == home && d.Key.LaneID == "warm" && d.VendorID == contact
			}
		}
	}
	if !found {
		t.Errorf("the lookup did not get the merged-in lead's contact under the renamed lane: %v", lk.Looked())
	}
}
