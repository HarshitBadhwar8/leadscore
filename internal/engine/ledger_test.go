// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// The happy path: each lead goes to its highest-priority cold lane, every
// step is called in order with the earlier steps' ids in Prior, and the rows
// end done; a second run calls nothing.
func TestColdPushHappyPathAndOnceEver(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
	res, out := w.mustRun()
	if res.Pushed != 2 {
		t.Fatalf("pushed %d, want 2\n%s", res.Pushed, out)
	}
	if got := calls(w.apollo); !slices.Equal(got, []string{"ana@acme.example seq-a contact", "ana@acme.example seq-a enroll"}) {
		t.Errorf("apollo calls %v", got)
	}
	if got := calls(w.fake); !slices.Equal(got, []string{"bo@beta.example seq-b push"}) {
		t.Errorf("fake calls %v", got)
	}
	enroll := w.apollo.Calls()[1]
	if enroll.Prior["contact"] == "" || enroll.Lead.Status != "new" {
		t.Errorf("enroll request: prior %v, status %q", enroll.Prior, enroll.Lead.Status)
	}
	for _, step := range []string{"contact", "enroll"} {
		r := w.push("ana@acme.example", "seq-a", step)
		if r["state"] != stateDone || r["vendor_id"] == "" || r["called_at"] == "" || r["intent_run"] != "" ||
			r["first_started_at"] == "" || r["lane_kind"] != kindCold || r["dest"] != "sequence/A" {
			t.Errorf("seq-a %s row %v", step, r)
		}
	}
	if w.state(ledgerRowsKey) != "3" {
		t.Errorf("ledger_rows %q, want 3", w.state(ledgerRowsKey))
	}
	if w.ranked("ana@acme.example")["lane"] != "seq-a" || w.ranked("bo@beta.example")["lane"] != "seq-b" {
		t.Errorf("Ranked lanes %v / %v", w.ranked("ana@acme.example"), w.ranked("bo@beta.example"))
	}

	// Run 2: both are contacted and nothing is pushed again.
	res, out = w.mustRun()
	if res.Pushed != 0 || len(w.apollo.Calls()) != 2 || len(w.fake.Calls()) != 1 {
		t.Fatalf("second run pushed %d, calls %v %v\n%s", res.Pushed, calls(w.apollo), calls(w.fake), out)
	}
	if s := w.outcome("ana@acme.example")["status"]; s != statusContacted {
		t.Errorf("Ana's status %q, want contacted", s)
	}
	if w.outcome("ana@acme.example")["contacted_at"] == "" {
		t.Error("a completed cold push sets contacted_at")
	}
	if l := w.ranked("ana@acme.example")["lane"]; l != "list" {
		t.Errorf("Ana's planned lane after her cold push is %q, want the export lane", l)
	}
	if r := w.ranked("ana@acme.example")["reasons"]; !strings.Contains(r, "lane seq-a skipped: already pushed") {
		t.Errorf("reasons %q", r)
	}
}

// seqANoMatch makes the Apollo lane's `when` fail for everyone, so a lead
// that was in it now matches only the plain cold lane seq-b.
func seqANoMatch(w *world) { w.rubric("{ field: tier, eq: 1 }", "{ field: tier, eq: 9 }") }

// assertOneColdPush checks a lead has at most one cold push across both cold
// vendors: the safety rule "never cold-contact a person twice".
func assertOneColdPush(t *testing.T, w *world, email string) {
	t.Helper()
	n := 0
	for _, v := range [][]string{pushedTo(w, w.apollo, "contact"), pushedTo(w, w.fake, "push")} {
		if slices.Contains(v, email) {
			n++
		}
	}
	if n > 1 {
		t.Errorf("%s got %d cold pushes", email, n)
	}
	if lanes := coldPushes(w, email); len(lanes) > 1 {
		t.Errorf("%s holds cold rows in %v", email, lanes)
	}
}

// Ledger case: a transient error, followed by the lead matching another cold
// lane, never gives a second cold push. The push's first step times out, so
// only that pending (then cancelled) row's called_at holds the push.
func TestColdPushTransientThenAnotherLane(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.apollo.FailAfter("contact", 1) // the vendor made the contact, then the call timed out
	w.mustRun()
	r := w.push("ana@acme.example", "seq-a", "contact")
	if r["state"] != statePending || r["called_at"] == "" || r["attempts"] != "0" || r["intent_run"] != "" {
		t.Fatalf("after a transient error the step is pending and called, no attempt counted: %v", r)
	}
	if r := w.push("ana@acme.example", "seq-a", "enroll"); r["state"] != statePending || r["called_at"] != "" {
		t.Fatalf("the second step was never called: %v", r)
	}
	seqANoMatch(w)
	res, out := w.mustRun()
	if len(w.fake.Calls()) != 0 || res.Pushed != 0 {
		t.Fatalf("the lead moved to another cold lane: %v\n%s", calls(w.fake), out)
	}
	if r := w.push("ana@acme.example", "seq-a", "contact"); r["state"] != stateCancelled || r["called_at"] == "" {
		t.Errorf("the step is cancelled and keeps called_at: %v", r)
	}
	// The next run explains why seq-b is skipped, and still pushes nothing.
	if res, _ := w.mustRun(); res.Pushed != 0 || len(w.fake.Calls()) != 0 {
		t.Fatal("a later run moved the lead to another cold lane")
	}
	if reasons := w.ranked("ana@acme.example")["reasons"]; !strings.Contains(reasons, "lane seq-b skipped: the lead's one cold push is held by lane seq-a") {
		t.Errorf("reasons %q", reasons)
	}
	assertOneColdPush(t, w, "ana@acme.example")
}

// A transient error is retried in the same lane next run, and only the step
// that was not done is called.
func TestTransientRetriesInTheSameLane(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.apollo.Fail("enroll", 1)
	w.mustRun()
	res, _ := w.mustRun()
	if got := calls(w.apollo); !slices.Equal(got, []string{"ana@acme.example seq-a contact", "ana@acme.example seq-a enroll", "ana@acme.example seq-a enroll"}) {
		t.Fatalf("calls %v", got)
	}
	if res.Pushed != 1 || w.push("ana@acme.example", "seq-a", "enroll")["state"] != stateDone {
		t.Errorf("the retry finishes the push: pushed %d, row %v", res.Pushed, w.push("ana@acme.example", "seq-a", "enroll"))
	}
	if w.apollo.Count("contact") != 1 {
		t.Error("the done contact step is never called again")
	}
}

// Ledger case: a skipped call (Stop arrived first) leaves the step pending and
// never called; while it is pending the lead stays in that lane even when a
// higher lane would now match; and once the lane stops matching, the step
// is cancelled with no call, so the lead's one cold push is still free.
func TestSkippedCallStaysInLaneThenAnotherLane(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"cy@cyan.example,Cy C,Head of Sales,cyan.example")
	stop := make(chan struct{})
	w.apollo.Before(func(_ context.Context, req api.StepRequest) {
		if req.Key.Step == "contact" && req.Lead.Emails[0] == "ana@acme.example" {
			closeStop(stop)
		}
	})
	w.mustRun(stopAfter(stop))
	w.apollo.Before(nil)
	if r := w.push("cy@cyan.example", "seq-a", "contact"); r["state"] != statePending || r["called_at"] != "" || r["intent_run"] != "" || r["first_started_at"] == "" {
		t.Fatalf("Cy's skipped step: %v", r)
	}
	if r := w.push("ana@acme.example", "seq-a", "enroll"); r["state"] != statePending || r["called_at"] != "" {
		t.Fatalf("Ana's skipped enroll: %v", r)
	}

	// seq-b now outranks seq-a, but Cy has an open row in seq-a: she stays.
	w.rubric("priority: 10, when: { field: receiver_only", "priority: 25, when: { field: receiver_only")
	w.mustRun()
	if len(w.fake.Calls()) != 0 {
		t.Fatalf("a lead with an open cold row moved lanes: %v", calls(w.fake))
	}
	if w.push("cy@cyan.example", "seq-a", "enroll")["state"] != stateDone || w.push("ana@acme.example", "seq-a", "enroll")["state"] != stateDone {
		t.Error("both pushes finish in seq-a")
	}
	assertOneColdPush(t, w, "ana@acme.example")
	assertOneColdPush(t, w, "cy@cyan.example")
}

// Ledger case: a skipped call, then the lane no longer matching: the
// never-called step is cancelled and the lead may take its one cold push in
// another lane.
func TestSkippedCallThenCancelFreesTheColdPush(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"cy@cyan.example,Cy C,Head of Sales,cyan.example")
	stop := make(chan struct{})
	w.apollo.Before(func(_ context.Context, req api.StepRequest) {
		if req.Lead.Emails[0] == "ana@acme.example" && req.Key.Step == "contact" {
			closeStop(stop)
		}
	})
	w.mustRun(stopAfter(stop))
	w.apollo.Before(nil)
	seqANoMatch(w)
	w.mustRun()
	if got := calls(w.fake); !slices.Equal(got, []string{"cy@cyan.example seq-b push"}) {
		t.Errorf("fake calls %v: only Cy (never called in seq-a) may go to seq-b", got)
	}
	if r := w.push("cy@cyan.example", "seq-a", "contact"); r["state"] != stateCancelled || r["called_at"] != "" {
		t.Errorf("Cy's seq-a step %v", r)
	}
	assertOneColdPush(t, w, "ana@acme.example")
	assertOneColdPush(t, w, "cy@cyan.example")
}

// Ledger case: a crash between the two ledger writes leaves intent_run set; the
// next run turns it into called_at, so the cold push stays held when the
// lead then matches another lane.
func TestCrashBetweenLedgerWritesHoldsTheColdPush(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	crashed := false
	w.apollo.Before(func(context.Context, api.StepRequest) {
		if !crashed {
			crashed = true
			panic("process killed mid-call")
		}
	})
	if _, out, err := w.run(); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("the crash run: %v\n%s", err, out)
	}
	r := w.push("ana@acme.example", "seq-a", "contact")
	if r["intent_run"] == "" || r["called_at"] != "" || r["state"] != statePending {
		t.Fatalf("after the crash the pre-batch write stands alone: %v", r)
	}
	w.apollo.Before(nil)
	seqANoMatch(w)
	w.mustRun()
	if len(w.fake.Calls()) != 0 {
		t.Fatalf("a crash mid-call let the lead take a second cold lane: %v", calls(w.fake))
	}
	r = w.push("ana@acme.example", "seq-a", "contact")
	if r["intent_run"] != "" || r["called_at"] == "" || r["state"] != stateCancelled {
		t.Errorf("the loaded intent becomes called_at, then the lane check cancels it: %v", r)
	}
	assertOneColdPush(t, w, "ana@acme.example")
}

// Ledger case: a crash, then a skipped call, then a cancel, then another cold
// lane: still one cold push.
func TestCrashSkippedCallCancelHoldsTheColdPush(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"pat@pine.example,Pat P,Clerk,pine.example")
	crashed := false
	w.apollo.Before(func(context.Context, api.StepRequest) {
		if !crashed {
			crashed = true
			panic("process killed mid-call")
		}
	})
	if _, _, err := w.run(); err == nil {
		t.Fatal("the crash run should fail")
	}
	w.apollo.Before(nil)

	// Run 2: Pat replied, so the warm (non-cold) lane runs first; Stop closes
	// during Pat's call, so Ana's call is skipped.
	w.edit(func(m *model.Model) {
		o := m.Outcomes[model.Key(w.id("pat@pine.example"))]
		o.LeadID, o.ReplyStatus, o.ReplyAt = w.id("pat@pine.example"), "replied_positive", time.Now().UTC()
		m.Put(model.TableOutcomes, o)
	})
	stop := make(chan struct{})
	w.hubspot.Before(func(context.Context, api.StepRequest) { closeStop(stop) })
	w.mustRun(stopAfter(stop))
	w.hubspot.Before(nil)
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("Ana's call should have been skipped: %v", calls(w.apollo))
	}
	r := w.push("ana@acme.example", "seq-a", "contact")
	if r["state"] != statePending || r["called_at"] == "" || r["intent_run"] != "" {
		t.Fatalf("after the skip: %v", r)
	}

	// Run 3: the lane no longer matches; the step is cancelled and still holds.
	seqANoMatch(w)
	w.mustRun()
	if slices.Contains(pushedTo(w, w.fake, "push"), "ana@acme.example") {
		t.Fatal("Ana got a second cold push")
	}
	if r := w.push("ana@acme.example", "seq-a", "contact"); r["state"] != stateCancelled || r["called_at"] == "" {
		t.Errorf("row %v", r)
	}
	assertOneColdPush(t, w, "ana@acme.example")
}

// Ledger case: two leads at one company in one batch open one deal: the second
// lead's deal step carries the first's done deal step in Related.
func TestTwoLeadsAtOneCompanyOpenOneDeal(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.hubspot.ReuseRelated("deal")
	now := time.Now().UTC()
	w.mustRun(func(_ *api.RunOptions, _ *settings) {}) // run 1 makes the leads (and pushes them cold)
	w.edit(func(m *model.Model) {
		for _, e := range []string{"ana@acme.example", "ben@acme.example"} {
			id := w.id(e)
			o := m.Outcomes[model.Key(id)]
			o.LeadID, o.ReplyStatus, o.ReplyAt = id, "replied_positive", now
			m.Put(model.TableOutcomes, o)
		}
	})
	w.mustRun()
	if n := w.hubspot.Count("deal"); n != 1 {
		t.Fatalf("%d deals at one company, want 1: %v", n, calls(w.hubspot))
	}
	if n := w.hubspot.Count("contact"); n != 2 {
		t.Errorf("%d contacts, want 2", n)
	}
	var second api.StepRequest
	for _, c := range w.hubspot.Calls() {
		if c.Key.Step == "deal" {
			second = c
		}
	}
	found := false
	for _, rel := range second.Related {
		found = found || (rel.Key.Step == "deal" && rel.State == stateDone && rel.VendorID != "" && rel.Key.LeadID != second.Key.LeadID)
	}
	if !found {
		t.Errorf("the second deal step's Related %v lacks the first lead's deal", second.Related)
	}
	if w.push("ana@acme.example", "warm", "deal")["vendor_id"] != w.push("ben@acme.example", "warm", "deal")["vendor_id"] {
		t.Error("both deal steps name the one deal")
	}
}
