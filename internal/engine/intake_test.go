package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// eventsRubric fires `hot` on three visits in a week and lists hot leads.
const eventsRubric = `version: 1
detectors:
  hot: { kind: count_in_window, event: "visit_*", window: 7d, min: 3 }
  new_co: { kind: first_seen, subject: company, event: "visit_*", within: 7d }
derive:
  hot_lead:
    - when: { detector: hot }
      then: hot
    - else: cold
lanes:
  - { id: list, kind: export, when: { field: hot_lead, eq: hot }, push: "export:list" }
`

func (in *install) appendEvents(evs ...api.RawEvent) {
	in.t.Helper()
	if err := in.store().AppendEvents(context.Background(), evs); err != nil {
		in.t.Fatal(err)
	}
}

func visitRaw(email, visitedAt string, received time.Time) api.RawEvent {
	return api.RawEvent{Kind: "apollo_visit", ReceivedAt: received, Body: []byte(
		`{"event":"website_visited_pricing","domain":"ourproduct.example","visited_at":"` + visitedAt + `",` +
			`"contact":{"email":"` + email + `","first_name":"Lee"},"account":{"domain":"example.net"}}`)}
}

func replyRaw(event, email, stage string, received time.Time) api.RawEvent {
	return api.RawEvent{Kind: "apollo_reply", ReceivedAt: received, Body: []byte(
		`{"event":"` + event + `","contact_email":"` + email + `","contact_stage":"` + stage + `"}`)}
}

func ranked(in *install) map[string]api.Row {
	out := map[string]api.Row{}
	for _, r := range in.rows(model.TableRanked) {
		out[r["email"]] = r
	}
	return out
}

func outcomeOf(in *install, lead string) api.Row {
	for _, r := range in.rows(model.TableOutcomes) {
		if r["lead_id"] == lead {
			return r
		}
	}
	return nil
}

func leadOf(in *install, email string) string {
	for _, r := range in.rows(model.TableIdentities) {
		if r["key"] == email {
			return r["lead_id"]
		}
	}
	return ""
}

func ago(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.RFC3339) }

// The proof: three visits arriving over three runs count three, because
// detectors read Window events, not this run's events.
func TestThreeVisitsAcrossThreeRunsFireADetector(t *testing.T) {
	in := newInstall(t, "receiver: { visit_events: [visit_pricing] }\n", eventsRubric)
	for i, want := range []string{"cold", "cold", "hot"} {
		in.appendEvents(visitRaw("Lee.Park@Example.net", ago(time.Duration(3-i)*time.Hour), time.Now()))
		res, out, err := in.run(DefaultHooks())
		if err != nil || !res.Healthy {
			t.Fatalf("run %d: %+v %v\n%s", i+1, res, err, out)
		}
		if got := ranked(in)["lee.park@example.net"]["hot_lead"]; got != want {
			t.Fatalf("run %d: hot_lead %q, want %q", i+1, got, want)
		}
	}
	if n := len(in.rows(model.TableWindowEvents)); n != 3 {
		t.Errorf("Window events: %d rows, want 3", n)
	}
	if n := len(in.rows(model.TablePeople)); n != 1 {
		t.Errorf("three visits by one person made %d leads", n)
	}
	if in.state("cursor:events") != "3" || in.state("last_received:visit_pricing") == "" {
		t.Errorf("cursor %q, last_received %q", in.state("cursor:events"), in.state("last_received:visit_pricing"))
	}
	// First seen: the lead's and the company's, at the first visit's time.
	lead := leadOf(in, "lee.park@example.net")
	var people api.Row
	for _, r := range in.rows(model.TablePeople) {
		if r["lead_id"] == lead {
			people = r
		}
	}
	if !strings.Contains(people["first_seen"], "visit_pricing") {
		t.Errorf("People.first_seen: %q", people["first_seen"])
	}
	companySeen := ""
	for _, r := range in.rows(model.TableCompanyFacts) {
		if r["domain"] == "example.net" {
			companySeen = r["first_seen"]
		}
	}
	if !strings.Contains(companySeen, "visit_pricing") {
		t.Errorf("Company facts first_seen for example.net: %q", companySeen)
	}
}

// A redelivered body, in the same run or a later one, is one event: one Seen
// key, one Window row, one count.
func TestDedupeAcrossDeliveriesAndRuns(t *testing.T) {
	in := newInstall(t, "", eventsRubric)
	at := ago(time.Hour)
	in.appendEvents(visitRaw("lee@example.net", at, time.Now()), visitRaw("lee@example.net", at, time.Now()))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	in.appendEvents(visitRaw("LEE@example.net", at, time.Now()))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if n := len(in.rows(model.TableWindowEvents)); n != 1 {
		t.Errorf("Window events: %d, want 1", n)
	}
	if n := len(in.rows(model.TableSeenEvents)); n != 1 {
		t.Errorf("Seen events: %d, want 1", n)
	}
	// Two sends of one stage are one key too; a moved stage is a new one.
	in.appendEvents(replyRaw("email_sent", "lee@example.net", "Approaching", time.Now()),
		replyRaw("email_sent", "lee@example.net", "Approaching", time.Now()),
		replyRaw("email_replied", "lee@example.net", "Replied", time.Now()))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if n := len(in.rows(model.TableSeenEvents)); n != 3 {
		t.Errorf("Seen events: %d, want 3", n)
	}
	o := outcomeOf(in, leadOf(in, "lee@example.net"))
	if o["contacted_at"] == "" || o["reply_status"] != "replied_neutral" {
		t.Errorf("outcome %v", o)
	}
	// last_received is kept for reply kinds and configured visit kinds only.
	if in.state("last_received:sent") == "" || in.state("last_received:visit_pricing") != "" {
		t.Errorf("last_received: sent %q, unconfigured visit_pricing %q", in.state("last_received:sent"), in.state("last_received:visit_pricing"))
	}
}

// The safety proof: an opt-out for an email that belongs to a lead merged
// away is stored on that lead (the key's owner, so an un-merge keeps it with
// the person), blocks the live lead through the family, and creates no new
// lead.
func TestOptOutLandsOnTheLiveLeadThroughMergedInto(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: rows, type: stub }\n", testRubric)
	setStub(t, "rows", &stubOut{next: "1", rows: []api.InputRow{
		{Headers: []string{"Email", "Title"}, Columns: map[string]string{"Email": "first@example.com", "Title": "Head of Data"}},
		{Headers: []string{"Email", "Title"}, Columns: map[string]string{"Email": "second@example.com", "Title": "Head of Data"}},
	}})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	err := in.store().Commit(context.Background(), []api.TableWrite{{Table: model.TableOverrides, Op: api.OpAppend,
		Rows: []api.Row{{"person": "first@example.com", "action": "same_as", "value": "second@example.com", "note": ""}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	first, second := leadOf(in, "first@example.com"), leadOf(in, "second@example.com")
	var absorbed, live string
	for _, r := range in.rows(model.TablePeople) {
		if r["merged_into"] != "" {
			absorbed, live = r["lead_id"], r["merged_into"]
		}
	}
	if absorbed == "" || (absorbed != first && absorbed != second) {
		t.Fatalf("no merge happened: %v", in.rows(model.TablePeople))
	}
	absorbedEmail := "first@example.com"
	if absorbed == second {
		absorbedEmail = "second@example.com"
	}

	in.appendEvents(replyRaw("email_unsubscribed", strings.ToUpper(absorbedEmail), "", time.Now()))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if o := outcomeOf(in, absorbed); o["unsubscribed_at"] == "" || o["unsubscribed_origin"] != "event" {
		t.Errorf("the key owner's outcome %v", o)
	}
	if o := outcomeOf(in, live); o["status"] != "unsubscribed" {
		t.Errorf("live lead outcome %v, want unsubscribed through the family", o)
	}
	if n := len(in.rows(model.TablePeople)); n != 2 {
		t.Errorf("%d leads after the opt-out; it must not create one", n)
	}
	for _, r := range in.rows(model.TablePeople) {
		if r["lead_id"] == absorbed && r["apollo_held_at"] == "" {
			t.Error("an unsubscribe event makes its key owner Apollo-held (read across the family)")
		}
	}
}

// An opt-out for a person no input has named yet creates the lead under
// `receiver`, so the later CSV row merges into an already opted-out lead.
func TestOptOutBeforeTheRowCreatesTheLead(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: rows, type: stub }\n", testRubric)
	setStub(t, "rows", &stubOut{next: "1"})
	in.appendEvents(replyRaw("email_unsubscribed", "new@example.com", "", time.Now()))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	lead := leadOf(in, "new@example.com")
	if lead == "" || outcomeOf(in, lead)["unsubscribed_at"] == "" {
		t.Fatalf("lead %q outcome %v", lead, outcomeOf(in, lead))
	}
	setStub(t, "rows", &stubOut{next: "2", rows: []api.InputRow{{Headers: []string{"email"}, Columns: map[string]string{"email": "new@example.com"}}}})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if n := len(in.rows(model.TablePeople)); n != 1 || leadOf(in, "new@example.com") != lead {
		t.Errorf("the later row did not merge into the opted-out lead: %d leads", n)
	}
}

// Stub poller, registered for sink type testpoll.
var testPoll struct {
	sync.Mutex
	calls  []time.Time
	events []api.Event
	err    error
}

type testPoller struct{}

func (testPoller) Poll(_ context.Context, since time.Time) ([]api.Event, error) {
	testPoll.Lock()
	defer testPoll.Unlock()
	testPoll.calls = append(testPoll.calls, since)
	return testPoll.events, testPoll.err
}

func init() {
	api.RegisterPoller("testpoll", func(api.Config) (api.Poller, error) { return testPoller{}, nil })
}

func setPoll(t *testing.T, evs []api.Event, err error) {
	testPoll.Lock()
	testPoll.calls, testPoll.events, testPoll.err = nil, evs, err
	testPoll.Unlock()
	t.Cleanup(func() {
		testPoll.Lock()
		testPoll.calls, testPoll.events, testPoll.err = nil, nil, nil
		testPoll.Unlock()
	})
}

func pollCalls() []time.Time {
	testPoll.Lock()
	defer testPoll.Unlock()
	return append([]time.Time(nil), testPoll.calls...)
}

// With replies by polling: the poll runs every six hours from the right
// window start, last_poll_at moves only on success, a receiver reply is keyed
// with no effect, and polled labels apply.
func TestPolling(t *testing.T) {
	in := newInstall(t, "replies: polling\npolling: { sequence_length: 30d, window_margin: 7d }\nsinks: { testpoll: {} }\n", eventsRubric)
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(_ *api.RunOptions, s *settings) { s.now = func() time.Time { return clock } }

	// A failing poll does not move last_poll_at and makes the run unhealthy.
	setPoll(t, nil, errors.New("vendor down for x@secret.example"))
	res, _, err := in.run(DefaultHooks(), at)
	if err != nil || res.Healthy || !hasKey(res.Problems, "poll_failed:testpoll") || in.state("last_poll_at") != "" {
		t.Fatalf("%+v %v last_poll_at %q", res, err, in.state("last_poll_at"))
	}
	if v := in.health()["problem:poll_failed:testpoll"]; v == "" || strings.Contains(v, "x@secret.example") {
		t.Errorf("poll error in Health must be redacted: %q", v)
	}
	if c := pollCalls(); len(c) != 1 || !c[0].Equal(clock.Add(-37*24*time.Hour)) {
		t.Errorf("first poll since %v, want now - 37d", c)
	}

	// A receiver reply is keyed but has no effect; the polled one applies.
	in.appendEvents(replyRaw("email_replied_positive", "ana@example.com", "Interested", clock),
		replyRaw("email_replied", "bo@example.com", "Replied", clock))
	// A poller that sets its own origin still reads as polling, so its
	// opt-out is applied, not refused.
	setPoll(t, []api.Event{{Kind: "reply", Email: "Ana@Example.com", At: clock, ReceivedAt: clock,
		Attrs: map[string]string{"label": "not_interested", "message_id": "m1"}},
		{Kind: "reply", Email: "cy@example.com", At: clock, ReceivedAt: clock, Origin: "apollo",
			Attrs: map[string]string{"label": "unsubscribe", "message_id": "m2"}}}, nil)
	clock = clock.Add(time.Hour)
	if res, out, err := in.run(DefaultHooks(), at); err != nil || !res.Healthy {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if in.state("last_poll_at") != model.FormatTime(clock) {
		t.Errorf("last_poll_at %q", in.state("last_poll_at"))
	}
	o := outcomeOf(in, leadOf(in, "ana@example.com"))
	if o["reply_status"] != "replied_negative" {
		t.Errorf("outcome %v: the receiver's positive reply must not count under polling", o)
	}
	if n := len(in.rows(model.TableSeenEvents)); n != 4 {
		t.Errorf("Seen events %d, want two receiver replies' keys and two polled ones", n)
	}
	if leadOf(in, "bo@example.com") != "" {
		t.Error("a receiver reply under polling merged its row: it must have no effect at all")
	}
	if o := outcomeOf(in, leadOf(in, "cy@example.com")); o["unsubscribed_at"] == "" {
		t.Errorf("a polled opt-out from a poller with its own origin was lost: %v", o)
	}
	for _, w := range in.rows(model.TableWindowEvents) {
		if w["kind"] == "replied_positive" {
			t.Error("a receiver reply under polling must have no window row")
		}
	}

	// Within six hours: no poll. After: since is last_poll_at - margin when earlier.
	setPoll(t, nil, nil)
	last := clock
	clock = clock.Add(5 * time.Hour)
	_, _, _ = in.run(DefaultHooks(), at)
	if len(pollCalls()) != 0 {
		t.Error("polled again within six hours")
	}
	clock = last.Add(40 * 24 * time.Hour)
	_, _, _ = in.run(DefaultHooks(), at)
	if c := pollCalls(); len(c) != 1 || !c[0].Equal(last.Add(-7*24*time.Hour)) {
		t.Errorf("poll after an outage since %v, want last_poll_at - 7d", c)
	}

	// A dry run never polls.
	clock = clock.Add(7 * time.Hour)
	setPoll(t, nil, nil)
	_, _, _ = in.run(DefaultHooks(), at, dry)
	if len(pollCalls()) != 0 {
		t.Error("a dry run polled")
	}
}

// A shrunk event log: the run still takes the sources' events, saves no
// event cursor, scores, and does not push.
func TestEventsShrankKeepsSourceEvents(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: site, type: stub, events: true }\n", eventsRubric)
	if err := in.store().Commit(context.Background(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "cursor:events", "value": "50"}}}}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour)
	in.appendEvents(visitRaw("lee@example.net", ago(time.Hour), old))
	setStub(t, "site", &stubOut{next: "s1", events: []api.Event{{Kind: "visit_site", Email: "ana@example.com", At: time.Now().Add(-time.Hour)}}})
	res, out, err := in.run(DefaultHooks())
	if err != nil || !hasKey(res.Problems, "events_shrank") {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if left, _, _ := in.store().ReadEvents(context.Background(), ""); len(left) != 1 {
		t.Error("a run whose event log shrank deleted processed events")
	}
	if in.state("cursor:events") != "50" {
		t.Errorf("cursor moved to %q", in.state("cursor:events"))
	}
	if leadOf(in, "ana@example.com") == "" || len(in.rows(model.TableWindowEvents)) != 1 {
		t.Error("the source's event must still be taken")
	}
}

// Processed events past the window are deleted after phase 2; Window events
// and Seen events are trimmed to their retention in phase 2.
func TestDeletionAndTrims(t *testing.T) {
	in := newInstall(t, "", eventsRubric)
	old := time.Now().Add(-100 * 24 * time.Hour)
	in.appendEvents(visitRaw("lee@example.net", old.Format(time.RFC3339), old), visitRaw("lee@example.net", ago(time.Hour), time.Now()))
	err := in.store().Commit(context.Background(), []api.TableWrite{
		{Table: model.TableWindowEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "stale", "subject": "company", "domain": "example.org",
			"kind": "visit_x", "at": model.FormatTime(old), "attrs": "{}"}}},
		{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "ancient",
			"first_received_at": model.FormatTime(time.Now().Add(-400 * 24 * time.Hour)), "run_id": "r0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, out, err := in.run(DefaultHooks()); err != nil || !res.Healthy {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	left, _, err := in.store().ReadEvents(context.Background(), "")
	if err != nil || len(left) != 1 || left[0].Seq != "2" {
		t.Errorf("events left %+v %v: want only the recent one", left, err)
	}
	for _, w := range in.rows(model.TableWindowEvents) {
		if w["event_key"] == "stale" {
			t.Error("a window row older than 90 days was kept")
		}
	}
	keys := map[string]bool{}
	for _, s := range in.rows(model.TableSeenEvents) {
		keys[s["event_key"]] = true
	}
	if keys["ancient"] || len(keys) != 2 {
		t.Errorf("Seen events %v: want the two visits' keys, not the year-old one", keys)
	}
	// The old visit still counts as first seen, though it is past every window.
	if n := len(in.rows(model.TableWindowEvents)); n != 1 {
		t.Errorf("Window events %d, want only the recent visit", n)
	}
}

// A source row that cannot be an event is logged once, not every run; a
// vendor-only kind from a plug-in source is refused.
func TestRejectedSourceEventsAreLoggedOnce(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: site, type: stub, events: true }\n", eventsRubric)
	setStub(t, "site", &stubOut{next: "s1", events: []api.Event{
		{Origin: "", Attrs: map[string]string{"reject": "line 3: no event kind"}},
		{Kind: "unsubscribed", Email: "victim@example.com", At: time.Now()},
	}})
	for i := 0; i < 2; i++ {
		if _, out, err := in.run(DefaultHooks()); err != nil {
			t.Fatal(err, out)
		}
	}
	n := 0
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "row_rejected" {
			n++
			if strings.Contains(r["message"], "victim@") {
				t.Errorf("a log line carries an email: %q", r["message"])
			}
		}
	}
	if n != 2 {
		t.Errorf("row_rejected logged %d times over two runs, want once per row", n)
	}
	if leadOf(in, "victim@example.com") != "" {
		t.Error("a source claimed an opt-out and it was applied")
	}
}

// A stored body the parsers refuse is logged by sequence, without its email.
func TestRejectedReceiverBodyIsLogged(t *testing.T) {
	in := newInstall(t, "", eventsRubric)
	in.appendEvents(api.RawEvent{Kind: "apollo_reply", ReceivedAt: time.Now(),
		Body: []byte(`{"event":"email_replied","contact_email":"x@secret.example","contact_stage":"__unsubscribed__"}`)},
		api.RawEvent{Kind: "apollo_reply", ReceivedAt: time.Now(), Body: []byte(`{"event":"email_opened","contact_email":"x@secret.example"}`)})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	found := false
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "event_rejected" {
			found = true
			if !strings.Contains(r["message"], "stored event 1") || strings.Contains(r["message"], "secret.example") {
				t.Errorf("message %q", r["message"])
			}
		}
	}
	if !found {
		t.Error("no event_rejected line")
	}
	ignored := false
	for _, r := range in.rows(model.TableLog) {
		ignored = ignored || (r["kind"] == "event_ignored" && strings.Contains(r["message"], "email_opened"))
	}
	if !ignored {
		t.Error("an unacted kind must be logged as event_ignored, not dropped silently")
	}
	if in.state("cursor:events") != "2" {
		t.Error("the cursor must move past a rejected body")
	}
}

// A source can never pose as a vendor: whatever Origin it sets, its events
// keep its own id, and a vendor-only kind from it is refused.
func TestSourcesCannotPoseAsVendors(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: site, type: stub, events: true }\n", eventsRubric)
	setStub(t, "site", &stubOut{next: "s1", events: []api.Event{
		{Kind: "unsubscribed", Email: "a@example.com", At: time.Now(), Origin: "polling"},
		{Kind: "replied_positive", Email: "b@example.com", At: time.Now(), Origin: "receiver"},
		{Kind: "reply", Email: "c@example.com", At: time.Now(), Origin: "polling", Attrs: map[string]string{"label": "unsubscribe", "message_id": "m"}},
	}})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if n := len(in.rows(model.TablePeople)) + len(in.rows(model.TableOutcomes)); n != 0 {
		t.Errorf("a source's vendor-only events were applied: %v %v", in.rows(model.TablePeople), in.rows(model.TableOutcomes))
	}
	n := 0
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "row_rejected" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("row_rejected %d times, want 3", n)
	}
}

// The body-hash fallback: a visit with no visited_at is keyed by person and
// received day, so repeat visits on different days count and a redelivery the
// same day does not.
func TestVisitsWithNoTimeCountPerDay(t *testing.T) {
	in := newInstall(t, "", eventsRubric)
	body := func(received time.Time) api.RawEvent {
		return api.RawEvent{Kind: "apollo_visit", ReceivedAt: received, Body: []byte(
			`{"event":"website_visited_pricing","contact":{"email":"lee@example.net"},"account":{"domain":"example.net"}}`)}
	}
	now := time.Now().UTC()
	in.appendEvents(body(now.Add(-50*time.Hour)), body(now.Add(-26*time.Hour)), body(now.Add(-time.Minute)), body(now.Add(-time.Minute)))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if n := len(in.rows(model.TableWindowEvents)); n != 3 {
		t.Errorf("Window events %d, want one per day", n)
	}
	if got := ranked(in)["lee@example.net"]["hot_lead"]; got != "hot" {
		t.Errorf("three visits on three days must fire hot, got %q", got)
	}
}

// The reviewer's probe, end to end: lead A is known only by LinkedIn; an
// unsubscribe for an unknown email carrying A's URL opts A out too.
func TestOptOutReachesALinkedInOnlyLead(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: rows, type: stub }\n", testRubric)
	setStub(t, "rows", &stubOut{next: "1", rows: []api.InputRow{{Headers: []string{"linkedin_url", "full_name"},
		Columns: map[string]string{"linkedin_url": "https://www.linkedin.com/in/ana", "full_name": "Ana"}}}})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	a := leadOf(in, "linkedin.com/in/ana")
	in.appendEvents(api.RawEvent{Kind: "apollo_reply", ReceivedAt: time.Now(), Body: []byte(
		`{"event":"email_unsubscribed","contact_email":"ana@example.com","contact_linkedin_url":"https://www.linkedin.com/in/ana"}`)})
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	if o := outcomeOf(in, a); a == "" || o["unsubscribed_at"] == "" {
		t.Errorf("LinkedIn-only lead %q not opted out: %v", a, o)
	}
}

// deleteProcessed's cursor path, with a store that drops a partition: the new
// cursor is committed (retried once), and a second failure names the value to
// recover with.
type fakeLog struct {
	api.EventLog
	next api.Cursor
}

func (f fakeLog) DeleteProcessed(context.Context, api.Cursor, time.Time) (api.Cursor, error) {
	return f.next, nil
}

type fakeStore struct {
	api.Backend
	fails   int
	commits [][]api.TableWrite
}

func (f *fakeStore) Commit(_ context.Context, w []api.TableWrite) error {
	f.commits = append(f.commits, w)
	if f.fails > 0 {
		f.fails--
		return errors.New("injected")
	}
	return nil
}

type okLease struct{}

func (okLease) Check(context.Context) error   { return nil }
func (okLease) Release(context.Context) error { return nil }

func TestDeleteProcessedCommitsAChangedCursor(t *testing.T) {
	for _, fails := range []int{1, 2} {
		m := model.New()
		m.SetState(eventsCursorKey, "2026-07:40,2026-08:12")
		m.Committed(m.Writes())
		st := &fakeStore{fails: fails}
		r := &Run{Ctx: context.Background(), Model: m, Store: st, Events: fakeLog{next: "2026-08:12"}, Lease: okLease{}, Now: time.Now}
		err := deleteProcessed(r)
		if len(st.commits) != 2 {
			t.Errorf("fails %d: %d commit attempts, want 2 (one retry)", fails, len(st.commits))
		}
		if fails == 1 && err != nil {
			t.Errorf("one failure must be retried: %v", err)
		}
		if fails == 2 && (err == nil || !strings.Contains(err.Error(), "2026-08:12")) {
			t.Errorf("two failures must name the cursor to recover with: %v", err)
		}
		if w := st.commits[0]; len(w) != 1 || w[0].Table != model.TableState || w[0].Rows[0]["value"] != "2026-08:12" {
			t.Errorf("committed %+v", w)
		}
	}
	// A run whose event log shrank deletes nothing.
	r := &Run{Ctx: context.Background(), Model: model.New(), EventsShrank: true, Events: fakeLog{next: "x"}}
	r.Model.SetState(eventsCursorKey, "5")
	if err := deleteProcessed(r); err != nil {
		t.Error(err)
	}
}
