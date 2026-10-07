package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// C8 `pending, called` from a rate limit: no attempt counted, and the sink
// stops for the run while other sinks go on.
func TestRateLimitStopsTheSinkForTheRun(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"cy@cyan.example,Cy C,Head of Sales,cyan.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.apollo.Fail("contact", sinktest.RateLimited)
	res, _ := w.mustRun()
	if got := calls(w.apollo); len(got) != 1 {
		t.Fatalf("after a 429 the Apollo sink stops for the run: %v", got)
	}
	if got := calls(w.fake); !slices.Equal(got, []string{"bo@beta.example seq-b push"}) {
		t.Errorf("other sinks go on: %v", got)
	}
	first := w.apollo.Calls()[0].Lead.Emails[0]
	r := w.push(first, "seq-a", "contact")
	if r["state"] != statePending || r["called_at"] == "" || r["attempts"] != "0" || !strings.Contains(r["last_error"], "429") {
		t.Errorf("rate-limited row %v", r)
	}
	other := "cy@cyan.example"
	if first == other {
		other = "ana@acme.example"
	}
	if r := w.push(other, "seq-a", "contact"); r["state"] != statePending || r["called_at"] != "" || r["intent_run"] != "" {
		t.Errorf("the waiting lead's row %v", r)
	}
	if hasKey(res.Problems, "push_failed:"+string(w.id(first))+":seq-a:contact") {
		t.Error("a rate limit is not a failure")
	}
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); len(got) != 2 {
		t.Errorf("both finish next run: %v", got)
	}
}

// C8 `failed`: three counted attempts; the step then waits for a retry row,
// which resets it once; the `pushes` check reports it meanwhile.
func TestThreeFailuresThenRetry(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	for i := 0; i < 3; i++ {
		w.apollo.Fail("contact", sinktest.Other)
	}
	w.mustRun()
	if r := w.push("ana@acme.example", "seq-a", "contact"); r["state"] != statePending || r["attempts"] != "1" || r["called_at"] == "" {
		t.Fatalf("after one failure: %v", r)
	}
	w.mustRun()
	res, _ := w.mustRun()
	key := "push_failed:" + string(w.id("ana@acme.example")) + ":seq-a:contact"
	if r := w.push("ana@acme.example", "seq-a", "contact"); r["state"] != stateFailed || r["attempts"] != "3" {
		t.Fatalf("after three failures: %v", r)
	}
	if !hasKey(res.Problems, key) || res.Healthy {
		t.Errorf("problems %v", res.Problems)
	}
	// A failed step holds the cold push and is not called again.
	res, _ = w.mustRun()
	if len(w.apollo.Calls()) != 3 || !hasKey(res.Problems, key) || len(w.fake.Calls()) != 0 {
		t.Fatalf("a failed step was called again, or the lead moved lanes: %v %v", calls(w.apollo), calls(w.fake))
	}
	w.edit(func(m *model.Model) { merge.AddRetry(m, "ana@acme.example", "seq-a", time.Now()) })
	res, _ = w.mustRun()
	if r := w.push("ana@acme.example", "seq-a", "enroll"); r["state"] != stateDone {
		t.Errorf("after the retry the push finishes: %v", r)
	}
	if hasKey(res.Problems, key) {
		t.Errorf("the problem clears once retried: %v", res.Problems)
	}
	if len(w.rows(model.TableAppliedOverrides)) != 1 {
		t.Error("the retry row is applied once")
	}
	// The same retry row does nothing more.
	w.apollo.Fail("contact", sinktest.Other)
	w.mustRun()
	if n := len(w.apollo.Calls()); n != 5 {
		t.Errorf("calls %d", n)
	}
}

// C8 `cancelled` by a refusal: the call went out, so it holds the cold push;
// it is logged, and does not make the run unhealthy.
func TestRefusalCancelsAndHolds(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.apollo.Fail("enroll", sinktest.Refused)
	res, _ := w.mustRun()
	r := w.push("ana@acme.example", "seq-a", "enroll")
	if r["state"] != stateCancelled || r["called_at"] == "" || !strings.HasPrefix(r["last_error"], "refused:") {
		t.Fatalf("refused row %v", r)
	}
	for _, p := range res.Problems {
		if strings.HasPrefix(p, "push_failed") {
			t.Errorf("a refusal raised %s", p)
		}
	}
	w.mustRun()
	if len(w.fake.Calls()) != 0 || len(w.apollo.Calls()) != 2 {
		t.Errorf("a refused lead moved lanes or was called again: %v %v", calls(w.apollo), calls(w.fake))
	}
	found := false
	for _, l := range w.rows(model.TableLog) {
		found = found || l["kind"] == logPushRefused
	}
	if !found {
		t.Error("no push_refused log line")
	}
}

// C8 `cancelled` with neither called_at nor intent_run returns to pending
// when the lead is selected for the lane again.
func TestNeverCalledCancelReselects(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.pushesOff()
	id := w.id("ana@acme.example")
	w.edit(func(m *model.Model) {
		for _, step := range []string{"contact", "enroll"} {
			m.Put(model.TablePushes, model.Push{LeadID: id, LaneID: "seq-a", Step: step, LaneKind: kindCold,
				Dest: "sequence/A", State: stateCancelled, LastError: "status replied_negative", UpdatedAt: time.Now().UTC()})
		}
	})
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ana@acme.example"}) {
		t.Fatalf("a never-called cancelled push is reselected: %v", calls(w.apollo))
	}
	if r := w.push("ana@acme.example", "seq-a", "contact"); r["state"] != stateDone || r["last_error"] != "" {
		t.Errorf("row %v", r)
	}
}

// A lane removed from the rubric cancels its pending steps.
func TestRemovedLaneCancels(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	w.fake.Fail("push", sinktest.Transient)
	w.mustRun()
	w.write("rubric.yml", strings.Replace(laneRubric,
		`  - { id: seq-b, kind: cold, priority: 10, when: { field: receiver_only, eq: false }, push: "fake:b" }
`, "", 1))
	w.mustRun()
	if r := w.push("bo@beta.example", "seq-b", "push"); r["state"] != stateCancelled || r["last_error"] != "the lane was removed from the rubric" {
		t.Errorf("row %v", r)
	}
	if len(w.fake.Calls()) != 1 {
		t.Error("a removed lane's step was called")
	}
}

// No step is called for a merged lead; at load its pending steps never
// called are cancelled, and the survivor's cold push counts the absorbed
// lead's rows.
func TestMergedLeadIsNeverCalled(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"ana.private@home.example,Ana Private,Clerk,home.example")
	w.pushesOff()
	alt := w.id("ana.private@home.example")
	w.edit(func(m *model.Model) {
		// alt has a done cold push in seq-b, and a stray never-called row.
		m.Put(model.TablePushes, model.Push{LeadID: alt, LaneID: "seq-b", Step: "push", LaneKind: kindCold, Dest: "b",
			State: stateDone, VendorID: "push-old", CalledAt: time.Now().UTC(), FirstStartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		m.Put(model.TablePushes, model.Push{LeadID: alt, LaneID: "seq-a", Step: "contact", LaneKind: kindCold, Dest: "sequence/A",
			State: statePending, UpdatedAt: time.Now().UTC()})
	})
	w.override("ana@acme.example", "same_as", "ana.private@home.example", "")
	w.mustRun()
	if n := len(w.apollo.Calls()) + len(w.fake.Calls()); n != 0 {
		t.Fatalf("the survivor of a cold-pushed lead was pushed again, or the merged lead was called: %v %v", calls(w.apollo), calls(w.fake))
	}
	for _, r := range w.rows(model.TablePushes) {
		if r["lead_id"] == string(alt) && r["lane_id"] == "seq-a" && (r["state"] != stateCancelled || r["last_error"] != "the lead was merged into another lead") {
			t.Errorf("merged lead's pending row %v", r)
		}
	}
	if r := w.ranked("ana@acme.example"); !strings.Contains(r["reasons"], "held by lane seq-b") {
		t.Errorf("reasons %q", r["reasons"])
	}
}

// Deleted ledger rows block every push (contracts section 8): the run raises
// ledger_shrank, keeps ledger_rows, and pushes nothing until restored.
func TestDeletedLedgerRowsBlockPushes(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.fake.Fail("push", sinktest.Transient)
	w.mustRun()
	if w.state(ledgerRowsKey) != "3" {
		t.Fatalf("ledger_rows %q", w.state(ledgerRowsKey))
	}
	saved := w.push("ana@acme.example", "seq-a", "enroll")
	if err := w.store().Commit(context.Background(), []api.TableWrite{{Table: model.TablePushes, Op: api.OpDelete,
		Key: []string{"lead_id", "lane_id", "step"}, Rows: []api.Row{{"lead_id": saved["lead_id"], "lane_id": "seq-a", "step": "enroll"}}}}); err != nil {
		t.Fatal(err)
	}
	w.edit(func(m *model.Model) {}) // nothing else changes
	res, _ := w.mustRun()
	if !hasKey(res.Problems, "ledger_shrank") || res.Healthy {
		t.Errorf("problems %v", res.Problems)
	}
	if len(w.fake.Calls()) != 1 || len(w.apollo.Calls()) != 2 {
		t.Fatalf("pushes went out with a shrunk ledger: %v %v", calls(w.apollo), calls(w.fake))
	}
	if w.state(ledgerRowsKey) != "3" {
		t.Errorf("ledger_rows lowered to %q", w.state(ledgerRowsKey))
	}
	// Restored: pushing resumes, and Ana is not pushed again.
	if err := w.store().Commit(context.Background(), []api.TableWrite{{Table: model.TablePushes, Op: api.OpUpsert,
		Key: []string{"lead_id", "lane_id", "step"}, Rows: []api.Row{saved}}}); err != nil {
		t.Fatal(err)
	}
	res, _ = w.mustRun()
	if hasKey(res.Problems, "ledger_shrank") || len(w.fake.Calls()) != 2 || len(w.apollo.Calls()) != 2 {
		t.Errorf("after the restore: %v; calls %v %v", res.Problems, calls(w.apollo), calls(w.fake))
	}
}

// A sink returning no vendor id with no error has not done the step.
func TestEmptyVendorIDCountsAnAttempt(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	old := sinkFactory
	sinkFactory = func(typ string) (func(api.Config) (api.Sink, error), bool) {
		if typ == "fake" {
			return func(api.Config) (api.Sink, error) { return emptySink{}, nil }, true
		}
		return old(typ)
	}
	t.Cleanup(func() { sinkFactory = old })
	w.mustRun()
	if r := w.push("bo@beta.example", "seq-b", "push"); r["state"] != statePending || r["attempts"] != "1" {
		t.Errorf("row %v", r)
	}
}

type emptySink struct{}

func (emptySink) Steps(string) []string { return []string{"push"} }
func (emptySink) Do(context.Context, api.StepRequest) (string, error) {
	return "", nil
}
