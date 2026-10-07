package engine

// The S15 outcome suite: every way we learn about a reply or an opt-out
// (the receiver, polling, the HubSpot lookup, the Apollo lookup) reaches the
// status fold before the next push, including one that arrives while the run
// pushes; statuses outlive the 90-day window; the contracts section 7 worked
// example, step by step; the receiver_only_push flag; receiver silence.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

const apolloTestKey = "s15-test-key"

// realApollo points sinks.apollo's poller (and, with lookup, the Apollo
// contact Lookup, registered here as if ContactOptOutFlag were true) at a
// fake Apollo. The world's apollo sink stays the in-memory fake. It returns
// the fake and the run option that hands the run its HTTP client.
func realApollo(w *world, lookup bool) (*fakeapollo.Server, func(*api.RunOptions, *settings)) {
	w.t.Helper()
	w.t.Setenv(apollo.KeyVariable, apolloTestKey)
	f := fakeapollo.New(apolloTestKey)
	srv := httptest.NewServer(f)
	w.t.Cleanup(srv.Close)
	w.config("apollo: {}", fmt.Sprintf("apollo: { base_url: %q }", srv.URL))
	if lookup {
		old := lookupFactory
		lookupFactory = func(typ string) (func(api.Config) (api.Lookup, error), bool) {
			if typ == "apollo" {
				return apollo.NewLookup, true // the flag forced on
			}
			return old(typ)
		}
		w.t.Cleanup(func() { lookupFactory = old })
	}
	return f, func(_ *api.RunOptions, s *settings) { s.client = srv.Client() }
}

// pushedLeads lists every lead (by email) any of the world's vendors was
// called for.
func pushedLeads(w *world) []string {
	seen := map[string]bool{}
	for _, v := range [](interface{ Calls() []api.StepRequest }){w.apollo, w.hubspot, w.fake} {
		for _, c := range v.Calls() {
			if len(c.Lead.Emails) > 0 {
				seen[c.Lead.Emails[0]] = true
			}
		}
	}
	var out []string
	for e := range seen {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// blockedNotOthers checks that the next push reached Bo but not Ana, who is
// folded unsubscribed with the given origin.
func blockedNotOthers(t *testing.T, w *world, origin string) {
	t.Helper()
	if got := pushedLeads(w); !slices.Equal(got, []string{"bo@beta.example"}) {
		t.Errorf("pushed %v, want only bo (ana opted out)", got)
	}
	o := w.outcome("ana@acme.example")
	if o["status"] != statusUnsubscribed || o["unsubscribed_origin"] != origin || o["unsubscribed_at"] == "" {
		t.Errorf("ana's outcome %v, want unsubscribed with origin %s", o, origin)
	}
}

func twoLeads(t *testing.T) *world {
	return newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Head of Ops,beta.example")
}

// An unsubscribe webhook stored by the receiver blocks the next push.
func TestUnsubscribeByWebhookBlocksTheNextPush(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	w.appendEvents(replyRaw("email_unsubscribed", "Ana@ACME.example", "", time.Now().UTC()))
	w.mustRun()
	blockedNotOthers(t, w, "event")
}

// A polled reply labelled `unsubscribe` blocks the next push.
func TestUnsubscribeByPollingBlocksTheNextPush(t *testing.T) {
	w := twoLeads(t)
	f, client := realApollo(w, false)
	w.config("pushes_enabled: true", "pushes_enabled: true\nreplies: polling")
	w.pushesOff() // polls once and finds nothing
	now := time.Now().UTC()
	f.AddReply(fakeapollo.Reply{MessageID: "msg-1", Email: "ana@acme.example", Label: "unsubscribe", SentAt: now.Add(-time.Hour), RepliedAt: now})
	w.clock = func() time.Time { return time.Now().Add(7 * time.Hour) } // the next poll is due
	res, out := w.mustRun(client)
	if hasKey(res.Problems, "poll_failed:apollo") {
		t.Fatalf("the poll failed: %v\n%s", res.Problems, out)
	}
	blockedNotOthers(t, w, "event")
}

// A HubSpot contact marked opted out blocks the next push, through the
// pre-push HubSpot lookup.
func TestUnsubscribeByHubSpotBlocksTheNextPush(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	f, client := realHubSpot(w)
	f.AddContact("ana@acme.example", map[string]string{"hs_email_optout": "true"})
	f.AddContact("bo@beta.example", map[string]string{"hs_email_optout": "false"})
	f.Index()
	w.mustRun(client)
	blockedNotOthers(t, w, "lookup")
}

// An Apollo contact carrying the opt-out flag blocks the next push, through
// the Apollo contact lookup (registered only when S0 confirms the flag; it is
// forced on here).
func TestUnsubscribeByApolloLookupBlocksTheNextPush(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	f, client := realApollo(w, true)
	f.AddContact(fakeapollo.Contact{ID: "c-ana", Email: "ana@acme.example", OptedOut: true})
	f.AddContact(fakeapollo.Contact{ID: "c-bo", Email: "bo@beta.example"})
	res, out := w.mustRun(client)
	if hasKey(res.Problems, "lookup_failed:apollo") {
		t.Fatalf("the lookup failed: %v\n%s", res.Problems, out)
	}
	blockedNotOthers(t, w, "lookup")
}

// appendDuring stores an event in the event log the first time the fake
// vendor is called, as the receiver would while the run pushes.
func appendDuring(w *world, raws ...api.RawEvent) {
	var once sync.Once
	w.fake.Before(func(context.Context, api.StepRequest) {
		once.Do(func() { w.appendEvents(raws...) })
	})
}

// An unsubscribe webhook stored while the run pushes batch 1 blocks its
// lead in batch 2: the default ReRead hook reads it before the batch.
func TestUnsubscribeMidRunBlocksTheNextBatch(t *testing.T) {
	w := newWorld(t, clerks(30)...)
	w.pushesOff()
	appendDuring(w, replyRaw("email_unsubscribed", "p29@p29.example", "", time.Now().UTC()))
	res, _ := w.mustRun()
	if res.Pushed != 29 {
		t.Errorf("pushed %d, want 29", res.Pushed)
	}
	if slices.Contains(pushedLeads(w), "p29@p29.example") {
		t.Fatal("a lead who opted out mid-run was pushed in the next batch")
	}
	if r := w.push("p29@p29.example", "seq-b", "push"); r["state"] != stateCancelled || r["called_at"] != "" {
		t.Errorf("its row %v, want cancelled and never called", r)
	}
	// The re-read wrote no Seen events: the next run's Intake takes the event
	// and saves the opt-out with its key.
	if o := w.outcome("p29@p29.example"); o["status"] != statusUnsubscribed || o["unsubscribed_origin"] != "event" {
		t.Errorf("outcome %v", o)
	}
	w.mustRun()
	keys := 0
	for _, r := range w.rows(model.TableSeenEvents) {
		if strings.HasPrefix(r["event_key"], "apollo|") {
			keys++
		}
	}
	if keys != 1 {
		t.Errorf("%d receiver event keys after the next run, want 1", keys)
	}
}

// A mid-run opt-out for an email of a lead merged into another blocks the
// survivor in the next batch.
func TestUnsubscribeMidRunReachesTheSurvivor(t *testing.T) {
	w := newWorld(t, append(clerks(30), "alt@home.example,P29 Alt,Clerk,home.example")...)
	w.override("p29@p29.example", "same_as", "alt@home.example", "")
	w.pushesOff()
	if !slices.Contains(mergedInto(w), w.id("alt@home.example")) {
		t.Fatal("the merge did not happen")
	}
	appendDuring(w, replyRaw("email_unsubscribed", "alt@home.example", "", time.Now().UTC()))
	w.mustRun()
	if slices.Contains(pushedLeads(w), "p29@p29.example") {
		t.Fatal("the survivor of a lead who opted out mid-run was pushed")
	}
}

// mergedInto lists the leads that were merged into another.
func mergedInto(w *world) []api.LeadID {
	var out []api.LeadID
	for _, r := range w.rows(model.TablePeople) {
		if r["merged_into"] != "" {
			out = append(out, api.LeadID(r["lead_id"]))
		}
	}
	return out
}

// The re-read follows `replies: polling`: a receiver reply has no effect
// (polling owns replies), an unsubscribe still applies; it returns the live
// leads it changed and writes no Seen events or Window events.
func TestReReadPollingFilterAndChangedLeads(t *testing.T) {
	w := newWorld(t, clerks(30)...)
	w.config("pushes_enabled: true", "pushes_enabled: true\nreplies: polling")
	w.pushesOff()
	now := time.Now().UTC()
	appendDuring(w,
		replyRaw("email_replied_positive", "p27@p27.example", "", now),
		replyRaw("email_unsubscribed", "p28@p28.example", "", now),
		replyRaw("email_unsubscribed", "stranger@nowhere.example", "", now))
	var changed [][]api.LeadID
	var seen, window []int
	w.reread = func(r *Run) ([]api.LeadID, error) {
		c, err := reReadHook(r)
		changed = append(changed, c)
		seen, window = append(seen, len(r.Model.SeenEvents)), append(window, len(r.Model.WindowEvents))
		return c, err
	}
	w.mustRun()
	if len(changed) != 2 || len(changed[0]) != 0 || !slices.Equal(changed[1], []api.LeadID{w.id("p28@p28.example")}) {
		t.Errorf("changed per batch %v, want only p28 before batch 2", changed)
	}
	if seen[0] != seen[1] || window[0] != window[1] {
		t.Errorf("the re-read wrote Seen events %v or Window events %v", seen, window)
	}
	got := pushedLeads(w)
	if slices.Contains(got, "p28@p28.example") || !slices.Contains(got, "p27@p27.example") {
		t.Errorf("pushed %v: p28 opted out; p27's receiver reply has no effect under polling", got)
	}
	if o := w.outcome("p27@p27.example"); o["reply_status"] != "" {
		t.Errorf("p27 %v: a receiver reply under polling has no effect", o)
	}
	for _, r := range w.rows(model.TableIdentities) {
		if r["key"] == "stranger@nowhere.example" {
			t.Errorf("the re-read created a lead; the next run's Intake does")
		}
	}
}

// Statuses outlive the 90-day window: once the events are deleted and the
// window trimmed, an opt-out and a reply still hold, from Outcomes.
func TestStatusesOutliveTheWindow(t *testing.T) {
	w := twoLeads(t)
	w.leads("ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Head of Ops,beta.example",
		"cy@gamma.example,Cy C,Head of Ops,gamma.example")
	w.pushesOff()
	now := time.Now().UTC()
	w.appendEvents(
		replyRaw("email_unsubscribed", "ana@acme.example", "", now),
		replyRaw("email_replied", "cy@gamma.example", "Interested", now))
	w.config("pushes_enabled: true", "pushes_enabled: false")
	w.mustRun()
	w.config("pushes_enabled: false", "pushes_enabled: true")
	if len(w.rows(model.TableWindowEvents)) == 0 {
		t.Fatal("no window rows before the window passed")
	}

	w.clock = func() time.Time { return time.Now().Add(120 * 24 * time.Hour) }
	w.mustRun()
	w.mustRun()
	if n := len(w.rows(model.TableWindowEvents)); n != 0 {
		t.Errorf("%d window rows left after 120 days", n)
	}
	if evs, _, err := w.store().ReadEvents(context.Background(), ""); err != nil || len(evs) != 0 {
		t.Errorf("stored events after 120 days: %d (%v), want deleted", len(evs), err)
	}
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed {
		t.Errorf("ana %q, want unsubscribed", s)
	}
	if s := w.outcome("cy@gamma.example")["status"]; s != "replied_neutral" {
		t.Errorf("cy %q, want replied_neutral", s)
	}
	if got := pushedLeads(w); !slices.Equal(got, []string{"bo@beta.example"}) {
		t.Errorf("pushed %v, want only bo", got)
	}
}

// The contracts section 7 worked example, step by step: Priya is
// `contacted` by a `sent` event, `replied_positive` by her reply, `deal` once
// the warm lane opens a deal, stays `deal` on a later reply, and is
// `unsubscribed` for good once the Apollo lookup returns an opt-out.
func TestWorkedExample(t *testing.T) {
	w := newWorld(t, "priya@acme.example,Priya P,Clerk,acme.example")
	w.config("pushes_enabled: true", "pushes_enabled: false")
	status := func() string { return w.outcome("priya@acme.example")["status"] }
	now := time.Now().UTC()

	w.appendEvents(replyRaw("email_sent", "priya@acme.example", "Contacted", now))
	w.mustRun()
	if s := status(); s != statusContacted {
		t.Fatalf("after the sent event: %q, want contacted (rule 6)", s)
	}

	w.appendEvents(replyRaw("email_replied_positive", "priya@acme.example", "Interested", now.Add(time.Minute)))
	w.mustRun()
	if s := status(); s != "replied_positive" {
		t.Fatalf("after the positive reply: %q, want replied_positive (rule 5)", s)
	}

	w.config("pushes_enabled: false", "pushes_enabled: true")
	w.mustRun() // the warm lane opens a deal at her company
	if r := w.push("priya@acme.example", "warm", "deal"); r["state"] != stateDone {
		t.Fatalf("the deal step %v, want done", r)
	}
	if len(w.fake.Calls())+len(w.apollo.Calls()) != 0 {
		t.Fatalf("a replied lead was cold-pushed: %v %v", calls(w.fake), calls(w.apollo))
	}
	w.mustRun()
	if s := status(); s != statusDeal {
		t.Fatalf("after the deal: %q, want deal (rule 4)", s)
	}

	w.appendEvents(replyRaw("email_replied", "priya@acme.example", "Follow up", now.Add(2*time.Minute)))
	w.mustRun()
	if o := w.outcome("priya@acme.example"); o["status"] != statusDeal || o["reply_status"] != "replied_neutral" {
		t.Fatalf("after a later reply: %v, want reply_status replied_neutral and still deal", o)
	}

	// The pre-push lookups read the leads about to be pushed; a lead at `deal`
	// is pushed only by a lane for deal-stage leads, here one that keeps her
	// HubSpot contact current. Its push is the one the lookup's opt-out stops.
	w.rubric(`  - { id: warm,`, `  - { id: deal-sync, kind: non-cold, priority: 40, when: { field: status, eq: deal }, push: "hubspot:contacts" }
  - { id: warm,`)
	w.lookup("apollo").OptOut("priya@acme.example")
	w.mustRun()
	// She was selected for deal-sync, so the lookup read her; its opt-out
	// stopped the push before any ledger row was written or called.
	if !slices.Contains(w.apollo.LookedIDs(), w.id("priya@acme.example")) {
		t.Errorf("the Apollo lookup never read Priya: %v", w.apollo.LookedIDs())
	}
	if r := w.push("priya@acme.example", "deal-sync", "contact"); r != nil && (r["called_at"] != "" || r["state"] == stateDone) {
		t.Errorf("the deal-sync row %v, want none or never called", r)
	}
	for _, c := range w.hubspot.Calls() {
		if c.Key.LaneID == "deal-sync" {
			t.Errorf("deal-sync called the vendor: %v", c.Key)
		}
	}
	if o := w.outcome("priya@acme.example"); o["status"] != statusUnsubscribed || o["unsubscribed_origin"] != "lookup" {
		t.Fatalf("after the lookup's opt-out: %v, want unsubscribed (rule 1)", o)
	}
	delete(w.lookups, "apollo")
	w.mustRun()
	if s := status(); s != statusUnsubscribed {
		t.Errorf("a later run: %q; an automated opt-out is for good", s)
	}
	if len(w.fake.Calls())+len(w.apollo.Calls()) != 0 {
		t.Errorf("Priya was cold-pushed: %v %v", calls(w.fake), calls(w.apollo))
	}
}

// receiver_only_push: a push to a lead known only from webhooks raises a
// warning that stays while the lead is receiver-only, and clears once a
// source reports the lead.
func TestReceiverOnlyPushRaiseAndClear(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Clerk,beta.example")
	w.pushesOff()
	w.appendEvents(replyRaw("email_replied_positive", "zoe@zeta.example", "Interested", time.Now().UTC()))
	res, _ := w.mustRun()
	zoe := w.id("zoe@zeta.example")
	key := "receiver_only_push:" + string(zoe)
	if r := w.push("zoe@zeta.example", "warm", "deal"); r["state"] != stateDone {
		t.Fatalf("the warm lane did not push the webhook-only lead: %v", r)
	}
	if !hasKey(res.Problems, key) {
		t.Fatalf("problems %v, want %s", res.Problems, key)
	}
	if v := w.health()["problem:"+key]; !strings.HasPrefix(v, "warning: ") || strings.Contains(v, "@") {
		t.Errorf("Health %q, want a warning naming the lead id only", v)
	}
	if hasKey(res.Problems, "receiver_only_push:"+string(w.id("bo@beta.example"))) {
		t.Error("a lead from a source was flagged")
	}

	res, _ = w.mustRun() // nothing pushed: the flag stays
	if !hasKey(res.Problems, key) {
		t.Errorf("the flag cleared while the lead is still receiver-only: %v", res.Problems)
	}

	w.leads("bo@beta.example,Bo B,Clerk,beta.example", "zoe@zeta.example,Zoe Z,Clerk,zeta.example")
	res, _ = w.mustRun()
	if hasKey(res.Problems, key) || w.health()["problem:"+key] != "" {
		t.Errorf("the flag stayed after a source reported the lead: %v", res.Problems)
	}
}

// receiver-silence inside a run: an expected kind silent past the threshold
// makes the run unhealthy; an event of that kind clears it.
func TestReceiverSilenceInRun(t *testing.T) {
	w := twoLeads(t)
	w.config("pushes_enabled: true", "pushes_enabled: false\nreceiver: { visit_events: [visit_pricing], public_url: \"https://hooks.example\" }")
	res, _ := w.mustRun()
	if hasKey(res.Problems, "silent:sent") || hasKey(res.Problems, "silent:visit_pricing") {
		t.Fatalf("a new install is flagged: %v", res.Problems)
	}
	later := time.Now().Add(4 * 24 * time.Hour)
	w.clock = func() time.Time { return later }
	res, _ = w.mustRun()
	if !hasKey(res.Problems, "silent:sent") || !hasKey(res.Problems, "silent:visit_pricing") || res.Healthy {
		t.Fatalf("problems %v healthy %v, want both kinds silent and the run unhealthy", res.Problems, res.Healthy)
	}
	if v := w.health()["problem:silent:sent"]; v == "" || strings.HasPrefix(v, "warning: ") {
		t.Errorf("silent:sent in Health %q, want an error", v)
	}
	w.appendEvents(replyRaw("email_sent", "ana@acme.example", "Contacted", later.UTC()))
	res, _ = w.mustRun()
	if hasKey(res.Problems, "silent:sent") || !hasKey(res.Problems, "silent:visit_pricing") {
		t.Errorf("after a sent event: %v, want only visit_pricing silent", res.Problems)
	}
}

// RecordCrash writes a crashed run to Health under the lease: unhealthy,
// run_failed with the cause (redacted), the other open problems kept with
// their first_seen_at. With the lease held by another run it writes nothing.
func TestRecordCrash(t *testing.T) {
	w := twoLeads(t)
	w.config("pushes_enabled: true", "pushes_enabled: false\nreceiver: { visit_events: [visit_pricing], public_url: \"https://hooks.example\" }")
	w.mustRun()
	w.clock = func() time.Time { return time.Now().Add(4 * 24 * time.Hour) }
	w.mustRun() // silent:sent is open
	before := w.rows(model.TableHealth)
	first := ""
	for _, r := range before {
		if r["key"] == "silent:sent" {
			first = r["first_seen_at"]
		}
	}
	if first == "" {
		t.Fatal("no open problem to keep")
	}
	start := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	s := w.store()
	if err := RecordCrash(context.Background(), s, start, errors.New("panic: boom for ana@acme.example")); err != nil {
		t.Fatal(err)
	}
	h := w.health()
	if h["result:last_result"] != "unhealthy" || h["result:last_run_at"] != model.FormatTime(start) {
		t.Errorf("Health %v", h)
	}
	if v := h["problem:run_failed"]; !strings.Contains(v, "boom") || strings.Contains(v, "ana@acme.example") {
		t.Errorf("run_failed %q, want the cause, redacted", v)
	}
	for _, r := range w.rows(model.TableHealth) {
		if r["key"] == "silent:sent" && r["first_seen_at"] != first {
			t.Errorf("an open problem lost its first_seen_at: %v", r)
		}
	}
	if !hasKey(keysOf(w.health()), "problem:silent:sent") {
		t.Error("the crash dropped an open problem")
	}
	if l := w.state("lease_owner"); l != "" {
		if exp, _ := model.ParseTime(w.state("lease_expires_at")); exp.After(time.Now()) {
			t.Errorf("the lease is still held after the write: %s", l)
		}
	}

	w.setLease("another-run", time.Now().Add(time.Hour))
	err := RecordCrash(context.Background(), s, start.Add(time.Hour), errors.New("panic: again"))
	if !errors.Is(err, api.ErrLeaseHeld) {
		t.Errorf("err %v, want ErrLeaseHeld", err)
	}
	if h := w.health(); h["result:last_run_at"] != model.FormatTime(start) {
		t.Errorf("wrote under another run's lease: %v", h)
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// An opt-out reaching a hand-edited merged_into cycle is kept on every
// member, so neither is pushed once a person fixes the cycle.
func TestOptOutInACycleSurvivesTheFix(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	a, b := w.id("ana@acme.example"), w.id("bo@beta.example")
	setMerged := func(ma, mb api.LeadID) {
		w.edit(func(m *model.Model) {
			pa, pb := m.People[model.Key(a)], m.People[model.Key(b)]
			pa.MergedInto, pb.MergedInto = ma, mb
			m.Put(model.TablePeople, pa)
			m.Put(model.TablePeople, pb)
		})
	}
	setMerged(b, a)
	w.appendEvents(replyRaw("email_unsubscribed", "bo@beta.example", "", time.Now().UTC()))
	w.mustRun()
	setMerged("", "")
	w.mustRun()
	if got := pushedLeads(w); len(got) != 0 {
		t.Errorf("pushed %v after the cycle was fixed; the opt-out reached both members", got)
	}
	for _, e := range []string{"ana@acme.example", "bo@beta.example"} {
		if s := w.outcome(e)["status"]; s != statusUnsubscribed {
			t.Errorf("%s %q, want unsubscribed", e, s)
		}
	}
}

// flakyWorld is a world on the flaky store, whose event log tests can count
// and make shrink.
func flakyWorld(t *testing.T, leads ...string) *world {
	w := newWorld(t, leads...)
	w.cfg = "store: { type: flaky, path: leadscore.db }\n" + laneConfig
	w.install.config(w.cfg)
	flaky.Lock()
	flaky.eventsRead, flaky.shrinkEvents = 0, false
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.eventsRead, flaky.shrinkEvents = 0, false; flaky.Unlock() })
	return w
}

// A reply Apollo sends again mid-run, already taken by an earlier run, is
// skipped by the re-read as Intake skips it: it never moves a stored
// replied_positive back to replied_neutral.
func TestReReadSkipsARedeliveredReply(t *testing.T) {
	w := newWorld(t, clerks(30)...)
	now := time.Now().UTC()
	first := replyRaw("email_replied", "p00@p00.example", "Interested", now.Add(-2*time.Minute))
	w.appendEvents(first, replyRaw("email_replied_positive", "p00@p00.example", "Meeting", now.Add(-time.Minute)))
	w.pushesOff()
	if s := w.outcome("p00@p00.example")["reply_status"]; s != "replied_positive" {
		t.Fatalf("reply_status %q before the redelivery", s)
	}
	again := first
	again.ReceivedAt = now // the same event, delivered again later
	appendDuring(w, again)
	w.mustRun()
	if s := w.outcome("p00@p00.example")["reply_status"]; s != "replied_positive" {
		t.Errorf("after a mid-run redelivery: reply_status %q, want replied_positive", s)
	}
	w.mustRun()
	if s := w.outcome("p00@p00.example")["reply_status"]; s != "replied_positive" {
		t.Errorf("the next run: reply_status %q, want replied_positive", s)
	}
}

// Each re-read reads only what arrived since the last one: over three
// batches, an event stored during the first is read once, not before every
// later batch.
func TestReReadReadsEachEventOnce(t *testing.T) {
	w := flakyWorld(t, clerks(60)...)
	w.limits("{ max_pushes_per_run: 100, max_pushes_per_day: 100 }")
	w.pushesOff()
	flaky.Lock()
	flaky.eventsRead = 0
	flaky.Unlock()
	appendDuring(w, replyRaw("email_unsubscribed", "p59@p59.example", "", time.Now().UTC()))
	res, _ := w.mustRun()
	if res.Pushed != 59 {
		t.Errorf("pushed %d, want 59", res.Pushed)
	}
	flaky.Lock()
	n := flaky.eventsRead
	flaky.Unlock()
	if n != 1 {
		t.Errorf("the run read %d stored events, want the one event read once", n)
	}
}

// A mid-run opt-out naming an unknown email and an existing lead's LinkedIn
// URL blocks that lead in the next batch, as Intake would apply it.
func TestReReadOptOutReachesTheLinkedInHolder(t *testing.T) {
	w := newWorld(t)
	lines := []string{"Email,Name,Title,Domain,LinkedIn URL"}
	for i, l := range clerks(30) {
		li := ""
		if i == 29 {
			li = "https://www.linkedin.com/in/p29-example"
		}
		lines = append(lines, l+","+li)
	}
	w.write("leads.csv", csvText(lines...))
	w.pushesOff()
	appendDuring(w, api.RawEvent{Kind: "apollo_reply", ReceivedAt: time.Now().UTC(), Body: []byte(
		`{"event":"email_unsubscribed","contact_email":"someone.new@elsewhere.example","contact_linkedin_url":"https://linkedin.com/in/P29-example/"}`)})
	w.mustRun()
	if slices.Contains(pushedLeads(w), "p29@p29.example") {
		t.Fatal("the LinkedIn holder of a mid-run opt-out was pushed in the next batch")
	}
}

// A re-read that fails mid-run (the event log shrank) stops pushing for the
// rest of the run, with step_failed:reread.
func TestDefaultReReadFailureStopsPushing(t *testing.T) {
	w := flakyWorld(t, clerks(30)...)
	w.pushesOff()
	var once sync.Once
	w.fake.Before(func(context.Context, api.StepRequest) {
		once.Do(func() { flaky.Lock(); flaky.shrinkEvents = true; flaky.Unlock() })
	})
	res, _ := w.mustRun()
	if n := len(w.fake.Calls()); n != 25 {
		t.Errorf("calls %d, want only the first batch (25)", n)
	}
	if !hasKey(res.Problems, "step_failed:reread") || res.Healthy {
		t.Errorf("problems %v healthy %v", res.Problems, res.Healthy)
	}
}

// receiver_only_push on a cold lane: a lane that does not require
// receiver_only false pushes a webhook-only lead, and the flag is raised.
func TestReceiverOnlyPushOnAColdLane(t *testing.T) {
	w := newWorld(t, "bo@beta.example,Bo B,Head of Ops,beta.example")
	w.rubric(`when: { field: receiver_only, eq: false }, push: "fake:b"`, `when: { field: tier, eq: 2 }, push: "fake:b"`)
	w.pushesOff()
	w.appendEvents(visitRaw("zoe@zeta.example", ago(time.Hour), time.Now().UTC()))
	res, _ := w.mustRun()
	zoe := w.id("zoe@zeta.example")
	if r := w.push("zoe@zeta.example", "seq-b", "push"); r == nil || r["state"] != stateDone {
		t.Fatalf("the cold lane did not push the webhook-only lead: %v", r)
	}
	if !hasKey(res.Problems, "receiver_only_push:"+string(zoe)) {
		t.Errorf("problems %v, want receiver_only_push for the cold push", res.Problems)
	}
}

// A resubscribe row logs per person, however many leads of the family held
// a manual opt-out.
func TestResubscribeLogsPerPerson(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ana.alt@home.example,Ana Alt,Clerk,home.example")
	w.config("pushes_enabled: true", "pushes_enabled: false")
	w.mustRun()
	for _, e := range []string{"ana@acme.example", "ana.alt@home.example"} {
		id := w.id(e)
		w.edit(func(m *model.Model) {
			o := m.Outcomes[model.Key(id)]
			o.LeadID, o.UnsubscribedAt, o.UnsubscribedOrigin = id, time.Now().UTC(), unsubManual
			m.Put(model.TableOutcomes, o)
		})
	}
	w.override("ana@acme.example", "same_as", "ana.alt@home.example", "")
	w.override("ana@acme.example", "status", "resubscribe", "")
	w.mustRun()
	var msgs []string
	for _, r := range w.rows(model.TableLog) {
		if r["kind"] == logResubscribe {
			msgs = append(msgs, r["message"])
		}
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "cleared the person's manual opt-out") || strings.Contains(msgs[0], "2 manual") {
		t.Errorf("resubscribe log %q, want one line for the person", msgs)
	}
}

// A wrong merge undone by hand keeps each opt-out with the person who opted
// out, in both directions: while merged the whole family is blocked; after
// the un-merge the opted-out person stays blocked and the other is
// contactable.
func TestUnmergeKeepsTheOptOutWithItsOwner(t *testing.T) {
	for _, optedOut := range []string{"ana@acme.example", "bea@beta.example"} {
		t.Run(optedOut, func(t *testing.T) {
			w := newWorld(t,
				"bea@beta.example,Bea B,Head of Ops,beta.example",
				"ana@acme.example,Ana A,Head of Ops,acme.example")
			w.override("bea@beta.example", "same_as", "ana@acme.example", "")
			w.pushesOff()
			ana := w.id("ana@acme.example")
			if !slices.Contains(mergedInto(w), ana) {
				t.Fatal("ana was not merged into bea")
			}
			w.appendEvents(replyRaw("email_unsubscribed", optedOut, "", time.Now().UTC()))
			w.config("pushes_enabled: true", "pushes_enabled: false")
			w.mustRun()
			if s := w.outcome("bea@beta.example")["status"]; s != statusUnsubscribed {
				t.Fatalf("the merged family %q, want unsubscribed", s)
			}
			// A person undoes the wrong merge by hand.
			w.edit(func(m *model.Model) {
				p := m.People[model.Key(ana)]
				p.MergedInto = ""
				m.Put(model.TablePeople, p)
				for _, o := range m.Overrides {
					if o.Action == "same_as" {
						m.Delete(model.TableOverrides, []string{o.Person, o.Action, o.Value, o.Note})
					}
				}
			})
			w.config("pushes_enabled: false", "pushes_enabled: true")
			w.mustRun()
			other := "ana@acme.example"
			if optedOut == other {
				other = "bea@beta.example"
			}
			if got := pushedLeads(w); !slices.Equal(got, []string{other}) {
				t.Errorf("pushed %v, want only %s (contactable); %s opted out", got, other, optedOut)
			}
			if s := w.outcome(optedOut)["status"]; s != statusUnsubscribed {
				t.Errorf("%s %q after the un-merge, want unsubscribed", optedOut, s)
			}
		})
	}
}

// RecordCrash reads only State's schema_version and Health: it writes even
// when loading the whole store would panic (a table that cannot be read), a
// panic of its own comes back as an error, and a store from a newer major
// schema gets no write at all.
func TestRecordCrashWithABrokenStore(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	s := w.store()
	start := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	panicky := &panickyStore{Store: s}
	if err := RecordCrash(context.Background(), panicky, start, errors.New("panic: boom")); err != nil {
		t.Fatalf("a store whose other tables panic: %v", err)
	}
	if h := w.health(); h["result:last_run_at"] != model.FormatTime(start) || !strings.Contains(h["problem:run_failed"], "boom") {
		t.Errorf("Health %v", h)
	}
	panicky.commitPanics = true
	if err := RecordCrash(context.Background(), panicky, start.Add(time.Hour), errors.New("panic: again")); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err %v, want the write's panic recovered into the error", err)
	}

	if err := s.Commit(context.Background(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "schema_version", "value": "99.0"}}}}); err != nil {
		t.Fatal(err)
	}
	before := w.rows(model.TableHealth)
	err := RecordCrash(context.Background(), s, start.Add(2*time.Hour), errors.New("panic: newer"))
	if !errors.Is(err, codec.ErrNewerSchema) {
		t.Errorf("err %v, want ErrNewerSchema", err)
	}
	if after := w.rows(model.TableHealth); !reflect.DeepEqual(before, after) {
		t.Errorf("wrote Health into a newer-schema store:\n%v\n%v", before, after)
	}
}

// panickyStore panics reading any table but Health (so a full load panics),
// and, when set, on Commit.
type panickyStore struct {
	*sqlite.Store
	commitPanics bool
}

func (p *panickyStore) ReadTable(ctx context.Context, name string) ([]api.Row, error) {
	if name != model.TableHealth && name != model.TableState {
		panic("cannot read " + name)
	}
	return p.Store.ReadTable(ctx, name)
}

func (p *panickyStore) Commit(ctx context.Context, w []api.TableWrite) error {
	if p.commitPanics {
		panic("commit")
	}
	return p.Store.Commit(ctx, w)
}

// A redelivery inside one re-read is taken once, as Intake takes it: the
// later copy of a positive reply never overrides the neutral reply that came
// between.
func TestReReadTakesADuplicateInOneReadOnce(t *testing.T) {
	w := newWorld(t, clerks(30)...)
	w.pushesOff()
	now := time.Now().UTC()
	positive := replyRaw("email_replied_positive", "p29@p29.example", "Meeting", now.Add(-2*time.Minute))
	again := positive
	again.ReceivedAt = now
	appendDuring(w, positive, replyRaw("email_replied", "p29@p29.example", "Follow up", now.Add(-time.Minute)), again)
	w.mustRun()
	if s := w.outcome("p29@p29.example")["reply_status"]; s != "replied_neutral" {
		t.Errorf("after the re-read: reply_status %q, want replied_neutral (the repeat is skipped)", s)
	}
	w.mustRun() // Intake takes the same events: the same answer
	if s := w.outcome("p29@p29.example")["reply_status"]; s != "replied_neutral" {
		t.Errorf("the next run: reply_status %q, want replied_neutral", s)
	}
}

// Intake never stores a last_received later than the run's clock, and
// replaces a stored one that is.
func TestIntakeCapsLastReceivedAtNow(t *testing.T) {
	w := twoLeads(t)
	w.pushesOff()
	start := time.Now().UTC()
	w.appendEvents(replyRaw("email_sent", "ana@acme.example", "Contacted", start.Add(400*24*time.Hour)))
	w.mustRun()
	got, err := model.ParseTime(w.state("last_received:sent"))
	if err != nil || got.After(time.Now()) || got.Before(start) {
		t.Errorf("last_received:sent %v (%v), want capped at the run's clock", got, err)
	}
	w.edit(func(m *model.Model) {
		m.SetState("last_received:sent", model.FormatTime(start.Add(400*24*time.Hour)))
	})
	w.appendEvents(replyRaw("email_sent", "bo@beta.example", "Contacted", time.Now().UTC()))
	w.mustRun()
	if got, _ := model.ParseTime(w.state("last_received:sent")); got.After(time.Now()) {
		t.Errorf("a stored future last_received:sent %v was kept", got)
	}
}
