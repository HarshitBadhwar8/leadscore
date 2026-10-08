package apollo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
)

// countsAnAttempt reports an error that is none of the three classes: the
// engine counts it as one attempt.
func countsAnAttempt(err error) bool {
	return err != nil && !errors.Is(err, api.ErrRefused) && !errors.Is(err, api.ErrTransient) && !errors.Is(err, api.ErrRateLimited)
}

// The contact's own opt-out flag refuses the enroll before the add call,
// even with ContactOptOutFlag false (the Lookup unregistered).
func TestEnrollRefusesAnOptedOutContactBeforeAdding(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddContact(fakeapollo.Contact{ID: "c-out", Email: "out@acme-robotics.example", OptedOut: true})
	_, err := push(t, newTestSink(t, cfg), lead("lead-1", "out@acme-robotics.example"))
	if !errors.Is(err, api.ErrRefused) || !strings.Contains(err.Error(), reasonOptedOut) {
		t.Errorf("err = %v, want ErrRefused naming %q", err, reasonOptedOut)
	}
	if n := callsOf(fake, "/add_contact_ids"); n != 0 {
		t.Errorf("%d add calls, want none", n)
	}
}

// A duplicate contact with the lead's email that is in a sequence, or opted
// out, refuses the enroll: Apollo already holds that person.
func TestEnrollRefusesWhenADuplicateContactIsHeld(t *testing.T) {
	for name, dup := range map[string]fakeapollo.Contact{
		"active in another sequence": {ID: "c-dup", Email: "dana@acme-robotics.example", Sequences: map[string]string{"seq-0009": "active"}},
		"opted out":                  {ID: "c-dup", Email: "DANA@acme-robotics.example", OptedOut: true},
		"in this sequence":           {ID: "c-dup", Email: "dana@acme-robotics.example", Sequences: map[string]string{testSeqID: "finished"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake, cfg := outreachFake(t)
			s := newTestSink(t, cfg)
			l := lead("lead-1", "dana@acme-robotics.example")
			cid, err := s.Do(context.Background(), step(l, StepContact, nil))
			if err != nil {
				t.Fatal(err)
			}
			fake.AddContact(dup) // a second contact for the same person
			_, err = s.Do(context.Background(), step(l, StepEnroll, map[string]string{StepContact: cid}))
			if !errors.Is(err, api.ErrRefused) {
				t.Errorf("err = %v, want ErrRefused", err)
			}
			if n := callsOf(fake, "/add_contact_ids"); n != 0 || fake.Count("enroll") != 0 {
				t.Errorf("%d add calls, %d enrollments; want none", n, fake.Count("enroll"))
			}
		})
	}
	// The lead's other emails are searched too.
	fake, cfg := outreachFake(t)
	fake.AddContact(fakeapollo.Contact{ID: "c-alt", Email: "dana.alt@acme-robotics.example", Sequences: map[string]string{"seq-0009": "paused"}})
	l := lead("lead-2", "dana@acme-robotics.example")
	l.Emails = append(l.Emails, "dana.alt@acme-robotics.example")
	if _, err := push(t, newTestSink(t, cfg), l); !errors.Is(err, api.ErrRefused) || fake.Count("enroll") != 0 {
		t.Errorf("a held contact under the lead's second email: err = %v", err)
	}
}

// Every skip shape counts as a skip; success needs the contact listed and
// not skipped; an unrecognised reason, or an unexpected reply, counts one
// attempt.
func TestAddOutcome(t *testing.T) {
	const id = "c1"
	listedOnly := []any{map[string]any{"id": id}}
	cases := []struct {
		name  string
		reply map[string]any
		want  string // "done", "refused" or "attempt"
	}{
		{"listed, no skips", map[string]any{"contacts": listedOnly, "skipped_contact_ids": map[string]any{}}, "done"},
		{"keyed by id", map[string]any{"contacts": []any{}, "skipped_contact_ids": map[string]any{id: "contacts_unsubscribed"}}, "refused"},
		{"plain list", map[string]any{"contacts": listedOnly, "skipped_contact_ids": []any{id}}, "attempt"},
		{"reason to ids", map[string]any{"skipped_contact_ids": map[string]any{"contacts_active_in_other_campaigns": []any{id}}}, "refused"},
		{"nested", map[string]any{"skipped": map[string]any{"reasons": []any{map[string]any{"code": "invalid_email", "ids": []any{"x", id}}}}}, "attempt"},
		{"nested under a reason key", map[string]any{"skips": map[string]any{"contacts_without_email": map[string]any{"ids": []any{id}}}}, "refused"},
		{"listed but also skipped", map[string]any{"contacts": listedOnly, "skipped_contact_ids": map[string]any{id: "contacts_unsubscribed"}}, "refused"},
		{"unrecognised reason", map[string]any{"skipped_contact_ids": map[string]any{id: "contacts_on_holiday"}}, "attempt"},
		{"already in this sequence", map[string]any{"skipped_contact_ids": map[string]any{id: "already_in_campaign"}}, "confirm"},
		{"neither listed nor skipped", map[string]any{"contacts": []any{map[string]any{"id": "other"}}}, "attempt"},
		{"empty reply", map[string]any{}, "attempt"},
		{"another contact skipped", map[string]any{"contacts": listedOnly, "skipped_contact_ids": map[string]any{"c2": "contacts_unsubscribed"}}, "done"},
	}
	for _, c := range cases {
		err := addOutcome(c.reply, id)
		got := map[bool]string{true: "done"}[err == nil]
		switch {
		case errors.Is(err, errSaysInSequence):
			got = "confirm"
		case errors.Is(err, api.ErrRefused):
			got = "refused"
		case countsAnAttempt(err):
			got = "attempt"
		}
		if got != c.want {
			t.Errorf("%s: %v (%s), want %s", c.name, err, got, c.want)
		}
	}
}

// The fake skips a contact it does not know (contacts_not_found): an
// unrecognised reason, which counts one attempt.
func TestUnknownSkipReasonCountsAnAttempt(t *testing.T) {
	_, cfg := outreachFake(t)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.addToSequence(context.Background(), testSeqID, "contact-unknown", testMailbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := addOutcome(reply, "contact-unknown"); !countsAnAttempt(err) {
		t.Errorf("err = %v, want one that counts an attempt", err)
	}
}

// "Already in this sequence", from a 4xx or a skip, is done only when a
// second read of the contact shows this sequence; else one attempt.
func TestAlreadyInSequenceIsConfirmed(t *testing.T) {
	addReplies := map[string]struct {
		status int
		body   string
	}{
		"4xx":  {422, `{"error":"Contact is already in this sequence","error_code":"contact_already_exists_in_campaign"}`},
		"skip": {200, `{"contacts":[],"skipped_contact_ids":{"c1":"already_in_campaign"}}`},
	}
	for name, add := range addReplies {
		for _, seqOnReread := range []string{testSeqID, "seq-other"} {
			t.Run(name+"/"+seqOnReread, func(t *testing.T) {
				t.Setenv(KeyVariable, testKey)
				reads := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == sequencesSearchPath:
						_, _ = fmt.Fprintf(w, `{"emailer_campaigns":[{"id":%q,"name":%q}],"pagination":{"page":1,"total_pages":1}}`, testSeqID, testSeqName)
					case r.URL.Path == contactsPath+"/c1":
						reads++
						statuses := `[]`
						if reads > 1 {
							statuses = fmt.Sprintf(`[{"emailer_campaign_id":%q,"status":"active"}]`, seqOnReread)
						}
						_, _ = fmt.Fprintf(w, `{"contact":{"id":"c1","email":"a@acme-robotics.example","contact_campaign_statuses":%s}}`, statuses)
					case r.URL.Path == contactsSearchPath:
						_, _ = w.Write([]byte(`{"contacts":[],"pagination":{"page":1,"total_pages":0}}`))
					case strings.HasSuffix(r.URL.Path, "/add_contact_ids"):
						w.WriteHeader(add.status)
						_, _ = w.Write([]byte(add.body))
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(srv.Close)
				s := newTestSink(t, api.Config{"base_url": srv.URL, "_http_client": srv.Client(), "mailbox_id": testMailbox})
				id, err := s.Do(context.Background(), step(lead("lead-1", "a@acme-robotics.example"), StepEnroll, map[string]string{StepContact: "c1"}))
				if seqOnReread == testSeqID {
					if err != nil || id != testSeqID+":c1" {
						t.Errorf("confirmed: id %q err %v, want done", id, err)
					}
				} else if !countsAnAttempt(err) {
					t.Errorf("not confirmed: err %v, want one that counts an attempt", err)
				}
				if reads != 2 {
					t.Errorf("%d contact reads, want 2 (before the add, and the confirmation)", reads)
				}
			})
		}
	}
}

// A refused key (401) or one without the scope (403) stops the sink for the
// run: ErrRateLimited, no attempt counted.
func TestRefusedKeyStopsTheSink(t *testing.T) {
	fake, cfg := outreachFake(t)
	s := newTestSink(t, cfg)
	l := lead("lead-1", "a@acme-robotics.example")
	fake.FailNext(fakeapollo.CallCreateContact, "forbidden")
	if _, err := s.Do(context.Background(), step(l, StepContact, nil)); !errors.Is(err, api.ErrRateLimited) {
		t.Errorf("403 on create: err = %v, want ErrRateLimited", err)
	}
	t.Setenv(KeyVariable, "wrong")
	if _, err := newTestSink(t, cfg).Do(context.Background(), step(l, StepContact, nil)); !errors.Is(err, api.ErrRateLimited) {
		t.Errorf("401: err = %v, want ErrRateLimited", err)
	}
}

// A contact record without its sequence list is an error, never read as a
// contact in no sequence.
func TestContactWithoutStatusesIsAnError(t *testing.T) {
	for _, body := range []string{`{}`, `{"contact":null}`, `{"contact":{"id":"c1","email":"a@acme-robotics.example"}}`} {
		c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		if _, err := c.readContact(context.Background(), "c1"); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
	c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"contacts":[{"id":"c1","email":"a@acme-robotics.example"}],"pagination":{"page":1,"total_pages":1}}`))
	})
	if _, err := c.contactsByEmail(context.Background(), "a@acme-robotics.example"); err == nil {
		t.Error("a matching search result without statuses: no error")
	}
}

func TestEachPage(t *testing.T) {
	run := func(n int, pages ...pagination) (int, error) {
		calls := 0
		err := eachPage(5, func(page int) (int, pagination, error) {
			calls++
			return n, pages[min(page, len(pages))-1], nil
		})
		return calls, err
	}
	if calls, err := run(perPage, pagination{TotalPages: 2}, pagination{TotalPages: 2}); err != nil || calls != 2 {
		t.Errorf("two pages: %d calls, %v", calls, err)
	}
	if calls, err := run(3, pagination{}); err != nil || calls != 1 {
		t.Errorf("a short page with no total_pages: %d calls, %v", calls, err)
	}
	if _, err := run(perPage, pagination{}); err == nil {
		t.Error("a full page with no total_pages: no error")
	}
	if calls, err := run(perPage, pagination{TotalPages: 6}); err == nil || calls != 1 {
		t.Errorf("over the limit: %d calls, %v; want an error after page 1", calls, err)
	}
}

// fullPage serves perPage records under key with no pagination.
func fullPage(t *testing.T, key, record string) api.Config {
	t.Helper()
	t.Setenv(KeyVariable, testKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		recs := strings.TrimSuffix(strings.Repeat(record+",", perPage), ",")
		_, _ = fmt.Fprintf(w, `{%q:[%s]}`, key, recs)
	}))
	t.Cleanup(srv.Close)
	return api.Config{"base_url": srv.URL, "_http_client": srv.Client(), "mailbox_id": testMailbox}
}

// The poller, the lookup and the sequence search refuse a full page with no
// total_pages.
func TestFullPageWithoutTotalIsAnError(t *testing.T) {
	cfg := fullPage(t, "emailer_messages", `{"id":"m","to_email":"a@acme-robotics.example"}`)
	p, _ := NewPoller(cfg)
	if _, err := p.Poll(context.Background(), time.Now()); err == nil {
		t.Error("poller: no error")
	}
	cfg = fullPage(t, "contacts", `{"id":"c","email":"a@acme-robotics.example","email_unsubscribed":false}`)
	l, _ := NewLookup(cfg)
	if _, failed, err := l.Lookup(context.Background(), []api.LeadRef{{ID: "a", Emails: []string{"a@acme-robotics.example"}}}); err != nil || failed["a"] == nil {
		t.Errorf("lookup: failed %v err %v, want lead a failed", failed, err)
	}
	cfg = fullPage(t, "emailer_campaigns", `{"id":"s","name":"Other"}`)
	c, _ := NewClient(cfg)
	if _, err := c.ResolveSequence(context.Background(), "Mine"); err == nil || errors.Is(err, ErrSequenceNotFound) {
		t.Errorf("sequence search: err = %v, want an error that is not not-found", err)
	}
}

// A rate limit after an opt-out was learned in the same call keeps the
// opt-out, and fails only the leads not looked up.
func TestLookupKeepsAnOptOutLearnedBeforeARateLimit(t *testing.T) {
	fake, cfg := outreachFake(t)
	fake.AddContact(fakeapollo.Contact{ID: "c-out", Email: "out@acme-robotics.example", OptedOut: true})
	fake.FailNextAfter(fakeapollo.CallSearchContacts, 1, "rate_limited")
	l, _ := NewLookup(cfg)
	evs, failed, err := l.Lookup(context.Background(), []api.LeadRef{
		{ID: "a", Emails: []string{"out@acme-robotics.example"}},
		{ID: "b", Emails: []string{"b@acme-robotics.example"}},
		{ID: "c", Emails: []string{"c@acme-robotics.example"}},
	})
	if err != nil || len(evs) != 1 || evs[0].Email != "out@acme-robotics.example" {
		t.Fatalf("evs %v err %v, want the opt-out kept", evs, err)
	}
	if failed["a"] != nil || !errors.Is(failed["b"], api.ErrRateLimited) || !errors.Is(failed["c"], api.ErrRateLimited) {
		t.Errorf("failed %v, want b and c only", failed)
	}
}

// A polled reply Apollo gave no time for is timed at the poll and marked, so
// its key leaves the time out.
func TestPolledReplyWithNoTime(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	e, ok := polledMessage{ContactID: "c1", ReplyClass: "Not_Interested"}.event(now)
	if !ok || !e.At.Equal(now) || e.Attrs[AttrNoReplyTime] != "yes" || e.Attrs[AttrLabel] != "not_interested" {
		t.Errorf("event %+v", e)
	}
	if e, _ := (polledMessage{ID: "m", ContactID: "c1", CompletedAt: "2026-09-30T08:00:00.000+00:00"}).event(now); e.Attrs[AttrNoReplyTime] != "" {
		t.Errorf("a timed reply is marked: %+v", e)
	}
}
