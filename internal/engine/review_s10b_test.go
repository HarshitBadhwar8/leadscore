package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// Review 1: a lane whose kind changes from non-cold to cold must not let a
// person be cold-contacted twice. Bo's non-cold step times out (pending,
// called); the lane becomes cold and finishes; a higher cold lane then
// matches him: he must not be enrolled again.
func TestLaneKindChangeNeverGivesASecondColdPush(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	nonCold := "{ id: seq-b, kind: non-cold, priority: 10"
	w.rubric("{ id: seq-b, kind: cold, priority: 10", nonCold)
	w.fake.FailAfter("push", sinktest.Transient)
	w.mustRun()
	if r := w.push("bo@beta.example", "seq-b", "push"); r["state"] != statePending || r["called_at"] == "" {
		t.Fatalf("setup: %v", r)
	}
	w.write("rubric.yml", laneRubric) // seq-b is cold again
	w.mustRun()
	if r := w.push("bo@beta.example", "seq-b", "push"); r["state"] != stateDone || r["lane_kind"] != kindCold {
		t.Errorf("the row used for a cold lane is cold: %v", r)
	}
	// A higher-priority cold lane now matches Bo.
	w.rubric("{ field: tier, eq: 1 }", "{ field: tier, eq: 2 }")
	w.mustRun()
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("Bo was cold-contacted twice: %v", calls(w.apollo))
	}
}

// Review 1: a cold lane whose kind changes to non-cold keeps holding: a
// stored cold kind is never downgraded, and either kind being cold holds.
func TestLaneKindDowngradeStillHolds(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	w.fake.FailAfter("push", sinktest.Transient)
	w.mustRun() // seq-b cold: pending, called
	w.rubric("{ id: seq-b, kind: cold, priority: 10", "{ id: seq-b, kind: non-cold, priority: 10")
	w.mustRun() // finished as non-cold
	if r := w.push("bo@beta.example", "seq-b", "push"); r["lane_kind"] != kindCold {
		t.Errorf("a cold row was downgraded: %v", r)
	}
	w.write("rubric.yml", strings.Replace(strings.Replace(laneRubric, "{ id: seq-b, kind: cold, priority: 10", "{ id: seq-b, kind: non-cold, priority: 10", 1),
		"{ field: tier, eq: 1 }", "{ field: tier, eq: 2 }", 1))
	w.mustRun()
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("a lead whose cold push is held under a now non-cold lane was cold-contacted again: %v", calls(w.apollo))
	}
}

// Review 1: a lane's destination changing under a pending step. Never
// called: the step moves to the new destination. Called: it is cancelled
// (it holds the cold push) and the new destination is never called.
func TestLaneDestChangeUnderPendingStep(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example", "cy@cyan.example,Cy C,Clerk,cyan.example")
	w.fake.FailAfter("push", sinktest.Transient) // the first lead's call goes out, then times out
	w.limits("{ max_pushes_per_run: 1 }")        // the second lead's row is not created yet
	w.mustRun()
	first := w.fake.Calls()[0].Lead.Emails[0]
	w.write("rubric.yml", strings.Replace(laneRubric, `push: "fake:b"`, `push: "fake:c"`, 1))
	w.mustRun()
	r := w.push(first, "seq-b", "push")
	if r["state"] != stateCancelled || r["called_at"] == "" || r["dest"] != "b" {
		t.Errorf("a called step under a changed destination: %v", r)
	}
	for _, c := range w.fake.Calls() {
		if c.Lead.Emails[0] == first && c.Dest == "c" {
			t.Fatalf("the new destination was called for a lead already called at the old one: %v", calls(w.fake))
		}
	}
	other := "cy@cyan.example"
	if first == other {
		other = "bo@beta.example"
	}
	if got := pushedTo(w, w.fake, "push"); !slices.Contains(got, other) {
		t.Errorf("the other lead is pushed to the new destination: %v", got)
	}
}

// Review 3: a cold hubspot:deals lane at a company with two matching leads.
// The second lead's step marked by this run's pre-batch write (not yet
// called) is no deal: the first lead is pushed and opens the deal; the
// second is then held back by that called deal step.
func TestColdDealsLaneWithTwoLeadsAtACompany(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.rubric(`push: "apollo:sequence/A"`, `push: "hubspot:deals"`) // seq-a is now a cold deals lane
	w.mustRun()
	if n := w.hubspot.Count("deal"); n != 1 {
		t.Fatalf("%d deals, want the first lead's one: %v", n, calls(w.hubspot))
	}
	second := "ben@acme.example"
	if w.hubspot.Calls()[0].Lead.Emails[0] == second {
		second = "ana@acme.example"
	}
	if r := w.push(second, "seq-a", "contact"); r["state"] != stateCancelled || r["called_at"] != "" || !strings.Contains(r["last_error"], "deal") {
		t.Errorf("the colleague's step: %v", r)
	}
}

// Reviews 3 and 4: only a called `deal` step holds a company as `deal`. A
// colleague's deals push that stopped at the contact step (rate limited,
// timed out or refused), its deal step marked but never called, blocks no
// cold push and makes nobody `deal`.
func TestContactStepNeverHoldsTheCompany(t *testing.T) {
	for _, kind := range []sinktest.FailKind{sinktest.RateLimited, sinktest.Transient, sinktest.Refused} {
		t.Run(kind.String(), func(t *testing.T) {
			w := newWorld(t,
				"ana@acme.example,Ana A,Clerk,acme.example",
				"ben@acme.example,Ben B,Head of IT,acme.example")
			w.pushesOff()
			w.reply("ana@acme.example", "replied_positive")
			w.hubspot.FailAfter("contact", kind)
			w.mustRun()
			if got := calls(w.hubspot); !slices.Equal(got, []string{"ana@acme.example warm contact"}) {
				t.Fatalf("hubspot %v", got)
			}
			if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ben@acme.example"}) {
				t.Errorf("Ben's cold push was blocked by a deal that was never asked for: %v", calls(w.apollo))
			}
			w.mustRun()
			if s := w.outcome("ben@acme.example")["status"]; s == statusDeal {
				t.Error("a contact step made the company a deal")
			}
		})
	}
}

// Review 5: a deal step that timed out (no deal id) holds its company until
// a deals-by-company lookup finds no open or won deal there and reports
// deal_lost with no deal id; that releases the company.
func TestTimedOutDealStepReleasedByCompanyLookup(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	w.hubspot.Fail("deal", sinktest.Transient) // called, nothing created
	w.mustRun()
	if r := w.push("ana@acme.example", "warm", "deal"); r["called_at"] == "" || r["vendor_id"] != "" {
		t.Fatalf("setup: %v", r)
	}
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusDeal {
		t.Fatalf("held: Ben %q", s)
	}
	lk := w.lookup("hubspot")
	lk.DealsByCompany()
	w.clock = func() time.Time { return time.Now().Add(dealSearchLag + time.Minute) } // past the search-lag window
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusNew {
		t.Errorf("after the lookup found no deal, Ben is %q, want new", s)
	}
	if o := w.outcome("ana@acme.example"); o["deal_stage"] != "lost" || o["deal_checked_at"] == "" || o["deal_id"] != "" {
		t.Errorf("Ana's outcome %v", o)
	}
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ben@acme.example"}) {
		t.Errorf("Ben after the release: %v", got)
	}
}

// Review 2: a pre-batch write that fails is undone in full, State.ledger_rows
// included, so phase 2 never saves a count the ledger does not have (which
// would block every later run with ledger_shrank).
func TestFailedPreBatchWriteKeepsLedgerRows(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	w.config("sources:", "store: { type: flaky, path: leadscore.db }\nsources:")
	flaky.Lock()
	flaky.failPushes = 2 // the write and its one retry
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.failPushes = 0; flaky.Unlock() })
	res, out := w.mustRun()
	if !hasKey(res.Problems, "step_failed:push") || len(w.fake.Calls()) != 0 {
		t.Fatalf("setup: %v %v\n%s", res.Problems, calls(w.fake), out)
	}
	if v := w.state(ledgerRowsKey); v != "" && v != "0" {
		t.Errorf("ledger_rows saved as %q with an empty ledger", v)
	}
	res, _ = w.mustRun()
	if hasKey(res.Problems, "ledger_shrank") || len(w.fake.Calls()) != 1 {
		t.Errorf("the next run: %v, calls %v", res.Problems, calls(w.fake))
	}
}

// Review 6: many leads at one company stay fast: the deal rule reads a
// per-domain summary built once per view, not every colleague per lead.
func TestManyLeadsAtOneCompanyStayFast(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	var leads []string
	for i := 0; i < 3000; i++ {
		leads = append(leads, fmt.Sprintf("p%04d@big.example,Person %04d,Clerk,big.example", i, i))
	}
	w := newWorld(t, leads...)
	w.config("pushes_enabled: true", "pushes_enabled: true\ningest_chunk_rows: 5000")
	w.limits("{ max_pushes_per_run: 100 }")
	w.lookup("hubspot")
	start := time.Now()
	res, out := w.mustRun()
	took := time.Since(start)
	if res.Pushed != 100 {
		t.Fatalf("pushed %d\n%s", res.Pushed, out)
	}
	if took > 15*time.Second && !raceOn {
		t.Errorf("3,000 leads at one company took %s", took)
	}
	t.Logf("3,000 leads at one company, 100 pushes: %s", took)
}

// Review 7: vendor error text is redacted and cut before it reaches the
// ledger, the Log and Health.
func TestLongVendorErrorsAreCut(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	long := "boom from someone@vendor.example " + strings.Repeat("x", 60000)
	old := sinkFactory
	sinkFactory = func(typ string) (func(api.Config) (api.Sink, error), bool) {
		if typ == "fake" {
			return func(api.Config) (api.Sink, error) { return errSink{errors.New(long)}, nil }, true
		}
		return old(typ)
	}
	t.Cleanup(func() { sinkFactory = old })
	lk := w.lookup("hubspot")
	lk.FailLookup(w.idOrEmpty("bo@beta.example"), nil)
	for i := 0; i < 3; i++ {
		w.mustRun()
	}
	r := w.push("bo@beta.example", "seq-b", "push")
	if r["state"] != stateFailed || len(r["last_error"]) > maxErrText+10 || strings.Contains(r["last_error"], "someone@") {
		t.Errorf("last_error (%d chars) %.80q", len(r["last_error"]), r["last_error"])
	}
	for _, l := range w.rows(model.TableLog) {
		if len(l["message"]) > maxErrText+200 || strings.Contains(l["message"], "someone@") {
			t.Errorf("a Log line of %d chars", len(l["message"]))
		}
	}
	for _, h := range w.rows(model.TableHealth) {
		if len(h["value"]) > maxErrText+400 || strings.Contains(h["value"], "someone@") {
			t.Errorf("a Health value of %d chars: %.80q", len(h["value"]), h["value"])
		}
	}
	// A whole lookup failing with a long error is cut too.
	lk.FailAllLookups(errors.New(long))
	w.mustRun()
	for _, h := range w.rows(model.TableHealth) {
		if len(h["value"]) > maxErrText+400 {
			t.Errorf("a Health value of %d chars", len(h["value"]))
		}
	}
}

type errSink struct{ err error }

func (errSink) Steps(string) []string                                 { return []string{"push"} }
func (s errSink) Do(context.Context, api.StepRequest) (string, error) { return "", s.err }

func (w *world) idOrEmpty(email string) api.LeadID {
	for _, r := range w.rows(model.TableIdentities) {
		if r["key"] == email {
			return api.LeadID(r["lead_id"])
		}
	}
	return ""
}

// Review 13: a plug-in lookup's opt-out is stored with origin `lookup`, so a
// `resubscribe` never undoes it.
func TestPluginLookupOptOutSurvivesResubscribe(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	w.pushesOff()
	w.lookup("fake").OptOut("bo@beta.example")
	w.mustRun()
	if o := w.outcome("bo@beta.example"); o["unsubscribed_origin"] != "lookup" {
		t.Fatalf("outcome %v", o)
	}
	delete(w.lookups, "fake")
	w.override("bo@beta.example", "status", "resubscribe", "2026-10-07T00:00:00.000Z")
	w.mustRun()
	if s := w.outcome("bo@beta.example")["status"]; s != statusUnsubscribed || len(w.fake.Calls()) != 0 {
		t.Errorf("status %q, calls %v", s, calls(w.fake))
	}
}

// Final review 2: two leads at one company never open two deals in one run.
// Ana's deal step creates the deal, then times out (no id); Ben's deal step
// waits (pending, never called, not cancelled). The next run finishes Ana's
// and Ben's then reuses her deal through Related.
func TestSecondDealStepWaitsForTheFirst(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Clerk,acme.example")
	w.rubric("{ field: status, eq: replied_positive }", "{ field: status, in: [replied_positive, deal] }")
	w.hubspot.ReuseRelated("deal")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	w.reply("ben@acme.example", "replied_positive")
	w.hubspot.FailAfter("deal", sinktest.Transient)
	w.mustRun()
	if n := w.hubspot.Count("deal"); n != 1 {
		t.Fatalf("%d deals after run 1, want 1: %v", n, calls(w.hubspot))
	}
	first := "ana@acme.example"
	if r := w.push(first, "warm", "deal"); r["called_at"] == "" {
		first = "ben@acme.example"
	}
	second := "ben@acme.example"
	if first == second {
		second = "ana@acme.example"
	}
	if r := w.push(second, "warm", "deal"); r["state"] != statePending || r["called_at"] != "" {
		t.Fatalf("the second deal step should wait: %v", r)
	}
	w.mustRun()
	w.mustRun()
	if n := w.hubspot.Count("deal"); n != 1 {
		t.Errorf("%d deals, want 1: %v", n, calls(w.hubspot))
	}
	if a, b := w.push(first, "warm", "deal"), w.push(second, "warm", "deal"); a["state"] != stateDone || b["state"] != stateDone || a["vendor_id"] != b["vendor_id"] {
		t.Errorf("rows %v / %v", a, b)
	}
}
