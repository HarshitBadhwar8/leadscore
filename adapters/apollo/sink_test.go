// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package apollo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
)

const (
	testMailbox = "mailbox-0001"
	testSeqID   = "seq-0001"
	testSeqName = "Qualified founders"
	testDest    = SequencePrefix + testSeqName
)

// outreachFake is a fake holding one sequence and one mailbox, and the
// sinks.apollo block pointing at it.
func outreachFake(t *testing.T) (*fakeapollo.Server, api.Config) {
	t.Helper()
	t.Setenv(KeyVariable, testKey)
	fake := fakeapollo.New(testKey)
	fake.AddSequence(testSeqID, testSeqName)
	fake.AddMailbox(testMailbox)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, api.Config{"base_url": srv.URL, "_http_client": srv.Client(), "mailbox_id": testMailbox}
}

func newTestSink(t *testing.T, cfg api.Config) *Sink {
	t.Helper()
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*Sink)
}

func lead(id, email string) api.LeadRef {
	return api.LeadRef{ID: api.LeadID(id), Emails: []string{email}, FullName: "Dana van der Reyes", Title: "Head of Operations",
		Domain: "acme-robotics.example", Fields: map[string]string{"company.name": "Acme Robotics"}}
}

func step(l api.LeadRef, name string, prior map[string]string) api.StepRequest {
	return api.StepRequest{Key: api.StepKey{LeadID: l.ID, LaneID: "cold", Step: name}, Dest: testDest, Lead: l, Prior: prior}
}

// push runs both steps and returns the enroll step's id and error.
func push(t *testing.T, s *Sink, l api.LeadRef) (string, error) {
	t.Helper()
	cid, err := s.Do(context.Background(), step(l, StepContact, nil))
	if err != nil {
		t.Fatalf("contact step: %v", err)
	}
	return s.Do(context.Background(), step(l, StepEnroll, map[string]string{StepContact: cid}))
}

func callsOf(fake *fakeapollo.Server, path string) int {
	n := 0
	for _, c := range fake.Calls() {
		if strings.HasSuffix(c.Path, path) {
			n++
		}
	}
	return n
}

func TestSinkSteps(t *testing.T) {
	_, cfg := outreachFake(t)
	s := newTestSink(t, cfg)
	if got := s.Steps(testDest); strings.Join(got, ",") != "contact,enroll" {
		t.Errorf("Steps(%q) = %v", testDest, got)
	}
	for _, d := range []string{"qualified", "sequence/", "sequence/  ", "list/x"} {
		if got := s.Steps(d); got != nil {
			t.Errorf("Steps(%q) = %v, want none", d, got)
		}
	}
}

func TestNewSinkNeedsTheMailboxAsText(t *testing.T) {
	_, cfg := outreachFake(t)
	for name, v := range map[string]any{"missing": nil, "empty": " ", "a number": 12345} {
		c := api.Config{}
		for k, val := range cfg {
			c[k] = val
		}
		if v == nil {
			delete(c, "mailbox_id")
		} else {
			c["mailbox_id"] = v
		}
		if _, err := NewSink(c); err == nil || !strings.Contains(err.Error(), "mailbox_id") {
			t.Errorf("%s: err = %v, want one naming mailbox_id", name, err)
		}
	}
}

// The contact step sends the split name and leaves out what it does not
// know; a second create for the same email gets the same contact.
func TestContactStepCreatesOnceWithDedupe(t *testing.T) {
	fake, cfg := outreachFake(t)
	s := newTestSink(t, cfg)
	l := lead("lead-1", "dana@acme-robotics.example")
	l.Title = ""
	a, err := s.Do(context.Background(), step(l, StepContact, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := newTestSink(t, cfg).Do(context.Background(), step(l, StepContact, nil))
	if err != nil || a != b || fake.Count("contact") != 1 {
		t.Fatalf("ids %q %q err %v, %d contacts", a, b, err, fake.Count("contact"))
	}
	var body map[string]any
	for _, c := range fake.Calls() {
		if c.Path == contactsPath {
			_ = json.Unmarshal(c.Body, &body)
		}
	}
	want := map[string]any{"email": "dana@acme-robotics.example", "run_dedupe": true, "first_name": "Dana",
		"last_name": "van der Reyes", "organization_name": "Acme Robotics", "website_url": "https://acme-robotics.example"}
	if len(body) != len(want) {
		t.Errorf("body %v, want %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
}

func TestSplitName(t *testing.T) {
	for in, want := range map[string][2]string{
		"": {"", ""}, "Cher": {"Cher", ""}, "Ada Lovelace": {"Ada", "Lovelace"},
		"  Jan  van der Berg ": {"Jan", "van der Berg"},
	} {
		if f, l := splitName(in); f != want[0] || l != want[1] {
			t.Errorf("splitName(%q) = %q, %q", in, f, l)
		}
	}
}

// Proof: a repeat enroll is a no-op. The same sink, a new sink (the next
// run, after a crash), and a contact the team already put in the sequence
// all get the same id with one enrollment and no second add call.
func TestRepeatEnrollIsANoOp(t *testing.T) {
	fake, cfg := outreachFake(t)
	l := lead("lead-1", "dana@acme-robotics.example")
	s := newTestSink(t, cfg)
	first, err := push(t, s, l)
	if err != nil || first == "" {
		t.Fatalf("first enroll: %q %v", first, err)
	}
	again, err := push(t, s, l)
	if err != nil || again != first {
		t.Fatalf("repeat on the same sink: %q %v, want %q", again, err, first)
	}
	replay, err := push(t, newTestSink(t, cfg), l)
	if err != nil || replay != first {
		t.Fatalf("repeat on a new sink: %q %v, want %q", replay, err, first)
	}
	if n := fake.Count("enroll"); n != 1 {
		t.Errorf("%d enrollments, want 1", n)
	}
	if n := callsOf(fake, "/add_contact_ids"); n != 1 {
		t.Errorf("%d add calls, want 1: a repeat reads the contact and stops", n)
	}

	// A contact already in the sequence before leadscore saw it.
	fake.AddContact(fakeapollo.Contact{ID: "contact-0007", Email: "eli@acme-robotics.example",
		Sequences: map[string]string{testSeqID: "paused"}})
	id, err := push(t, s, lead("lead-2", "eli@acme-robotics.example"))
	if err != nil || id != testSeqID+":contact-0007" || fake.Count("enroll") != 1 {
		t.Errorf("already enrolled: %q %v, %d enrollments", id, err, fake.Count("enroll"))
	}
}

// Proof: each refusal maps to ErrRefused, and none of them enrolls anyone.
func TestRefusalsMapToErrRefused(t *testing.T) {
	fake, cfg := outreachFake(t)
	s := newTestSink(t, cfg)
	fake.AddSequence("seq-0002", "Other team")
	fake.AddContact(fakeapollo.Contact{ID: "c-other", Email: "other@acme-robotics.example", Sequences: map[string]string{"seq-0002": "active"}})
	fake.AddContact(fakeapollo.Contact{ID: "c-done", Email: "done@acme-robotics.example", Sequences: map[string]string{"seq-0002": "finished"}})
	fake.AddContact(fakeapollo.Contact{ID: "c-out", Email: "out@acme-robotics.example", OptedOut: true})
	fake.AddContact(fakeapollo.Contact{ID: "c-bad", Email: "bad@acme-robotics.example", InvalidEmail: true})

	for _, email := range []string{"other@acme-robotics.example", "done@acme-robotics.example",
		"out@acme-robotics.example", "bad@acme-robotics.example"} {
		if _, err := push(t, s, lead("lead-"+email, email)); !errors.Is(err, api.ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", email, err)
		}
	}
	// An address Apollo refuses at create.
	if _, err := s.Do(context.Background(), step(lead("lead-x", "not-an-email"), StepContact, nil)); !errors.Is(err, api.ErrRefused) {
		t.Errorf("invalid email at create: err = %v, want ErrRefused", err)
	}
	if _, err := s.Do(context.Background(), step(api.LeadRef{ID: "lead-y"}, StepContact, nil)); !errors.Is(err, api.ErrRefused) {
		t.Errorf("no email: err = %v, want ErrRefused", err)
	}
	// Apollo's own skip, for a contact the read did not show in another
	// sequence (the race between the read and the add).
	fake.FailNext(fakeapollo.CallAddToSequence, "skipped_other_sequence")
	if _, err := push(t, s, lead("lead-z", "zed@acme-robotics.example")); !errors.Is(err, api.ErrRefused) ||
		!strings.Contains(err.Error(), reasonOtherSequence) {
		t.Errorf("skipped by Apollo: err = %v, want ErrRefused naming %q", err, reasonOtherSequence)
	}
	if n := fake.Count("enroll"); n != 0 {
		t.Errorf("%d enrollments, want none", n)
	}
	// The pre-read refuses without calling add for a contact in another
	// sequence or opted out.
	if n := callsOf(fake, "/add_contact_ids"); n != 2 {
		t.Errorf("%d add calls, want 2 (bad email, the race)", n)
	}
}

// A refusal's text never comes from Apollo's body, which may quote the person.
func TestClassify(t *testing.T) {
	status := func(code int, body string) error {
		return &StatusError{Status: code, body: []byte(body)}
	}
	cases := []struct {
		name string
		err  error
		want error // nil: counts an attempt
	}{
		{"422 another sequence", status(422, `{"error":"Contact is active in another sequence"}`), api.ErrRefused},
		{"400 opted out", status(400, `{"error":"contacts_unsubscribed"}`), api.ErrRefused},
		{"422 invalid email by code", status(422, `{"error":"Bad contact","error_code":"invalid_email"}`), api.ErrRefused},
		{"422 other", status(422, `{"error":"Email account not found"}`), nil},
		// The rest of a body may echo the contact: never read for a refusal.
		{"422 echoing the contact", status(422, `{"error":"Email account not found","contact":{"email":"unsubscribe-me@acme-robotics.example","email_unsubscribed":false,"note":"invalid email"}}`), nil},
		{"422 already in this sequence", status(422, `{"error_code":"contact_already_exists_in_campaign"}`), nil},
		{"not JSON", status(400, `unsubscribe`), nil},
		{"401", status(401, `{"error":"Invalid access credentials."}`), api.ErrRateLimited},
		{"403", status(403, `{"error":"not accessible with this api_key"}`), api.ErrRateLimited},
		{"500", status(500, `{}`), api.ErrTransient},
		{"503", status(503, ``), api.ErrTransient},
		{"timeout", context.DeadlineExceeded, api.ErrTransient},
		{"429", errors.Join(api.ErrRateLimited), api.ErrRateLimited},
		{"no id", errNoID, nil},
	}
	for _, c := range cases {
		got := classify(c.err)
		for _, class := range []error{api.ErrRefused, api.ErrTransient, api.ErrRateLimited} {
			if errors.Is(got, class) != (class == c.want) {
				t.Errorf("%s: classify = %v, want class %v", c.name, got, c.want)
			}
		}
		if c.want == api.ErrRefused && strings.Contains(got.Error(), "active in") {
			t.Errorf("%s: the refusal quotes Apollo's body: %v", c.name, got)
		}
	}
}

// Sequence names resolve once per run, exactly (never a near match), across
// pages; a missing or ambiguous name is ErrTransient.
func TestSequenceResolution(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddSequence("seq-0002", "Qualified founders 2")
	fake.AddSequence("seq-0003", "Twice")
	fake.AddSequence("seq-0004", "Twice")
	fake.SetPageSize(1)
	s := newTestSink(t, cfg)
	if _, err := push(t, s, lead("lead-1", "a@acme-robotics.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := push(t, s, lead("lead-2", "b@acme-robotics.example")); err != nil {
		t.Fatal(err)
	}
	if c, _ := fake.ContactByEmail("a@acme-robotics.example"); c.Sequences[testSeqID] != "active" || len(c.Sequences) != 1 {
		t.Errorf("enrolled in %v, want only %s", c.Sequences, testSeqID)
	}
	if n := callsOf(fake, sequencesSearchPath); n != 2 { // two pages, once
		t.Errorf("%d sequence searches, want 2 (two pages, resolved once per run)", n)
	}

	for _, name := range []string{"Missing", "Twice", "qualified founders"} {
		l := lead("lead-"+name, "c@acme-robotics.example")
		cid, _ := s.Do(context.Background(), step(l, StepContact, nil))
		req := step(l, StepEnroll, map[string]string{StepContact: cid})
		req.Dest = SequencePrefix + name
		if _, err := s.Do(context.Background(), req); !errors.Is(err, api.ErrTransient) {
			t.Errorf("%q: err = %v, want ErrTransient", name, err)
		}
	}
	if n := fake.Count("enroll"); n != 2 {
		t.Errorf("%d enrollments, want 2", n)
	}
}

func TestEnrollSendsFromTheMailboxAndSkipsOtherSequences(t *testing.T) {
	fake, cfg := outreachFake(t)
	if _, err := push(t, newTestSink(t, cfg), lead("lead-1", "a@acme-robotics.example")); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	for _, c := range fake.Calls() {
		if strings.HasSuffix(c.Path, "/add_contact_ids") {
			_ = json.Unmarshal(c.Body, &body)
		}
	}
	if body["send_email_from_email_account_id"] != testMailbox || body["sequence_active_in_other_campaigns"] != false ||
		body["sequence_finished_in_other_campaigns"] != false {
		t.Errorf("add body = %v", body)
	}
}

func TestEnrollNeedsTheContactID(t *testing.T) {
	_, cfg := outreachFake(t)
	_, err := newTestSink(t, cfg).Do(context.Background(), step(lead("l", "a@acme-robotics.example"), StepEnroll, nil))
	if err == nil || errors.Is(err, api.ErrRefused) || errors.Is(err, api.ErrTransient) {
		t.Errorf("err = %v, want an error that counts an attempt", err)
	}
}

// Proof (poller): since is used as given (its UTC day; a reply sent before
// it is not returned), and each reply carries its label and message id, and
// its contact id when Apollo gives one. A reply is timed at its send
// (completed_at): Apollo gives no reply time.
func TestPollUsesSinceAndCarriesLabelAndMessageID(t *testing.T) {
	fake, cfg := outreachFake(t)
	sent := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	fake.AddReply(fakeapollo.Reply{MessageID: "msg-1", ContactID: "contact-1", Email: "Dana@Acme-Robotics.example",
		Label: "willing_to_meet", SentAt: sent})
	fake.AddReply(fakeapollo.Reply{MessageID: "msg-2", Email: "eli@acme-robotics.example", SentAt: sent.Add(time.Hour)})
	fake.AddReply(fakeapollo.Reply{MessageID: "msg-old", Email: "old@acme-robotics.example", SentAt: sent.Add(-72 * time.Hour)})
	p, err := NewPoller(cfg)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 1, 23, 30, 0, 0, time.FixedZone("x", 2*3600)) // 21:30 UTC on 1 September
	evs, err := p.Poll(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, c := range fake.Calls() {
		if c.Path != messagesSearchPath {
			continue
		}
		pages++
		var body map[string]any
		_ = json.Unmarshal(c.Body, &body)
		if c.Method != http.MethodPost || len(c.Query) != 0 || body["page"] != float64(pages) {
			t.Errorf("poll call %d: %s, query %v, body %v", pages, c.Method, c.Query, body)
		}
	}
	if pages != 2 {
		t.Errorf("%d search pages read, want 2 (the second empty: only an empty page ends the read)", pages)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want the two replies sent since", evs)
	}
	a, b := evs[0], evs[1]
	if a.Kind != "reply" || a.Email != "dana@acme-robotics.example" || a.Attrs[AttrLabel] != "willing_to_meet" ||
		a.Attrs[AttrMessageID] != "msg-1" || a.Attrs[AttrContactID] != "contact-1" || !a.At.Equal(sent) || a.Attrs[AttrNoReplyTime] != "" {
		t.Errorf("first event = %+v", a)
	}
	if b.Email != "eli@acme-robotics.example" || b.Attrs[AttrMessageID] != "msg-2" || b.Attrs[AttrLabel] != "" ||
		b.Attrs[AttrContactID] != "" || !b.At.Equal(sent.Add(time.Hour)) {
		t.Errorf("an unlabelled reply with a null contact_id = %+v (matched by to_email, timed at its send)", b)
	}
}

// The reply search has no pagination record (live check 2026-10-08), so the
// poller reads pages until an empty one, never stopping on a short page:
// Apollo may cap per_page below the 100 asked without saying so, and a poll
// that stopped short would lose replies yet count as complete. 250 replies
// are four calls at 100 a page, and every reply is read when the page is
// silently capped at 30 (nine pages, then an empty one).
func TestPollReadsPagesUntilAnEmptyOne(t *testing.T) {
	for _, tc := range []struct{ n, capped, calls int }{{250, 0, 4}, {200, 0, 3}, {250, 30, 10}} {
		fake, cfg := outreachFake(t)
		if tc.capped > 0 {
			fake.SetPageSize(tc.capped)
		}
		sent := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
		for i := range tc.n {
			fake.AddReply(fakeapollo.Reply{MessageID: fmt.Sprintf("msg-%03d", i), Email: fmt.Sprintf("p%d@acme-robotics.example", i), SentAt: sent})
		}
		p, err := NewPoller(cfg)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := p.Poll(context.Background(), sent.Add(-time.Hour))
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		ids := map[string]bool{}
		for _, e := range evs {
			ids[e.Attrs[AttrMessageID]] = true
		}
		if len(evs) != tc.n || len(ids) != tc.n || callsOf(fake, messagesSearchPath) != tc.calls {
			t.Errorf("%+v: %d events (%d distinct) in %d calls", tc, len(evs), len(ids), callsOf(fake, messagesSearchPath))
		}
	}
}

// Up to maxPages pages may hold records; one more is read, and the search is
// complete only if that one is empty. More is an error, never read as
// complete.
func TestPollPageLimit(t *testing.T) {
	pages := func(full int) func(int) (int, error) {
		return func(page int) (int, error) {
			if page <= full {
				return perPage, nil
			}
			return 0, nil
		}
	}
	if err := eachUntilEmpty(3, pages(3)); err != nil {
		t.Errorf("exactly 3 full pages, then an empty one: %v", err)
	}
	if err := eachUntilEmpty(3, pages(4)); err == nil {
		t.Errorf("4 full pages under a limit of 3: no error")
	}
	calls := 0
	err := eachUntilEmpty(3, func(int) (int, error) { calls++; return 1, nil })
	if err == nil || calls != 4 {
		t.Errorf("never empty: err %v after %d calls, want an error after 4", err, calls)
	}
}

// An unknown contact id is a 422 (live check 2026-10-08), not a 404: the
// enroll step's read of it fails and counts one attempt, neither a refusal
// nor a wait.
func TestReadingAnUnknownContactIs422(t *testing.T) {
	_, cfg := outreachFake(t)
	s := newTestSink(t, cfg)
	_, err := s.c.readContact(context.Background(), "contact-9999")
	if !IsStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("err = %v, want a 422", err)
	}
	if c := classify(err); errors.Is(c, api.ErrRefused) || errors.Is(c, api.ErrTransient) || errors.Is(c, api.ErrRateLimited) {
		t.Errorf("classify = %v, want an error that counts an attempt", c)
	}
}

// mailbox_id may be the mailbox's address: the sink resolves it to the id
// once per run (one mailbox list call) and sends from the id. An id is used
// as given. An address no mailbox has makes the enroll step wait
// (ErrTransient), and enrolls nothing.
func TestMailboxByAddress(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddMailboxAddress("mailbox-0002", "Sales@Leadscore-Demo.example")
	cfg["mailbox_id"] = "sales@leadscore-demo.example"
	s := newTestSink(t, cfg)
	for _, l := range []api.LeadRef{lead("lead-1", "a@acme-robotics.example"), lead("lead-2", "b@acme-robotics.example")} {
		if _, err := push(t, s, l); err != nil {
			t.Fatal(err)
		}
	}
	if n := callsOf(fake, emailAccountsPath); n != 1 {
		t.Errorf("%d mailbox list calls, want 1 (resolved once per run)", n)
	}
	adds := 0
	for _, c := range fake.Calls() {
		if strings.HasSuffix(c.Path, "/add_contact_ids") {
			adds++
			var body map[string]any
			_ = json.Unmarshal(c.Body, &body)
			if body["send_email_from_email_account_id"] != "mailbox-0002" {
				t.Errorf("sent from %v, want the resolved id", body["send_email_from_email_account_id"])
			}
		}
	}
	if adds != 2 {
		t.Errorf("%d add calls, want 2", adds)
	}

	fake2, cfg2 := outreachFake(t)
	if _, err := push(t, newTestSink(t, cfg2), lead("lead-1", "a@acme-robotics.example")); err != nil {
		t.Fatal(err)
	}
	if n := callsOf(fake2, emailAccountsPath); n != 0 {
		t.Errorf("an id: %d mailbox list calls, want 0", n)
	}

	// An unknown address: both steps wait, before any contact is created or
	// read.
	fake3, cfg3 := outreachFake(t)
	cfg3["mailbox_id"] = "nobody@leadscore-demo.example"
	s3 := newTestSink(t, cfg3)
	l := lead("lead-1", "a@acme-robotics.example")
	_, err := s3.Do(context.Background(), step(l, StepContact, nil))
	if !errors.Is(err, api.ErrTransient) || !errors.Is(err, ErrMailboxNotFound) || fake3.Count("contact") != 0 {
		t.Errorf("an unknown address, contact step: err = %v, %d created", err, fake3.Count("contact"))
	}
	_, err = s3.Do(context.Background(), step(l, StepEnroll, map[string]string{StepContact: "contact-0001"}))
	if !errors.Is(err, api.ErrTransient) || callsOf(fake3, "/api/v1/contacts/contact-0001") != 0 {
		t.Errorf("an unknown address, enroll step: err = %v (or the contact was read)", err)
	}
}

// An alias of a mailbox resolves to that mailbox; the mailbox's own address
// wins over an alias another mailbox shares.
func TestMailboxByAlias(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddMailboxAddress("mailbox-0002", "old@leadscore-demo.example", "old@leadscore-demo.example", "team@leadscore-demo.example")
	fake.AddMailboxAddress("mailbox-0003", "team@leadscore-demo.example")
	c := newTestSink(t, cfg).c
	for addr, want := range map[string]string{"OLD@leadscore-demo.example": "mailbox-0002", "team@leadscore-demo.example": "mailbox-0003"} {
		if id, err := c.ResolveMailbox(context.Background(), addr); err != nil || id != want {
			t.Errorf("%s: %q, %v; want %s", addr, id, err, want)
		}
	}
	fake.AddMailboxAddress("mailbox-0004", "main@leadscore-demo.example", "main@leadscore-demo.example", "sales@leadscore-demo.example")
	if id, err := c.ResolveMailbox(context.Background(), "sales@leadscore-demo.example"); err != nil || id != "mailbox-0004" {
		t.Errorf("an alias: %q, %v", id, err)
	}
}

// A mailbox lookup that fails other than by the key or a rate limit (a 404,
// a 422, a 5xx) is a setup problem, no lead's: the step waits (ErrTransient)
// rather than spending the lead's attempts. A refused key still stops the
// sink for the run (ErrRateLimited).
func TestMailboxLookupFailuresWait(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{404, api.ErrTransient}, {422, api.ErrTransient}, {500, api.ErrTransient}, {401, api.ErrRateLimited}, {429, api.ErrRateLimited}} {
		c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		})
		s := &Sink{c: c, mailbox: "sales@leadscore-demo.example", sequences: map[string]string{}}
		_, err := s.sendingMailbox(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("%d: err = %v, want %v", tc.status, err, tc.want)
		}
		if tc.want == api.ErrTransient && (errors.Is(err, api.ErrRateLimited) || errors.Is(err, api.ErrRefused)) {
			t.Errorf("%d: err = %v is also another class", tc.status, err)
		}
	}
}

// The reply search is a POST, as a live API test used (body encoding not
// recorded; it is sent as JSON, like the other searches):
// the replied filter as a list, the date filter by completed_at from since's
// UTC day, and the page asked for. The body is the replies fixture's request.
func TestPollSendsTheSearchAsAJSONBody(t *testing.T) {
	fake, cfg := outreachFake(t)
	p, err := NewPoller(cfg)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 8, 1, 23, 30, 0, 0, time.FixedZone("x", 2*3600)) // 21:30 UTC on 1 August
	if _, err := p.Poll(context.Background(), since); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one search", calls)
	}
	fx, err := fakeapollo.Load("emailer_messages_search/replies")
	if err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if c.Method != fx.Method || c.Method != http.MethodPost || c.Path != fx.Path || len(c.Query) != 0 {
		t.Errorf("call = %s %s?%v, want %s %s with no query", c.Method, c.Path, c.Query, fx.Method, fx.Path)
	}
	var got, want map[string]any
	if err := json.Unmarshal(c.Body, &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", c.Body, err)
	}
	if err := json.Unmarshal(fx.RequestBody, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body\n got %v\nwant %v (the fixture's request)", got, want)
	}
	// The date filter's key is emailer_message_date_range: the live check saw
	// Apollo ignore emailerMessageDateRange and return every reply.
	rng, _ := got["emailer_message_date_range"].(map[string]any)
	if got["per_page"] != float64(perPage) || rng["min"] != "2026-08-01" || got["emailerMessageDateRange"] != nil {
		t.Errorf("body = %v", got)
	}
}

func TestPollFailsWholeOnARateLimit(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddReply(fakeapollo.Reply{MessageID: "m", Email: "a@acme-robotics.example", SentAt: time.Now()})
	fake.FailNext(fakeapollo.CallSearchMessages, "rate_limited")
	p, _ := NewPoller(cfg)
	if evs, err := p.Poll(context.Background(), time.Now().Add(-time.Hour)); !errors.Is(err, api.ErrRateLimited) || evs != nil {
		t.Errorf("Poll = %v, %v; want no events and ErrRateLimited", evs, err)
	}
}

// The Lookup, tested with the flag on: it is registered only when
// ContactOptOutFlag is true.
func TestLookupRegistrationFollowsTheFlag(t *testing.T) {
	if _, ok := api.LookupFactory("apollo"); ok != ContactOptOutFlag {
		t.Errorf("apollo Lookup registered = %v, want %v (ContactOptOutFlag)", ok, ContactOptOutFlag)
	}
	if _, ok := api.SinkFactory("apollo"); !ok {
		t.Error("the apollo sink is not registered")
	}
	if _, ok := api.PollerFactory("apollo"); !ok {
		t.Error("the apollo poller is not registered")
	}
}

func TestLookupReportsOptOutsAndPerLeadFailures(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddContact(fakeapollo.Contact{ID: "c-out", Email: "out@acme-robotics.example", OptedOut: true})
	fake.AddContact(fakeapollo.Contact{ID: "c-in", Email: "in@acme-robotics.example"})
	fake.AddContact(fakeapollo.Contact{ID: "c-near", Email: "xout@acme-robotics.example", OptedOut: true})
	l, err := NewLookup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	leads := []api.LeadRef{
		{ID: "a", Emails: []string{"in@acme-robotics.example", "OUT@acme-robotics.example"}},
		{ID: "b", Emails: []string{"in@acme-robotics.example"}},
		{ID: "c", Emails: []string{"nobody@acme-robotics.example"}},
	}
	evs, failed, err := l.Lookup(context.Background(), leads)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err %v failed %v", err, failed)
	}
	if len(evs) != 1 || evs[0].Kind != "optout" || evs[0].Email != "out@acme-robotics.example" || evs[0].Attrs[AttrContactID] != "c-out" {
		t.Errorf("events = %+v, want one optout for out@ only (not the near match)", evs)
	}

	// A rate limit fails the lead it hit and every later one, keeping what
	// was learned.
	fake.FailNext(fakeapollo.CallSearchContacts, "rate_limited")
	evs, failed, err = l.Lookup(context.Background(), []api.LeadRef{leads[0], leads[1], leads[2]}[:3])
	if err != nil || len(failed) != 3 || !errors.Is(failed["a"], api.ErrRateLimited) || len(evs) != 0 {
		t.Errorf("rate limited at the first call: evs %v failed %v err %v", evs, failed, err)
	}
	fake.FailNext(fakeapollo.CallSearchContacts, "found") // served as saved: a match with the flag false
	evs, failed, err = l.Lookup(context.Background(), []api.LeadRef{{ID: "d", Emails: []string{"dana.reyes@acme-robotics.example"}}, leads[0]})
	if err != nil || len(failed) != 0 || len(evs) != 1 {
		t.Errorf("saved found case: evs %v failed %v err %v", evs, failed, err)
	}
}

// A matching contact whose flag cannot be read fails that lead: it is never
// read as "not opted out".
func TestLookupMissingFlagFailsTheLead(t *testing.T) {
	t.Setenv(KeyVariable, testKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"contacts":[{"id":"c1","email":"a@acme-robotics.example"}],"pagination":{"page":1,"total_pages":1}}`))
	}))
	t.Cleanup(srv.Close)
	l, _ := NewLookup(api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	_, failed, err := l.Lookup(context.Background(), []api.LeadRef{{ID: "a", Emails: []string{"a@acme-robotics.example"}}, {ID: "b"}})
	if err != nil || failed["a"] == nil || len(failed) != 1 {
		t.Errorf("failed %v err %v, want lead a failed", failed, err)
	}
}

func TestLookupKeyRefusedFailsWhole(t *testing.T) {
	_, cfg := outreachFake(t)
	t.Setenv(KeyVariable, "wrong")
	l, _ := NewLookup(cfg)
	if _, _, err := l.Lookup(context.Background(), []api.LeadRef{{ID: "a", Emails: []string{"a@acme-robotics.example"}}}); !KeyRefused(err) {
		t.Errorf("err = %v, want the key refused", err)
	}
}

func TestEmailAccountIDs(t *testing.T) {
	_, cfg := outreachFake(t)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := c.EmailAccountIDs(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != testMailbox {
		t.Errorf("ids %v err %v", ids, err)
	}
}
