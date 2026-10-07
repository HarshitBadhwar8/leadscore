package events

import (
	"fmt"
	"reflect"
	"strings"
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
	if key(t, visitBody(withTime("ct-1", "old@example.com", "2026-08-20T11:00:00Z"), t0.Add(2*time.Hour))) == a {
		t.Error("a second visit by the same person is a new event")
	}
	if key(t, visitBody(withTime("ct-2", "other@example.com", "2026-08-20T10:00:00Z"), t0)) == a {
		t.Error("two people visiting at one time are two events")
	}

	// No usable visited_at: person, page and received day key it, so the same
	// body twice in a day is one event and on another day a new one.
	noTime := `{"event":"website_visited_site","contact":{"email":"ada@example.com"}}`
	h := key(t, visitBody(noTime, t0))
	if key(t, visitBody(noTime, t0.Add(time.Hour))) != h {
		t.Error("the same body twice in one day must key the same")
	}
	if key(t, visitBody(noTime, t0.Add(24*time.Hour))) == h {
		t.Error("the same body on another day is a new visit")
	}
	if key(t, visitBody(`{"event":"website_visited_site","contact":{"email":"bo@example.com"}}`, t0)) == h {
		t.Error("another person the same day is a different event")
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
	Apply(m2, "L2", api.Event{Kind: "visit_site", At: t0}, nil)
	if len(m2.Outcomes) != 0 || !m2.People["L2"].ApolloHeldAt.IsZero() {
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
	before := len(codecWrites(m))
	Apply(m, "L1", api.Event{Kind: "optout", At: t0.Add(-time.Hour), Origin: OriginApolloLookup}, nil)
	if !reflect.DeepEqual(outcome(m, "L1"), o) || len(codecWrites(m)) != before {
		t.Error("applying the same opt-out twice must change nothing")
	}
	m2 := newModel(t, "L2")
	Apply(m2, "L2", api.Event{Kind: "optout", At: t0, Origin: OriginApolloLookup}, nil)
	if !m2.People["L2"].ApolloHeldAt.IsZero() {
		t.Error("a lookup opt-out is not an Apollo engagement")
	}
}

// An opt-out for an email of a lead that was merged away is stored on that
// lead, the key's owner (the fold reads it across the family); an opt-out
// whose keys name no lead lands on the resolved live lead.
func TestOptOutFollowsMergedInto(t *testing.T) {
	m := newModel(t, "LIVE")
	m.Put(model.TablePeople, model.Person{LeadID: "OLD", CreatedAt: t0.Add(time.Hour), MergedInto: "LIVE"})
	m.Put(model.TableIdentities, model.Identity{Key: "old@example.com", Kind: "email", LeadID: "OLD", SourceID: "csv", FirstSeenAt: t0})

	e := merge.NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: " Old@Example.com ", At: t0, Origin: OriginReceiver})
	lead := merge.ApplyEventPerson(m, e)
	if lead != "LIVE" {
		t.Fatalf("resolved %q, want the live lead", lead)
	}
	if changed := Apply(m, lead, e, nil); len(changed) != 1 || changed[0] != "LIVE" {
		t.Errorf("changed %v, want the live lead", changed)
	}
	if o := outcome(m, "OLD"); o.UnsubscribedAt.IsZero() {
		t.Errorf("opt-out did not land on the key's owner: %+v", o)
	}
	if o := outcome(m, "LIVE"); !o.UnsubscribedAt.IsZero() {
		t.Errorf("opt-out copied onto the survivor: %+v", o)
	}
	// Called with the absorbed lead's id, it still writes the live lead only:
	// the fold reads opt-outs across the family, and an un-merge must leave
	// the other person contactable.
	m2 := newModel(t, "LIVE")
	m2.Put(model.TablePeople, model.Person{LeadID: "OLD", CreatedAt: t0, MergedInto: "LIVE"})
	changed := Apply(m2, "OLD", api.Event{Kind: "optout", At: t0}, nil)
	if outcome(m2, "LIVE").UnsubscribedAt.IsZero() || !outcome(m2, "OLD").UnsubscribedAt.IsZero() {
		t.Error("Apply must write the live lead, not the absorbed one")
	}
	if len(changed) != 1 || changed[0] != "LIVE" {
		t.Errorf("changed %v, want the live lead", changed)
	}
	if again := Apply(m2, "OLD", api.Event{Kind: "optout", At: t0}, nil); len(again) != 0 {
		t.Errorf("a repeat changed %v", again)
	}
}

// In a hand-edited merged_into cycle, an opt-out reaching one member is
// written on every member, so it survives a person fixing the cycle by hand.
func TestOptOutReachesEveryCycleMember(t *testing.T) {
	m := newModel(t)
	m.Put(model.TablePeople, model.Person{LeadID: "A", CreatedAt: t0, MergedInto: "B"})
	m.Put(model.TablePeople, model.Person{LeadID: "B", CreatedAt: t0, MergedInto: "C"})
	m.Put(model.TablePeople, model.Person{LeadID: "C", CreatedAt: t0, MergedInto: "A"})
	m.Put(model.TablePeople, model.Person{LeadID: "D", CreatedAt: t0, MergedInto: "B"}) // merged into a member
	m.Put(model.TableIdentities, model.Identity{Key: "c@example.com", Kind: "email", LeadID: "C", SourceID: "csv", FirstSeenAt: t0})
	e := merge.NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: "c@example.com", At: t0, Origin: OriginReceiver})
	lead, ok := merge.FindPerson(m, e)
	if !ok || lead != "A" {
		t.Fatalf("resolved %q, want the cycle's lowest id", lead)
	}
	Apply(m, lead, e, nil)
	for _, id := range []api.LeadID{"A", "B", "C", "D"} {
		if o := outcome(m, string(id)); o.UnsubscribedAt.IsZero() || o.UnsubscribedOrigin != UnsubEvent {
			t.Errorf("%s: %+v, want the opt-out", id, o)
		}
	}
	// A person then fixes the cycle: every former member stays opted out.
	for _, id := range []api.LeadID{"A", "B", "C", "D"} {
		p := m.People[model.Key(id)]
		p.MergedInto = ""
		m.Put(model.TablePeople, p)
	}
	for _, id := range []api.LeadID{"A", "B", "C", "D"} {
		if outcome(m, string(id)).UnsubscribedAt.IsZero() {
			t.Errorf("%s lost the opt-out once live again", id)
		}
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

func codecWrites(m *model.Model) []api.TableWrite { return m.Writes(model.TableOutcomes) }

// A stage typed in another case or with spaces is the same state.
func TestStageTextIsNormalizedInTheKey(t *testing.T) {
	if key(t, replyBody("email_replied", "ada@example.com", "Replied")) != key(t, replyBody("email_replied", "ada@example.com", " REPLIED ")) {
		t.Error("a stage differing only in case or spaces made a new event")
	}
}

// Two people's polled replies with no message id never share a key, so b's
// opt-out is not dropped as a repeat of a's.
func TestPolledReplyWithoutMessageIDKeysThePerson(t *testing.T) {
	ev := func(email string) api.Event {
		return api.Event{Kind: "reply", Origin: OriginPolling, Email: email, At: t0, ReceivedAt: t0, Attrs: map[string]string{"label": "unsubscribe"}}
	}
	if Key(ev("a@example.com")) == Key(ev("b@example.com")) {
		t.Fatal("two people's opt-outs share a key")
	}
	if Key(ev("a@example.com")) != Key(ev("a@example.com")) {
		t.Error("the same reply polled twice must key the same")
	}
	withLabel := func(l string) api.EventID {
		return Key(api.Event{Kind: "reply", Origin: OriginPolling, Attrs: map[string]string{"message_id": "m1", "label": l}})
	}
	if withLabel("Unsubscribe") != withLabel("unsubscribe") {
		t.Error("labels compare lowercased")
	}
}

func linkedInLead(m *model.Model, id, url string) {
	m.Put(model.TablePeople, model.Person{LeadID: api.LeadID(id), CreatedAt: t0})
	m.Put(model.TableIdentities, model.Identity{Key: url, Kind: "linkedin", LeadID: api.LeadID(id), SourceID: "csv", FirstSeenAt: t0})
}

// The reviewer's probe: lead A is known only by its LinkedIn URL. An
// unsubscribe for ana@example.com carrying that URL makes a new lead B (an
// unknown email never matches by URL), and the opt-out reaches A too.
func TestOptOutReachesTheLinkedInHolder(t *testing.T) {
	m := model.New()
	linkedInLead(m, "A", "linkedin.com/in/ana")
	e := merge.NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: "ana@example.com", LinkedInURL: "https://www.linkedin.com/in/ana",
		At: t0, ReceivedAt: t0, Origin: OriginReceiver})
	b := merge.ApplyEventPerson(m, e)
	if b == "" || b == "A" {
		t.Fatalf("resolved %q, want a new lead", b)
	}
	Apply(m, b, e, nil)
	for _, id := range []string{"A", string(b)} {
		if o := outcome(m, string(id)); o.UnsubscribedAt.IsZero() || o.UnsubscribedOrigin != UnsubEvent {
			t.Errorf("%s not opted out: %+v", id, o)
		}
	}
	logged := false
	for _, l := range m.Log {
		if l.Kind == merge.LogKeyConflict && l.LeadID == "A" && !strings.Contains(l.Message, "@") {
			logged = true
		}
	}
	if !logged {
		t.Error("no key_conflict line (ids only) for the opt-out reaching A")
	}

	// The different-email variant: the email belongs to lead C, the URL to A.
	m = model.New()
	linkedInLead(m, "A", "linkedin.com/in/ana")
	m.Put(model.TablePeople, model.Person{LeadID: "C", CreatedAt: t0})
	m.Put(model.TableIdentities, model.Identity{Key: "ana.other@example.com", Kind: "email", LeadID: "C", SourceID: "csv", FirstSeenAt: t0})
	e = merge.NormalizeEventKeys(api.Event{Kind: "optout", Email: "ana.other@example.com", LinkedInURL: "linkedin.com/in/ana", At: t0, Origin: OriginApolloLookup})
	c := merge.ApplyEventPerson(m, e)
	Apply(m, c, e, nil)
	if outcome(m, "A").UnsubscribedAt.IsZero() || outcome(m, "C").UnsubscribedAt.IsZero() {
		t.Errorf("A %+v, C %+v: both must be opted out", outcome(m, "A"), outcome(m, "C"))
	}

	// A reply or a visit stays on the resolved lead.
	m = model.New()
	linkedInLead(m, "A", "linkedin.com/in/ana")
	m.Put(model.TablePeople, model.Person{LeadID: "C", CreatedAt: t0})
	Apply(m, "C", api.Event{Kind: "replied", LinkedInURL: "linkedin.com/in/ana", At: t0, ReceivedAt: t0, Origin: OriginReceiver}, nil)
	if outcome(m, "A").ReplyStatus != "" {
		t.Error("a reply spread to another lead")
	}
}

// A deal event's domain is normalized before it finds the company's leads.
func TestDealDomainIsNormalized(t *testing.T) {
	m := model.New()
	m.Put(model.TablePeople, model.Person{LeadID: "A", CreatedAt: t0,
		Fields: map[string]model.Field{model.CompanyDomainField: {Value: "example.com", SourceID: "csv", At: t0}}})
	Apply(m, "", api.Event{Kind: "deal_open", Domain: "https://www.Example.com/", At: t0, Origin: OriginHubSpot, Attrs: map[string]string{"deal_id": "d1"}}, nil)
	if outcome(m, "A").DealID != "d1" {
		t.Errorf("%+v", outcome(m, "A"))
	}
}

// The re-check probe: two polled unsubscribes with no message id and no At,
// for contacts c1 and c2, never share a key (contact id first, received time
// as the time).
func TestPolledReplyWithoutMessageIDOrTimeKeysTheContact(t *testing.T) {
	ev := func(cid string) api.Event {
		return api.Event{Kind: "reply", Origin: OriginPolling, ReceivedAt: t0,
			Attrs: map[string]string{"label": "unsubscribe", "contact_id": cid}}
	}
	if Key(ev("c1")) == Key(ev("c2")) {
		t.Fatal("c2's opt-out shares c1's key and would be dropped")
	}
	later := ev("c1")
	later.ReceivedAt = t0.Add(time.Hour)
	if Key(later) == Key(ev("c1")) {
		t.Error("with no At, the received time must be part of the key")
	}
}

// A visit whose time is ahead of our clock is clamped for storage, but keyed
// on the time the vendor sent, so a retry received later keys the same.
func TestClampedVisitKeysOnTheSentTime(t *testing.T) {
	body := `{"event":"website_visited_site","visited_at":"2026-08-20T10:30:00Z","contact":{"email":"ada@example.com"}}`
	first := key(t, visitBody(body, t0))                  // received 10:00, clamped to 10:00
	retry := key(t, visitBody(body, t0.Add(2*time.Hour))) // received 12:00, not clamped
	if first != retry {
		t.Error("a retried delivery of a clamped visit made a new key")
	}
}

// Spreading an opt-out logs key_conflict only when it changed the holder,
// and names the case where no lead matched.
func TestOptOutSpreadLogsOnlyChanges(t *testing.T) {
	m := model.New()
	linkedInLead(m, "A", "linkedin.com/in/ana")
	e := api.Event{Kind: "optout", LinkedInURL: "linkedin.com/in/ana", At: t0, Origin: OriginApolloLookup}
	Apply(m, "", e, nil)
	Apply(m, "", e, nil)
	n := 0
	for _, l := range m.Log {
		if l.Kind == merge.LogKeyConflict {
			n++
			if strings.Contains(l.Message, "lead  ") || !strings.Contains(l.Message, "matched no lead") {
				t.Errorf("message %q", l.Message)
			}
		}
	}
	if n != 1 || outcome(m, "A").UnsubscribedAt.IsZero() {
		t.Errorf("%d key_conflict lines, want 1; A %+v", n, outcome(m, "A"))
	}
}

func TestLookupOrigin(t *testing.T) {
	for typ, want := range map[string]string{"hubspot": OriginHubSpot, "apollo": OriginApolloLookup, "crm": "lookup:crm"} {
		if got := LookupOrigin(typ); got != want {
			t.Errorf("%s: %q, want %q", typ, got, want)
		}
	}
	k := string(Key(api.Event{Kind: "optout", Email: "a@b.example", Origin: LookupOrigin("crm")}))
	if !strings.HasPrefix(k, "lookup|") {
		t.Errorf("a plug-in lookup's event is keyed as a lookup event: %q", k)
	}
}

// An opt-out on a large install costs no scan of People: only a lead that
// stands for a merge cycle looks for the other members.
func BenchmarkApplyOptOutLargeModel(b *testing.B) {
	m := model.New()
	for i := 0; i < 50000; i++ {
		id := api.LeadID(fmt.Sprintf("L%05d", i))
		m.Put(model.TablePeople, model.Person{LeadID: id, CreatedAt: t0})
		m.Put(model.TableIdentities, model.Identity{Key: fmt.Sprintf("p%05d@example.com", i), Kind: "email", LeadID: id, SourceID: "csv", FirstSeenAt: t0})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := i % 50000
		e := api.Event{Kind: "unsubscribed", Email: fmt.Sprintf("p%05d@example.com", n), At: t0, Origin: OriginReceiver}
		Apply(m, api.LeadID(fmt.Sprintf("L%05d", n)), e, nil)
	}
}

// Apollo's workflow variables carry no contact id, so a reply body names the
// person by email only. It still keys apart per person (the conversation
// link, else the email), finds the lead by email, and an unsubscribe applies.
func TestReplyBodyWithoutContactIDUsesTheEmail(t *testing.T) {
	m := newModel(t, "L1")
	m.Put(model.TableIdentities, model.Identity{Key: "ada@example.com", Kind: "email", LeadID: "L1", SourceID: "csv", FirstSeenAt: t0})
	body := func(event, email string) api.RawEvent {
		return api.RawEvent{Seq: "1", Kind: "apollo_reply", ReceivedAt: t0, Body: []byte(
			`{"event":"` + event + `","contact_email":"` + email + `","contact_stage":"Do Not Contact"}`)}
	}
	evs, rows := Parse([]api.RawEvent{body("email_unsubscribed", " Ada@Example.com ")})
	if len(evs) != 1 || evs[0].Kind != "unsubscribed" || evs[0].Attrs["contact_id"] != "" {
		t.Fatalf("events = %+v", evs)
	}
	if len(rows) != 1 || rows[0].Columns["contact_id"] != "" || rows[0].Columns["email"] != "ada@example.com" {
		t.Fatalf("rows = %+v", rows)
	}
	e := merge.NormalizeEventKeys(evs[0])
	if key(t, body("email_unsubscribed", "ada@example.com")) == key(t, body("email_unsubscribed", "bo@example.com")) {
		t.Error("two people's email-only replies share a key")
	}
	if !strings.Contains(string(Key(e)), "ada@example.com") {
		t.Errorf("key %q, want it built from the email", Key(e))
	}
	lead, ok := merge.FindPerson(m, e)
	if !ok || lead != "L1" {
		t.Fatalf("FindPerson = %q, %v; want L1 by email", lead, ok)
	}
	Apply(m, lead, e, nil)
	if o := outcome(m, "L1"); !o.UnsubscribedAt.Equal(t0) {
		t.Errorf("the unsubscribe did not apply: %+v", o)
	}
}
