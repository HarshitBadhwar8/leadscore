// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package apollo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

var received = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func raw(kind, body string) api.RawEvent {
	return api.RawEvent{Seq: "7", Kind: kind, ReceivedAt: received, Body: []byte(body)}
}

// Event and email are load-bearing; a missing stage is not, and an unacted
// kind needs no identity (it is ignored, not refused).
func TestParseNotificationRequiresWhatCannotBeInferred(t *testing.T) {
	tests := []struct {
		name, raw string
		wantErr   bool
	}{
		{"complete", `{"event":"email_replied","contact_email":"a@example.com","contact_stage":"Replied"}`, false},
		{"no stage is fine", `{"event":"email_replied","contact_email":"a@example.com"}`, false},
		{"missing event", `{"contact_email":"a@example.com","contact_stage":"Replied"}`, true},
		{"missing email", `{"event":"email_replied","contact_stage":"Replied"}`, true},
		{"malformed json", `{"event":`, true},
		{"unacted kind needs no identity", `{"event":"email_opened"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNotification([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseNotificationLowercasesEmail(t *testing.T) {
	n, err := parseNotification([]byte(`{"event":"email_replied","contact_email":"  Ada@EXAMPLE.com "}`))
	if err != nil || n.Email != "ada@example.com" {
		t.Fatalf("%q %v", n.Email, err)
	}
}

// The watched property (top-level domain) is the team's own site, never the
// visitor's employer.
func TestTheWatchedPropertyIsNotTheVisitorsEmployer(t *testing.T) {
	evs, rows, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_site","domain":"ourproduct.example",`+
		`"visited_at":"2026-08-20T10:00:00Z","contact":{"email":"ada@example.com"},"account":{"domain":"example.com"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Domain != "example.com" || rows[0].Columns["company.domain"] != "example.com" {
		t.Errorf("employer = %q / %q, want the account block's domain", evs[0].Domain, rows[0].Columns["company.domain"])
	}
}

// The employer falls back to the account's website URL, reduced to its host.
func TestTheEmployerFallsBackToTheAccountWebsiteURL(t *testing.T) {
	for _, u := range []string{"https://www.example.com/careers", "example.com", "http://EXAMPLE.com"} {
		evs, _, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_site","contact":{},"account":{"website_url":"`+u+`"}}`))
		if err != nil || len(evs) != 1 || evs[0].Domain != "example.com" {
			t.Errorf("%s: %+v %v", u, evs, err)
		}
	}
}

// A name and a company are not an identity: the visit stays company-only and
// yields no receiver row.
func TestANameAloneIsNotAnIdentity(t *testing.T) {
	evs, rows, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_site","contact":{"first_name":"Ada","last_name":"Lee"},"account":{"domain":"example.com"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || evs[0].Email != "" || evs[0].LinkedInURL != "" || evs[0].Attrs[AttrFullName] != "" {
		t.Errorf("rows %v, event %+v", rows, evs[0])
	}
}

// An unresolved employer leaves the domain unset rather than guessed; merge
// derives it from the work email.
func TestAnUnresolvedEmployerStillLandsAPersonVisit(t *testing.T) {
	evs, rows, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_site","contact":{"email":"ada@example.com","first_name":"Ada"}}`))
	if err != nil || len(rows) != 1 || evs[0].Domain != "" {
		t.Fatalf("%+v %+v %v", evs, rows, err)
	}
	if _, has := rows[0].Columns["company.domain"]; has {
		t.Error("the receiver row must not carry a guessed domain")
	}
}

func TestHostOfIsAnchoredOnTheScheme(t *testing.T) {
	for raw, want := range map[string]string{
		"example.com":                     "example.com",
		"https://www.example.com/careers": "example.com",
		"http://EXAMPLE.com":              "example.com",
		"//example.com/x":                 "example.com",
		"example.com/careers//apply":      "example.com",
		"sub.example.com/take//step2":     "sub.example.com",
	} {
		if got, err := hostOf(raw); err != nil || got != want {
			t.Errorf("hostOf(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if got, err := hostOf("   "); err != nil || got != "" {
		t.Errorf("an absent value is not an error: %q %v", got, err)
	}
	if _, err := hostOf("://"); err == nil {
		t.Error("a malformed value must be told apart from an absent one")
	}
}

// Rejects name the problem but never a value from the body.
func TestRejectsNeverQuoteTheEmail(t *testing.T) {
	for _, r := range []api.RawEvent{
		raw(KindReply, `{"event":"email_replied","contact_stage":"Replied","contact_name":"x@secret.example"}`),
		raw(KindVisit, `{"event":"page_view","contact":{"email":"x@secret.example"}}`),
		raw(KindReply, `{"__not_json":true,"raw":"x@secret.example"}`),
		raw("other", `{"event":"email_sent","contact_email":"x@secret.example"}`),
	} {
		_, _, err := ParseRaw(r)
		if err == nil {
			t.Errorf("%s accepted", r.Body)
			continue
		}
		if strings.Contains(err.Error(), "secret.example") {
			t.Errorf("reject quotes the body: %v", err)
		}
	}
}

func TestPolledReplyKeyIsLengthPrefixed(t *testing.T) {
	if PolledReplyKey("a:b", "c") == PolledReplyKey("a", "b:c") {
		t.Error("two different pairs flattened to one key")
	}
	if PolledReplyKey("m1", "willing_to_meet") == PolledReplyKey("m1", "unsubscribe") {
		t.Error("a relabelled reply must be a new key")
	}
}

func TestRequiredPathsCoverWhatTheParsersRead(t *testing.T) {
	have := map[string]bool{}
	for _, p := range RequiredPaths() {
		have[p] = true
	}
	for _, p := range []string{"event", "contact_email", "contact_stage", "last_conversation_link", "visited_at",
		"contact_id", "contact.id", "contact.email", "contact.linkedin_url", "contact_linkedin_url",
		"account.domain", "account.website_url", "account_domain"} {
		if !have[p] {
			t.Errorf("RequiredPaths lacks %s", p)
		}
	}
}

// A visit time after our received time is clamped to it; a missing one is
// marked, timed at receipt.
func TestVisitTimeIsClampedAndMarked(t *testing.T) {
	evs, _, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_site","visited_at":"2026-09-30T00:00:00Z","contact":{"email":"a@example.com"}}`))
	if err != nil || !evs[0].At.Equal(received) || evs[0].Attrs[AttrNoVisitTime] != "" {
		t.Errorf("future visit: %+v %v", evs, err)
	}
	evs, _, err = ParseRaw(raw(KindVisit, `{"event":"website_visited_site","contact":{"email":"a@example.com"}}`))
	if err != nil || !evs[0].At.Equal(received) || evs[0].Attrs[AttrNoVisitTime] != "yes" {
		t.Errorf("no visit time: %+v %v", evs, err)
	}
}

// An unacted kind is reported as ignored, not dropped silently; an overlong
// visit name is refused.
func TestIgnoredAndOverlongKinds(t *testing.T) {
	_, _, err := ParseRaw(raw(KindReply, `{"event":"email_opened","contact_email":"a@example.com"}`))
	if !errors.Is(err, ErrIgnored) {
		t.Errorf("unacted kind: %v", err)
	}
	long := strings.Repeat("x", 80)
	if _, _, err := ParseRaw(raw(KindVisit, `{"event":"website_visited_`+long+`","contact":{"email":"a@example.com"}}`)); err == nil {
		t.Error("an overlong visit kind was accepted")
	}
}
