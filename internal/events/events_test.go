package events

import (
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

var t0 = time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

func replyBody(event, email, stage string) api.RawEvent {
	return api.RawEvent{Seq: "1", Kind: "apollo_reply", ReceivedAt: t0, Body: []byte(
		`{"event":"` + event + `","contact_email":"` + email + `","contact_stage":"` + stage + `",` +
			`"last_conversation_link":"https://app.apollo.io/#/conv/9"}`)}
}

func key(t *testing.T, r api.RawEvent) api.EventID {
	t.Helper()
	evs, _ := Parse([]api.RawEvent{r})
	if len(evs) != 1 || evs[0].Kind == "" {
		t.Fatalf("parse: %+v", evs)
	}
	return Key(merge.NormalizeEventKeys(evs[0]))
}

// A redelivery of the same state is one key; a stage that moves is a new one.
func TestTheKeyMovesWithTheStage(t *testing.T) {
	first := key(t, replyBody("email_replied", "ada@example.com", "Replied"))
	again := replyBody("email_replied", "ada@example.com", "Replied")
	again.ReceivedAt = t0.Add(time.Hour) // a redelivery arrives later
	if key(t, again) != first {
		t.Error("a redelivery of the same state must key the same")
	}
	if key(t, replyBody("email_replied", "ada@example.com", "Meeting Booked")) == first {
		t.Error("a stage change is a new event")
	}
}

func TestKindsDoNotShareAKey(t *testing.T) {
	plain := key(t, replyBody("email_replied", "ada@example.com", "Replied"))
	positive := key(t, replyBody("email_replied_positive", "ada@example.com", "Replied"))
	sent := key(t, replyBody("email_sent", "ada@example.com", "Replied"))
	if plain == positive || plain == sent || positive == sent {
		t.Errorf("kinds share a key: %s %s %s", plain, positive, sent)
	}
}

// Free text is length-prefixed: a link and a stage that both hold colons can
// never flatten onto one key.
func TestReplyKeyIsLengthPrefixed(t *testing.T) {
	a := Key(api.Event{Kind: "replied", Origin: "receiver", Attrs: map[string]string{"conversation_link": "a", "stage": "b|1:c"}})
	b := Key(api.Event{Kind: "replied", Origin: "receiver", Attrs: map[string]string{"conversation_link": "a|1:b", "stage": "c"}})
	if a == b {
		t.Error("two events flattened to one key")
	}
}

func visitBody(body string, received time.Time) api.RawEvent {
	return api.RawEvent{Seq: "1", Kind: "apollo_visit", ReceivedAt: received, Body: []byte(body)}
}

// A visit keys on the person (contact id first, so a corrected email keeps one
// key), the kind and the vendor's visit time.
func TestVisitKeys(t *testing.T) {
	withTime := func(id, email, at string) string {
		return `{"event":"website_visited_site","visited_at":"` + at + `","contact":{"id":"` + id + `","email":"` + email + `"},"account":{"domain":"example.com"}}`
	}
	a := key(t, visitBody(withTime("ct-1", "old@example.com", "2026-08-20T10:00:00Z"), t0))
	if key(t, visitBody(withTime("ct-1", "new@example.com", "2026-08-20T10:00:00Z"), t0.Add(time.Hour))) != a {
		t.Error("a corrected email on the same contact and visit must key the same")
	}
	if key(t, visitBody(withTime("ct-1", "old@example.com", "2026-08-20T11:00:00Z"), t0)) == a {
		t.Error("a second visit by the same person is a new event")
	}
	if key(t, visitBody(withTime("ct-2", "other@example.com", "2026-08-20T10:00:00Z"), t0)) == a {
		t.Error("two people visiting at one time are two events")
	}

	// No usable visited_at: the body hash keys it, so a redelivered body is one
	// event and a different body is another.
	noTime := `{"event":"website_visited_site","contact":{"email":"ada@example.com"}}`
	h := key(t, visitBody(noTime, t0))
	if key(t, visitBody(noTime, t0.Add(time.Hour))) != h {
		t.Error("a redelivered body with no visit time must key the same")
	}
	if key(t, visitBody(`{"event":"website_visited_site","contact":{"email":"ada@example.com","title":"CTO"}}`, t0)) == h {
		t.Error("a different body is a different event")
	}

	// A company-only visit keys on the employer domain and time.
	co := `{"event":"website_visited_site","visited_at":"2026-08-20T10:00:00Z","contact":{},"account":{"domain":"example.com"}}`
	co2 := `{"event":"website_visited_site","visited_at":"2026-08-20T10:00:00Z","contact":{},"account":{"domain":"example.org"}}`
	if key(t, visitBody(co, t0)) == key(t, visitBody(co2, t0)) {
		t.Error("company-only visits at two companies share a key")
	}
}

func TestSourceAndPolledKeys(t *testing.T) {
	src := func(origin, email string, at time.Time) api.EventID {
		return Key(api.Event{Kind: "visit_csv", Origin: origin, Email: email, At: at})
	}
	if src("site", "a@example.com", t0) != src("site", "a@example.com", t0) {
		t.Error("the same source row must key the same every run")
	}
	if src("site", "a@example.com", t0) == src("other", "a@example.com", t0) ||
		src("site", "a@example.com", t0) == src("site", "b@example.com", t0) ||
		src("site", "a@example.com", t0) == src("site", "a@example.com", t0.Add(time.Second)) {
		t.Error("source, person and time each make a different key")
	}
	p := func(id, label string) api.EventID {
		return Key(api.Event{Kind: "reply", Origin: "polling", Attrs: map[string]string{"message_id": id, "label": label}})
	}
	if p("m1", "willing_to_meet") != p("m1", "willing_to_meet") || p("m1", "willing_to_meet") == p("m1", "not_interested") {
		t.Error("a polled reply keys on message id plus label")
	}
}

// Parse never drops a body silently: a body it cannot apply comes back as a
// rejected event naming its sequence.
func TestParseRejectsVisibly(t *testing.T) {
	evs, rows := Parse([]api.RawEvent{{Seq: "41", Kind: "apollo_reply", ReceivedAt: t0, Body: []byte(`{"event":"email_replied"}`)}})
	if len(evs) != 1 || evs[0].Kind != "" || evs[0].Attrs[AttrReject] == "" || len(rows) != 0 {
		t.Fatalf("%+v %+v", evs, rows)
	}
}

// --- Apply ---

func newModel(t *testing.T, ids ...string) *model.Model {
	t.Helper()
	m := model.New()
	for _, id := range ids {
		m.Put(model.TablePeople, model.Person{LeadID: api.LeadID(id), CreatedAt: t0})
	}
	return m
}

func outcome(m *model.Model, id string) model.Outcome { return m.Outcomes[model.Key(id)] }

func TestApplySetsEachEffect(t *testing.T) {
	m := newModel(t, "L1")
	Apply(m, "L1", api.Event{Kind: "sent", At: t0, ReceivedAt: t0}, nil)
	if o := outcome(m, "L1"); !o.ContactedAt.Equal(t0) {
		t.Errorf("sent: %+v", o)
	}
	if !m.People["L1"].ApolloHeldAt.Equal(t0) {
		t.Error("sent must set apollo_held_at")
	}
	Apply(m, "L1", api.Event{Kind: "replied", At: t0.Add(time.Hour), ReceivedAt: t0.Add(time.Hour)}, nil)
	if o := outcome(m, "L1"); o.ReplyStatus != "replied_neutral" {
		t.Errorf("replied: %+v", o)
	}
	Apply(m, "L1", api.Event{Kind: "replied_positive", At: t0.Add(2 * time.Hour), ReceivedAt: t0.Add(2 * time.Hour)}, nil)
	if o := outcome(m, "L1"); o.ReplyStatus != "replied_positive" {
		t.Errorf("replied_positive: %+v", o)
	}
	// An older reply delivered late does not replace the latest one.
	Apply(m, "L1", api.Event{Kind: "replied", At: t0.Add(30 * time.Minute), ReceivedAt: t0.Add(30 * time.Minute)}, nil)
	if o := outcome(m, "L1"); o.ReplyStatus != "replied_positive" {
		t.Errorf("a late older reply replaced the latest: %+v", o)
	}
	// A visit has no outcome effect and does not make the lead Apollo-held.
	m2 := newModel(t, "L2")
	if Apply(m2, "L2", api.Event{Kind: "visit_site", At: t0}, nil) || !m2.People["L2"].ApolloHeldAt.IsZero() {
		t.Error("a visit must change nothing")
	}
}

// The opt-out time is kept at its earliest, and an automated opt-out always
// takes the origin, even over manual. Applying twice changes nothing.
func TestOptOutEarliestAndOrigin(t *testing.T) {
	m := newModel(t, "L1")
	m.Put(model.TableOutcomes, model.Outcome{LeadID: "L1", UnsubscribedAt: t0, UnsubscribedOrigin: "manual"})
	Apply(m, "L1", api.Event{Kind: "unsubscribed", At: t0.Add(time.Hour)}, nil)
	o := outcome(m, "L1")
	if !o.UnsubscribedAt.Equal(t0) || o.UnsubscribedOrigin != UnsubEvent {
		t.Errorf("%+v", o)
	}
	Apply(m, "L1", api.Event{Kind: "optout", At: t0.Add(-time.Hour), Origin: OriginApolloLookup}, nil)
	o = outcome(m, "L1")
	if !o.UnsubscribedAt.Equal(t0.Add(-time.Hour)) || o.UnsubscribedOrigin != UnsubLookup {
		t.Errorf("%+v", o)
	}
	if Apply(m, "L1", api.Event{Kind: "optout", At: t0.Add(-time.Hour), Origin: OriginApolloLookup}, nil) {
		t.Error("applying the same opt-out twice must change nothing")
	}
	m2 := newModel(t, "L2")
	Apply(m2, "L2", api.Event{Kind: "optout", At: t0, Origin: OriginApolloLookup}, nil)
	if !m2.People["L2"].ApolloHeldAt.IsZero() {
		t.Error("a lookup opt-out is not an Apollo engagement")
	}
}

// An opt-out for a lead that was merged away lands on the live lead, and an
// event resolved by a merged lead's email does too.
func TestOptOutFollowsMergedInto(t *testing.T) {
	m := newModel(t, "LIVE")
	m.Put(model.TablePeople, model.Person{LeadID: "OLD", CreatedAt: t0.Add(time.Hour), MergedInto: "LIVE"})
	m.Put(model.TableIdentities, model.Identity{Key: "old@example.com", Kind: "email", LeadID: "OLD", SourceID: "csv", FirstSeenAt: t0})

	e := merge.NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: " Old@Example.com ", At: t0, Origin: OriginReceiver})
	lead := merge.ApplyEventPerson(m, e)
	if lead != "LIVE" {
		t.Fatalf("resolved %q, want the live lead", lead)
	}
	Apply(m, lead, e, nil)
	if o := outcome(m, "LIVE"); o.UnsubscribedAt.IsZero() {
		t.Errorf("opt-out did not land on the live lead: %+v", o)
	}
	// Called with the absorbed lead's id, it still writes the live lead.
	m2 := newModel(t, "LIVE")
	m2.Put(model.TablePeople, model.Person{LeadID: "OLD", CreatedAt: t0, MergedInto: "LIVE"})
	Apply(m2, "OLD", api.Event{Kind: "optout", At: t0}, nil)
	if outcome(m2, "LIVE").UnsubscribedAt.IsZero() || !outcome(m2, "OLD").UnsubscribedAt.IsZero() {
		t.Error("Apply must write the live lead, not the absorbed one")
	}
}

func TestPolledReplyLabels(t *testing.T) {
	cases := []struct {
		label     string
		overrides map[string]string
		reply     string
		unsub     bool
	}{
		{"willing_to_meet", nil, "replied_positive", false},
		{"not_interested", nil, "replied_negative", false},
		{"person_referral", nil, "replied_neutral", false},
		{"out_of_office", nil, "", false},
		{"", nil, "replied_unlabelled", false},
		{"unsubscribe", nil, "", true},
		{"unsubscribe", map[string]string{"unsubscribe": "replied_positive"}, "", true}, // fixed
		{"not_interested", map[string]string{"not_interested": "replied_neutral"}, "replied_neutral", false},
		{"", map[string]string{"_unlabelled": "none"}, "", false},
		{"some_new_label", nil, "replied_unlabelled", false},
	}
	for _, c := range cases {
		m := newModel(t, "L1")
		Apply(m, "L1", api.Event{Kind: "reply", At: t0, ReceivedAt: t0, Origin: OriginPolling,
			Attrs: map[string]string{"label": c.label, "message_id": "m1"}}, c.overrides)
		o := outcome(m, "L1")
		if o.ReplyStatus != c.reply || o.UnsubscribedAt.IsZero() == c.unsub {
			t.Errorf("label %q %v: %+v", c.label, c.overrides, o)
		}
		if c.unsub && o.UnsubscribedOrigin != UnsubEvent {
			t.Errorf("a polled unsubscribe has origin event: %+v", o)
		}
		if m.People["L1"].ApolloHeldAt.IsZero() {
			t.Errorf("label %q: a polled reply sets apollo_held_at", c.label)
		}
	}
}

// A deal event reaches every live lead at the company and every lead whose
// stored deal is the same one.
func TestDealFanOut(t *testing.T) {
	m := model.New()
	at := func(id, domain, merged string) {
		m.Put(model.TablePeople, model.Person{LeadID: api.LeadID(id), CreatedAt: t0, MergedInto: api.LeadID(merged),
			Fields: map[string]model.Field{model.CompanyDomainField: {Value: domain, SourceID: "csv", At: t0}}})
	}
	at("A", "example.com", "")
	at("B", "example.com", "")
	at("C", "example.com", "A") // merged away: reached through A
	at("D", "example.org", "")  // elsewhere, but holds the deal
	at("E", "example.org", "")
	m.Put(model.TableOutcomes, model.Outcome{LeadID: "D", DealID: "deal-1", DealStage: "open"})

	Apply(m, "", api.Event{Kind: "deal_won", Domain: "example.com", At: t0, Origin: OriginHubSpot,
		Attrs: map[string]string{"deal_id": "deal-1", "stage": "closedwon"}}, nil)
	for _, id := range []string{"A", "B", "D"} {
		if o := outcome(m, id); o.DealID != "deal-1" || o.DealStage != "won" || !o.DealCheckedAt.Equal(t0) {
			t.Errorf("%s: %+v", id, o)
		}
	}
	for _, id := range []string{"C", "E"} {
		if o := outcome(m, id); o.DealID != "" {
			t.Errorf("%s must not get the deal: %+v", id, o)
		}
	}
}
