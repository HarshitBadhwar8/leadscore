package apollo

// The outreach part of the fake: contacts, sequences and enrollment (the
// sink), the contact search (the opt-out lookup), mailboxes (the
// apollo-sequences check) and the replied-email search (the poller). Replies
// are rendered from the fixtures' records, with the fake's own state in them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

var outreachFixtures = []string{
	"contacts_create/created", "contacts_create/invalid_email", "contacts_create/rate_limited",
	"contacts_create/server_error", "contacts_create/forbidden", "contacts_create/bad_request",
	"contacts_get/found", "contacts_get/not_found",
	"contacts_search/found", "contacts_search/opted_out", "contacts_search/rate_limited",
	"emailer_campaigns_search/found", "emailer_campaigns_search/rate_limited", "emailer_campaigns_search/forbidden",
	"emailer_campaigns_add_contact_ids/added", "emailer_campaigns_add_contact_ids/skipped_other_sequence",
	"emailer_campaigns_add_contact_ids/skipped_unsubscribed", "emailer_campaigns_add_contact_ids/skipped_invalid_email",
	"emailer_campaigns_add_contact_ids/rate_limited", "emailer_campaigns_add_contact_ids/server_error",
	"emailer_campaigns_add_contact_ids/forbidden", "emailer_campaigns_add_contact_ids/bad_request", "emailer_campaigns_add_contact_ids/already_in_sequence",
	"email_accounts/list", "email_accounts/bad_key",
	"emailer_messages_search/replies", "emailer_messages_search/rate_limited",
}

// Call names of the outreach calls, as the fixture folders name them.
const (
	CallCreateContact   = "contacts_create"
	CallGetContact      = "contacts_get"
	CallSearchContacts  = "contacts_search"
	CallSearchSequences = "emailer_campaigns_search"
	CallAddToSequence   = "emailer_campaigns_add_contact_ids"
	CallEmailAccounts   = "email_accounts"
	CallSearchMessages  = "emailer_messages_search"
)

// Contact is an Apollo contact the fake holds.
type Contact struct {
	ID, Email    string
	OptedOut     bool              // the opt-out flag; enrollment skips it
	InvalidEmail bool              // enrollment skips it as a bad email
	Sequences    map[string]string // sequence id -> status (active, paused, finished)
}

// Reply is a replied sequence email the message search returns. Apollo gives
// no reply time, only the send's (completed_at); ContactID empty is a null
// contact_id, as some live messages have.
type Reply struct {
	MessageID, ContactID, Email string
	Label                       string // empty: no label yet
	SentAt                      time.Time
}

type outreach struct {
	contacts  map[string]*Contact // by id
	byEmail   map[string]string   // lowercased email -> id
	nextID    int
	sequences [][2]string // id, name, in the order added
	mailboxes [][2]string // id, address
	replies   []Reply
	pageSize  int            // overrides the per_page asked, when set
	created   map[string]int // sinktest step -> objects created
	fails     map[string][]string
}

func newOutreach() *outreach {
	return &outreach{contacts: map[string]*Contact{}, byEmail: map[string]string{}, created: map[string]int{}, fails: map[string][]string{}}
}

// AddSequence makes the fake hold a sequence.
func (s *Server) AddSequence(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.sequences = append(s.out.sequences, [2]string{id, name})
}

// AddMailbox makes the fake hold a sending mailbox (email account), with the
// address <id>@leadscore-demo.example.
func (s *Server) AddMailbox(id string) {
	s.AddMailboxAddress(id, id+"@leadscore-demo.example")
}

// AddMailboxAddress makes the fake hold a sending mailbox with this address.
func (s *Server) AddMailboxAddress(id, address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.mailboxes = append(s.out.mailboxes, [2]string{id, address})
}

// AddContact puts a contact in the fake as if the team already had it. It is
// not counted as created. A second contact with an email the fake already
// holds is a duplicate: the search finds both, and creating by that email
// still finds the first.
func (s *Server) AddContact(c Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := c
	cp.Sequences = map[string]string{}
	for k, v := range c.Sequences {
		cp.Sequences[k] = v
	}
	s.out.contacts[c.ID] = &cp
	if _, dup := s.out.byEmail[strings.ToLower(c.Email)]; c.Email != "" && !dup {
		s.out.byEmail[strings.ToLower(c.Email)] = c.ID
	}
}

// ContactByEmail returns a copy of the contact holding email.
func (s *Server) ContactByEmail(email string) (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.out.contacts[s.out.byEmail[strings.ToLower(email)]]
	if !ok {
		return Contact{}, false
	}
	cp := *c
	cp.Sequences = map[string]string{}
	for k, v := range c.Sequences {
		cp.Sequences[k] = v
	}
	return cp, true
}

// AddReply adds a replied email to the message search.
func (s *Server) AddReply(r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.replies = append(s.out.replies, r)
}

// SetLabel changes a reply's label, as a person (or Apollo's classifier)
// labels it after it first appeared.
func (s *Server) SetLabel(messageID, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.out.replies {
		if s.out.replies[i].MessageID == messageID {
			s.out.replies[i].Label = label
		}
	}
}

// SetPageSize makes every search answer pages of n, whatever it asked for.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.pageSize = n
}

// FailNext makes the next request of a call answer a fixture case exactly as
// saved, for example FailNext(CallSearchMessages, "rate_limited").
func (s *Server) FailNext(call, fixtureCase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.fixtures[call+"/"+fixtureCase]; !ok {
		panic("fakes/apollo: no fixture " + call + "/" + fixtureCase)
	}
	s.out.fails[call] = append(s.out.fails[call], fixtureCase)
}

// FailNextAfter answers the next `after` requests of a call normally, then
// the one after them with a fixture case exactly as saved.
func (s *Server) FailNextAfter(call string, after int, fixtureCase string) {
	s.mu.Lock()
	for range after {
		s.out.fails[call] = append(s.out.fails[call], "")
	}
	s.mu.Unlock()
	s.FailNext(call, fixtureCase)
}

// Count is the number of objects the sink's step created (sinktest.Vendor):
// contacts for "contact", enrollments for "enroll".
func (s *Server) Count(step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.created[step]
}

// Fail makes the next call of a sink step fail this way (sinktest.Vendor):
// the create call for "contact", the add call for "enroll".
func (s *Server) Fail(step string, kind sinktest.FailKind) {
	call := map[string]string{"contact": CallCreateContact, "enroll": CallAddToSequence}[step]
	if call == "" {
		panic("fakes/apollo: no sink step " + step)
	}
	c := map[sinktest.FailKind]string{
		sinktest.RateLimited: "rate_limited",
		sinktest.Transient:   "server_error",
		sinktest.Other:       "bad_request",
	}[kind]
	if kind == sinktest.Refused {
		c = map[string]string{"contact": "invalid_email", "enroll": "skipped_unsubscribed"}[step]
	}
	s.FailNext(call, c)
}

// outreachCall names the call a request makes, or "".
func outreachCall(r *http.Request) string {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/api/v1/contacts":
		return CallCreateContact
	case r.Method == http.MethodPost && p == "/api/v1/contacts/search":
		return CallSearchContacts
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/contacts/") && !strings.Contains(p[len("/api/v1/contacts/"):], "/"):
		return CallGetContact
	case r.Method == http.MethodPost && p == "/api/v1/emailer_campaigns/search":
		return CallSearchSequences
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v1/emailer_campaigns/") && strings.HasSuffix(p, "/add_contact_ids"):
		return CallAddToSequence
	case r.Method == http.MethodGet && p == "/api/v1/email_accounts":
		return CallEmailAccounts
	case r.Method == http.MethodPost && p == "/api/v1/emailer_messages/search":
		return CallSearchMessages
	}
	return ""
}

// answerOutreach answers an outreach call with a good key. The caller holds s.mu.
func (s *Server) answerOutreach(call string, r *http.Request, raw []byte) (Fixture, []byte) {
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	o := s.out
	if q := o.fails[call]; len(q) > 0 && q[0] == "" {
		o.fails[call] = q[1:]
	} else if len(q) > 0 {
		o.fails[call] = q[1:]
		f := s.fixtures[call+"/"+q[0]]
		if call == CallAddToSequence && strings.HasPrefix(q[0], "skipped_") {
			return f, s.skipBody(q[0], firstID(body))
		}
		return f, f.ResponseBody
	}
	switch call {
	case CallCreateContact:
		return s.createContact(body)
	case CallGetContact:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/contacts/")
		c, ok := o.contacts[id]
		if !ok {
			f := s.fixtures["contacts_get/not_found"]
			return f, f.ResponseBody
		}
		return s.fixtures["contacts_get/found"], mustJSON(map[string]any{"contact": s.renderContact(c)})
	case CallSearchContacts:
		kw := strings.ToLower(str(body["q_keywords"]))
		var ids []string
		for id, c := range o.contacts {
			if kw != "" && strings.Contains(strings.ToLower(c.Email), kw) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		from, to, pg := o.page(len(ids), num(body["page"]), num(body["per_page"]))
		var list []any
		for _, id := range ids[from:to] {
			list = append(list, s.renderContact(o.contacts[id]))
		}
		return s.fixtures["contacts_search/found"], mustJSON(map[string]any{"contacts": nonNil(list), "pagination": pg})
	case CallSearchSequences:
		name := strings.ToLower(str(body["q_name"]))
		var hits [][2]string
		for _, sq := range o.sequences {
			if strings.Contains(strings.ToLower(sq[1]), name) {
				hits = append(hits, sq)
			}
		}
		f := s.fixtures["emailer_campaigns_search/found"]
		tmpl := firstRecord(f.ResponseBody, "emailer_campaigns")
		from, to, pg := o.page(len(hits), num(body["page"]), num(body["per_page"]))
		var list []any
		for _, sq := range hits[from:to] {
			rec := clone(tmpl)
			rec["id"], rec["name"] = sq[0], sq[1]
			list = append(list, rec)
		}
		return f, mustJSON(map[string]any{"emailer_campaigns": nonNil(list), "pagination": pg})
	case CallAddToSequence:
		return s.addToSequence(r, body)
	case CallEmailAccounts:
		f := s.fixtures["email_accounts/list"]
		tmpl := firstRecord(f.ResponseBody, "email_accounts")
		var list []any
		for _, mb := range o.mailboxes {
			rec := clone(tmpl)
			rec["id"], rec["email"], rec["aliases"] = mb[0], mb[1], []any{mb[1]}
			list = append(list, rec)
		}
		return f, mustJSON(map[string]any{"email_accounts": nonNil(list)})
	case CallSearchMessages:
		return s.searchMessages(body)
	}
	return Fixture{Status: http.StatusNotFound}, []byte(`{"error":"not faked"}`)
}

func (s *Server) createContact(body map[string]any) (Fixture, []byte) {
	o := s.out
	email := strings.TrimSpace(str(body["email"]))
	if !strings.Contains(email, "@") {
		f := s.fixtures["contacts_create/invalid_email"]
		return f, f.ResponseBody
	}
	f := s.fixtures["contacts_create/created"]
	if id, ok := o.byEmail[strings.ToLower(email)]; ok && body["run_dedupe"] == true {
		return f, mustJSON(map[string]any{"contact": s.renderContact(o.contacts[id])})
	}
	o.nextID++
	c := &Contact{ID: fmt.Sprintf("contact-%05d", 1000+o.nextID), Email: email, Sequences: map[string]string{}}
	o.contacts[c.ID] = c
	o.byEmail[strings.ToLower(email)] = c.ID
	o.created["contact"]++
	return f, mustJSON(map[string]any{"contact": s.renderContact(c)})
}

// addToSequence enrolls the contacts the body names. A contact already in
// this sequence is left as it is and not reported as skipped (unconfirmed
// that Apollo does the same); an opted-out one, one with a bad email, or one in
// any other sequence is skipped with the fixtures' reason.
func (s *Server) addToSequence(r *http.Request, body map[string]any) (Fixture, []byte) {
	o := s.out
	seqID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/emailer_campaigns/"), "/add_contact_ids")
	known := false
	for _, sq := range o.sequences {
		known = known || sq[0] == seqID
	}
	if !known {
		return Fixture{Status: http.StatusNotFound}, []byte(`{"error":"Sequence not found"}`)
	}
	mailboxOK := false
	for _, m := range o.mailboxes {
		mailboxOK = mailboxOK || m[0] == str(body["send_email_from_email_account_id"])
	}
	if !mailboxOK {
		return Fixture{Status: http.StatusUnprocessableEntity}, []byte(`{"error":"Email account not found"}`)
	}
	skipped := map[string]any{}
	var added []any
	ids, _ := body["contact_ids"].([]any)
	for _, v := range ids {
		id := str(v)
		c, ok := o.contacts[id]
		if !ok {
			skipped[id] = "contacts_not_found"
			continue
		}
		if _, in := c.Sequences[seqID]; in {
			added = append(added, s.renderContact(c))
			continue
		}
		reason := ""
		switch {
		case c.OptedOut:
			reason = s.skipReason("skipped_unsubscribed")
		case c.InvalidEmail:
			reason = s.skipReason("skipped_invalid_email")
		case len(c.Sequences) > 0:
			reason = s.skipReason("skipped_other_sequence")
		}
		if reason != "" {
			skipped[id] = reason
			continue
		}
		c.Sequences[seqID] = "active"
		o.created["enroll"]++
		added = append(added, s.renderContact(c))
	}
	f := s.fixtures["emailer_campaigns_add_contact_ids/added"]
	var saved map[string]any
	_ = json.Unmarshal(f.ResponseBody, &saved)
	saved["contacts"], saved["skipped_contact_ids"] = nonNil(added), skipped
	return f, mustJSON(saved)
}

// skipReason is the reason a skipped_* fixture gives.
func (s *Server) skipReason(fixtureCase string) string {
	var saved struct {
		Skipped map[string]string `json:"skipped_contact_ids"`
	}
	_ = json.Unmarshal(s.fixtures["emailer_campaigns_add_contact_ids/"+fixtureCase].ResponseBody, &saved)
	for _, r := range saved.Skipped {
		return r
	}
	panic("fakes/apollo: " + fixtureCase + " names no reason")
}

// skipBody is a skipped_* fixture's body for the given contact id.
func (s *Server) skipBody(fixtureCase, contactID string) []byte {
	var saved map[string]any
	_ = json.Unmarshal(s.fixtures["emailer_campaigns_add_contact_ids/"+fixtureCase].ResponseBody, &saved)
	saved["skipped_contact_ids"] = map[string]any{contactID: s.skipReason(fixtureCase)}
	return mustJSON(saved)
}

// searchMessages answers the replied-email search from its JSON body, as the
// live check saw Apollo do: the date filter is emailer_message_date_range's
// min day on the send (completed_at), and any other key for it (the camelCase
// emailerMessageDateRange) is ignored, so every reply comes back; the reply
// has no pagination record; a message has no reply time. Only the replied
// filter is served. SetPageSize does not apply: with no total_pages, a short
// page is the end, so the fake keeps the per_page asked.
func (s *Server) searchMessages(body map[string]any) (Fixture, []byte) {
	o := s.out
	stats, _ := body["emailer_message_stats"].([]any)
	if len(stats) != 1 || stats[0] != "replied" {
		return Fixture{Status: http.StatusUnprocessableEntity}, []byte(`{"error":"the fake serves only the replied filter"}`)
	}
	var start time.Time
	if rng, ok := body["emailer_message_date_range"].(map[string]any); ok {
		if str(body["emailer_message_date_range_mode"]) != "completed_at" {
			return Fixture{Status: http.StatusUnprocessableEntity}, []byte(`{"error":"the fake serves only the date filter on completed_at"}`)
		}
		var err error
		if start, err = time.Parse(time.DateOnly, str(rng["min"])); err != nil {
			return Fixture{Status: http.StatusUnprocessableEntity}, []byte(`{"error":"bad date range"}`)
		}
	}
	var hits []Reply
	for _, rp := range o.replies {
		if !rp.SentAt.Before(start) {
			hits = append(hits, rp)
		}
	}
	per := num(body["per_page"])
	if per <= 0 {
		per = 25
	}
	page := max(num(body["page"]), 1)
	from, to := min(len(hits), (page-1)*per), min(len(hits), page*per)
	f := s.fixtures["emailer_messages_search/replies"]
	tmpl := firstRecord(f.ResponseBody, "emailer_messages")
	var list []any
	for _, rp := range hits[from:to] {
		rec := clone(tmpl)
		rec["id"], rec["to_email"] = rp.MessageID, rp.Email
		rec["contact_id"] = nil
		if rp.ContactID != "" {
			rec["contact_id"] = rp.ContactID
		}
		rec["reply_class"] = nil
		if rp.Label != "" {
			rec["reply_class"] = rp.Label
		}
		rec["completed_at"] = rp.SentAt.UTC().Format("2006-01-02T15:04:05.000+00:00")
		list = append(list, rec)
	}
	var saved map[string]any
	_ = json.Unmarshal(f.ResponseBody, &saved)
	saved["emailer_messages"] = nonNil(list)
	return f, mustJSON(saved)
}

// renderContact is the contact in the fixtures' shape.
func (s *Server) renderContact(c *Contact) map[string]any {
	rec := clone(recordOf(s.fixtures["contacts_create/created"].ResponseBody, "contact"))
	rec["id"], rec["email"], rec["email_unsubscribed"] = c.ID, c.Email, c.OptedOut
	stTmpl := recordOf(s.fixtures["contacts_get/found"].ResponseBody, "contact")["contact_campaign_statuses"].([]any)[0].(map[string]any)
	var ids []string
	for id := range c.Sequences {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	statuses := []any{}
	for _, id := range ids {
		st := clone(stTmpl)
		st["emailer_campaign_id"], st["status"] = id, c.Sequences[id]
		statuses = append(statuses, st)
	}
	rec["contact_campaign_statuses"] = statuses
	return rec
}

// page cuts a list of n into the asked page, and its pagination record.
func (o *outreach) page(n, page, per int) (from, to int, pg map[string]any) {
	if o.pageSize > 0 {
		per = o.pageSize
	}
	if per <= 0 {
		per = 25
	}
	if page < 1 {
		page = 1
	}
	pages := (n + per - 1) / per
	from, to = min((page-1)*per, n), min(page*per, n)
	return from, to, map[string]any{"page": page, "per_page": per, "total_entries": n, "total_pages": pages}
}

func recordOf(body json.RawMessage, key string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		panic("fakes/apollo: " + err.Error())
	}
	return m[key].(map[string]any)
}

func firstRecord(body json.RawMessage, key string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		panic("fakes/apollo: " + err.Error())
	}
	return m[key].([]any)[0].(map[string]any)
}

func clone(m map[string]any) map[string]any {
	b := mustJSON(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func nonNil(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int { f, _ := v.(float64); return int(f) }

func firstID(body map[string]any) string {
	ids, _ := body["contact_ids"].([]any)
	if len(ids) == 0 {
		return ""
	}
	return str(ids[0])
}
