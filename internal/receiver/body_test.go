// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package receiver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// stored runs a body through what the receiver does before storing it.
func stored(t *testing.T, body []byte) []byte {
	t.Helper()
	return storedBody(readBody(body, []string{testSecret}))
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("stored body is not JSON: %v\n%s", err, b)
	}
	return m
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parsed is what a run reads from a stored reply body.
func parsed(t *testing.T, kind string, body []byte) []api.Event {
	t.Helper()
	evs, _, err := apollo.ParseRaw(api.RawEvent{Kind: kind, ReceivedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Body: body})
	if err != nil {
		t.Fatalf("the stored body does not parse: %v", err)
	}
	return evs
}

// A delivery whose transcript is over the string cap must still be stored:
// capped, not rejected, with unmodelled fields kept and the cut recorded.
func TestAnOversizedTranscriptIsCappedNotRejected(t *testing.T) {
	huge := strings.Repeat("x", 300<<10)
	body := marshal(t, map[string]any{
		"event":                        "email_replied",
		"contact_email":                "priya@example.com",
		"contact_stage":                "Interested",
		"past_conversations":           huge,
		"last_conversation_summary":    strings.Repeat("z", 20<<10),
		"vendor_field_we_do_not_model": "must survive the rewrite",
	})
	out := stored(t, body)
	m := decode(t, out)
	for _, f := range []string{"past_conversations", "last_conversation_summary"} {
		s, _ := m[f].(string)
		if len(s) > maxStringBytes {
			t.Errorf("%s = %d bytes, want <= %d", f, len(s), maxStringBytes)
		}
		if !strings.HasSuffix(s, truncatedMarker) {
			t.Errorf("%s does not end with the truncation marker", f)
		}
	}
	if m["vendor_field_we_do_not_model"] != "must survive the rewrite" {
		t.Error("the rewrite dropped an unmodelled vendor field")
	}
	cut, _ := m[truncatedFieldsKey].([]any)
	if len(cut) != 2 || cut[0] != "last_conversation_summary" || cut[1] != "past_conversations" {
		t.Errorf("%s = %v, want both text fields named", truncatedFieldsKey, m[truncatedFieldsKey])
	}
	if n := utf8.RuneCount(out); n > maxBodyChars {
		t.Errorf("stored body = %d characters, over a Sheets cell", n)
	}
	if evs := parsed(t, apollo.KindReply, out); len(evs) != 1 || evs[0].Email != "priya@example.com" {
		t.Errorf("the record was damaged: %+v", evs)
	}
}

// A delivery inside every limit is stored byte for byte.
func TestANormalDeliveryIsRetainedVerbatim(t *testing.T) {
	body := []byte(`{"event":"email_replied","contact_email":"priya@example.com","past_conversations":"short <b>&</b>","n":1.50}`)
	if out := stored(t, body); string(out) != string(body) {
		t.Errorf("the body was rewritten:\n got %s\nwant %s", out, body)
	}
}

// The bulk in a field nobody caps: the body is still cut to fit one cell,
// keeping the fields the parsers read, and says why.
func TestAnOversizedBodyKeepsOnlyTheParsersFields(t *testing.T) {
	fields := map[string]any{
		"event":                  "email_unsubscribed",
		"contact_email":          "priya@example.com",
		"contact_stage":          "Do Not Contact",
		"contact_name":           "Priya Example",
		"contact_title":          "Head of Ops",
		"contact_id":             "ct-9",
		"contact_linkedin_url":   "https://www.linkedin.com/in/priya-example",
		"account_domain":         "example.com",
		"last_conversation_link": "https://app.apollo.io/#/conv/1",
	}
	want := parsed(t, apollo.KindReply, marshal(t, fields))
	for i := range 10 {
		fields["some_vendor_addition_"+string(rune('a'+i))] = strings.Repeat("y", 15<<10) // each under the string cap
	}
	out := stored(t, marshal(t, fields))
	if n := utf8.RuneCount(out); n > maxBodyChars {
		t.Fatalf("stored body = %d characters, over the %d a Sheets cell holds", n, maxBodyChars)
	}
	m := decode(t, out)
	if m[droppedReasonKey] == nil {
		t.Error("the extra fields were dropped without saying why")
	}
	if _, ok := m["some_vendor_addition_a"]; ok {
		t.Error("a field the parsers do not read was kept")
	}
	got := parsed(t, apollo.KindReply, out)
	if len(got) != 1 || got[0].Kind != want[0].Kind || got[0].Email != want[0].Email ||
		got[0].LinkedInURL != want[0].LinkedInURL || got[0].Domain != want[0].Domain ||
		len(got[0].Attrs) != len(want[0].Attrs) {
		t.Errorf("the cut-down body parses differently:\n got %+v\nwant %+v", got, want)
	}
}

// A visit cut down keeps its nested contact and account fields where they were.
func TestAnOversizedVisitKeepsNestedFieldsInPlace(t *testing.T) {
	body := map[string]any{
		"event":      "website_visited_pricing",
		"visited_at": "2026-08-20T10:00:00Z",
		"contact": map[string]any{"id": "ct-2", "email": "lee@example.net", "first_name": "Lee", "last_name": "Park",
			"title": "CTO", "company": "Example Net", "linkedin_url": "https://www.linkedin.com/in/lee", "notes": strings.Repeat("n", 15<<10)},
		"account": map[string]any{"domain": "example.net", "name": "Example Net", "website_url": "https://example.net",
			"description": strings.Repeat("d", 15<<10)},
		"pages": []any{strings.Repeat("p", 15<<10), strings.Repeat("q", 15<<10)},
	}
	want := parsed(t, apollo.KindVisit, marshal(t, body))
	out := stored(t, marshal(t, body))
	if n := utf8.RuneCount(out); n > maxBodyChars {
		t.Fatalf("stored body = %d characters", n)
	}
	m := decode(t, out)
	contact, _ := m["contact"].(map[string]any)
	if contact["email"] != "lee@example.net" || contact["notes"] != nil {
		t.Errorf("contact = %v, want only the parsers' fields, in place", contact)
	}
	got := parsed(t, apollo.KindVisit, out)
	if len(got) != 1 || got[0].Kind != want[0].Kind || got[0].Email != want[0].Email || !got[0].At.Equal(want[0].At) ||
		got[0].Attrs["company"] != want[0].Attrs["company"] {
		t.Errorf("the cut-down visit parses differently:\n got %+v\nwant %+v", got, want)
	}
}

// Even the parsers' own fields can be too long together; they are shortened
// further rather than overflow the cell.
func TestHugeRequiredFieldsStillFit(t *testing.T) {
	fields := map[string]any{"event": "email_replied", "contact_email": "a@example.com"}
	for _, p := range []string{"contact_stage", "last_conversation_link", "contact_name", "contact_title", "account_domain"} {
		fields[p] = strings.Repeat("\n", 20<<10) // doubles when encoded
	}
	out := stored(t, marshal(t, fields))
	if n := utf8.RuneCount(out); n > maxBodyChars {
		t.Fatalf("stored body = %d characters", n)
	}
	if m := decode(t, out); m["contact_email"] != "a@example.com" {
		t.Errorf("the record was damaged: %v", m["contact_email"])
	}
}

// A cut lands on a character boundary.
func TestTruncationDoesNotSplitARune(t *testing.T) {
	out := stored(t, []byte(`{"event":"email_replied","contact_email":"p@example.com","past_conversations":"`+
		strings.Repeat("日", 20<<10)+`"}`))
	s, _ := decode(t, out)["past_conversations"].(string)
	if !utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError) {
		t.Error("the capped field is not clean UTF-8; a byte cut split a character")
	}
	if len(s) > maxStringBytes {
		t.Errorf("capped field = %d bytes", len(s))
	}
}

// Escapable content: each newline becomes two characters once encoded, so
// fields that pass the string cap can still overflow the cell together. The
// limit is applied to the encoded body.
func TestEscapeExpansionCannotPushTheBodyOverTheCell(t *testing.T) {
	newlines := strings.Repeat("\n", 20<<10)
	body := marshal(t, map[string]any{
		"event":                     "email_replied",
		"contact_email":             "priya@example.com",
		"past_conversations":        newlines,
		"last_conversation_summary": newlines,
	})
	out := stored(t, body)
	if n := utf8.RuneCount(out); n > maxBodyChars {
		t.Errorf("stored body = %d encoded characters, over %d", n, maxBodyChars)
	}
	if decode(t, out)["contact_email"] != "priya@example.com" {
		t.Error("the record was damaged")
	}
}

// A body that is not a JSON object is kept as its first 16KB, marked.
func TestABodyThatIsNotJSONIsKeptMarked(t *testing.T) {
	for _, raw := range []string{"not json at all " + strings.Repeat("é", 20<<10), `["an","array"]`, `"a string"`, `{"a":1} {"b":2}`, ""} {
		out := stored(t, []byte(raw))
		m := decode(t, out)
		if m[notJSONKey] != true {
			t.Errorf("%.20q: %s missing", raw, notJSONKey)
		}
		s, _ := m[rawKey].(string)
		if len(s) > maxStringBytes || !utf8.ValidString(s) || !strings.HasPrefix(raw, s) {
			t.Errorf("%.20q: raw kept as %d bytes, want a clean prefix of at most 16KB", raw, len(s))
		}
		if _, _, err := apollo.ParseRaw(api.RawEvent{Kind: apollo.KindReply, Body: out}); err == nil {
			t.Errorf("%.20q: a not-JSON body must be refused by the parser", raw)
		}
	}
}

// The secret field is removed, in any spelling and at any depth, and a
// configured secret is masked wherever it appears; the rest is kept as sent.
func TestTheSecretFieldIsNeverStored(t *testing.T) {
	raw := []byte(`{"event":"email_sent","contact_email":"a@example.com","leadscore_secret":"s3cret","LeadScore_Secret":"other",` +
		`"contact":{"leadscore_secret":"nested"},"list":[{"LEADSCORE_SECRET":"deep"}],"note":"pasted ` + testSecret + ` here","n":12345678901234567890}`)
	if got := scanSecret(raw); !slices.Equal(got, []string{"s3cret"}) {
		t.Errorf("presented secret = %q", got)
	}
	out := string(storedBody(readBody(raw, []string{testSecret, ""})))
	for _, leak := range []string{"leadscore_secret", "s3cret", "other", "nested", "deep", testSecret} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(leak)) {
			t.Errorf("%q reached the stored body: %s", leak, out)
		}
	}
	if !strings.Contains(out, "12345678901234567890") || !strings.Contains(out, "pasted [REDACTED] here") {
		t.Errorf("the rest was changed: %s", out)
	}
	// A spelling other than the exact field is removed but never accepted.
	if got := scanSecret([]byte(`{"LEADSCORE_SECRET":"s3cret"}`)); len(got) != 0 {
		t.Errorf("a variant spelling was presented: %q", got)
	}
	// A nested field is never accepted either.
	if got := scanSecret([]byte(`{"contact":{"leadscore_secret":"s3cret"}}`)); len(got) != 0 {
		t.Errorf("a nested field was presented: %q", got)
	}
}

// A body that is not a JSON object is stored as text: every configured secret
// and any leadscore_secret value in it is masked first.
func TestTheSecretIsMaskedInABodyThatIsNotJSON(t *testing.T) {
	for _, raw := range []string{
		`{"event":"email_sent","leadscore_secret":"` + testSecret + `","contact_name":"Dana "DJ" Example"}`,
		`["leadscore_secret","` + testSecret + `"]`,
		`{"Leadscore_Secret" : "some-other-value", "x": }`,
		`not json, but it mentions ` + testSecret,
		`{"event":"x","leadscore_secret":"` + previousSecret + `"`,
	} {
		out := string(stored(t, []byte(raw)))
		for _, leak := range []string{testSecret, "some-other-value", previousSecret} {
			if strings.Contains(out, leak) {
				t.Errorf("%.30q: the secret reached the stored body: %s", raw, out)
			}
		}
		if !strings.Contains(out, notJSONKey) {
			t.Errorf("%.30q: not marked as not JSON: %s", raw, out)
		}
	}
}

// When the body is not valid JSON (Apollo left a quote unescaped), the body
// secret is still found in its text.
func TestABodySecretIsFoundInBrokenJSON(t *testing.T) {
	raw := []byte(`{"event":"email_unsubscribed","contact_name":"Dana "DJ" Example","leadscore_secret":"` + testSecret + `"}`)
	if got := scanSecret(raw); !slices.Contains(got, testSecret) {
		t.Errorf("scanSecret = %q, want the secret", got)
	}
	if got := scanSecret([]byte(`{"a":"b"c","LEADSCORE_SECRET":"` + testSecret + `"}`)); len(got) != 0 {
		t.Errorf("a variant spelling in broken JSON was presented: %q", got)
	}
}

// Kept parser fields that are not strings (a huge number, a long array) are
// shortened too, so the body always fits a cell.
func TestNonStringParserFieldsStillFit(t *testing.T) {
	arr := make([]any, 20000)
	for i := range arr {
		arr[i] = "x"
	}
	for name, body := range map[string]map[string]any{
		"huge number": {"event": "email_replied", "contact_email": "a@example.com", "contact_id": json.Number(strings.Repeat("9", 60000))},
		"long array":  {"event": "email_replied", "contact_email": "a@example.com", "contact_stage": arr},
		"array of long strings": {"event": "email_replied", "contact_email": "a@example.com",
			"contact_title": []any{strings.Repeat("t", 15<<10), strings.Repeat("u", 15<<10), strings.Repeat("v", 15<<10), strings.Repeat("w", 15<<10)}},
	} {
		out := stored(t, marshal(t, body))
		if n := utf8.RuneCount(out); n > maxBodyChars {
			t.Errorf("%s: stored body = %d characters", name, n)
		}
		m := decode(t, out)
		if m[droppedReasonKey] == nil {
			t.Errorf("%s: no reason given", name)
		}
	}
}

// A cut-down body keeps the list of the strings it shortened.
func TestACutDownBodyListsWhatItShortened(t *testing.T) {
	body := map[string]any{"event": "email_replied", "contact_email": "a@example.com", "contact_title": strings.Repeat("t", 20<<10)}
	for i := range 10 {
		body["extra"+strconv.Itoa(i)] = strings.Repeat("e", 15<<10)
	}
	m := decode(t, stored(t, marshal(t, body)))
	if m[droppedReasonKey] == nil {
		t.Fatal("not cut down")
	}
	cut, _ := m[truncatedFieldsKey].([]any)
	if !slices.Contains(cut, any("contact_title")) {
		t.Errorf("%s = %v, want contact_title listed", truncatedFieldsKey, m[truncatedFieldsKey])
	}
}

// A prospect who puts `","leadscore_secret":"x` into a field Apollo copies
// unescaped adds a second copy of the field; every copy is a candidate, so
// the real secret still authenticates, and neither copy is stored.
func TestEveryCopyOfTheSecretFieldIsACandidate(t *testing.T) {
	probe := `Dana","leadscore_secret":"x`
	broken := []byte(`{"event":"email_replied","contact_name":"` + probe + `","contact_email":"dana@example.com","leadscore_secret":"` + testSecret + `"}`)
	if got := scanSecret(broken); !slices.Contains(got, testSecret) || !slices.Contains(got, "x") {
		t.Errorf("broken JSON: candidates %q, want both copies", got)
	}
	// The same probe arriving as valid JSON with a duplicate top-level key.
	dup := []byte(`{"leadscore_secret":"x","event":"email_replied","leadscore_secret":"` + testSecret + `"}`)
	if got := scanSecret(dup); !slices.Contains(got, testSecret) {
		t.Errorf("duplicate keys: candidates %q, want the real secret among them", got)
	}
	// Anything after the object makes it text, and the text is searched too.
	trailing := []byte(`{"leadscore_secret":"x"} {"leadscore_secret":"` + testSecret + `"}`)
	if got := scanSecret(trailing); !slices.Contains(got, testSecret) {
		t.Errorf("trailing data: candidates %q, want the real secret among them", got)
	}
	for _, raw := range [][]byte{broken, dup, trailing} {
		out := string(storedBody(readBody(raw, []string{testSecret})))
		if strings.Contains(out, testSecret) || strings.Contains(out, `"leadscore_secret":"x`) {
			t.Errorf("a secret copy was stored: %s", out)
		}
	}
}

// A body with anything after its JSON object is not a JSON body.
func TestTrailingDataIsNotJSON(t *testing.T) {
	_, stage := storeBody(readBody([]byte(`{"event":"email_sent","contact_email":"a@example.com"} trailing`), nil))
	if stage != stageNotJSON {
		t.Errorf("stage = %q, want %q", stage, stageNotJSON)
	}
}

// A secret is masked in keys too, and in a body stored as text also in its
// JSON-escaped spellings, which a decoder would read back as the secret.
func TestSecretsAreMaskedInKeysAndEscapedForms(t *testing.T) {
	out := string(stored(t, []byte(`{"event":"email_sent","pasted `+testSecret+`":"v"}`)))
	if strings.Contains(out, testSecret) || !strings.Contains(out, "pasted [REDACTED]") {
		t.Errorf("a secret in a key was stored: %s", out)
	}
	var esc, escUpper strings.Builder
	for _, r := range testSecret {
		fmt.Fprintf(&esc, `\u%04x`, r)
		fmt.Fprintf(&escUpper, `\u%04X`, r)
	}
	for _, form := range []string{esc.String(), escUpper.String()} {
		raw := []byte(`{"event":"email_sent","note":"` + form + `", broken`)
		out := stored(t, raw)
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if text, _ := m[rawKey].(string); strings.Contains(text, form) {
			t.Errorf("an escaped secret was stored: %s", text)
		}
	}
}

// Many long keys whose values are cut make a long __truncated_fields list;
// each listed path is capped and the list goes before any parser field does.
func TestALongCutListNeverDropsParserFields(t *testing.T) {
	body := map[string]any{"event": "email_unsubscribed", "contact_email": "dana@example.com"}
	for i := range 50 {
		body[strings.Repeat("k", 2000)+strconv.Itoa(i)] = strings.Repeat("v", 17<<10)
	}
	out, stage := storeBody(readBody(marshal(t, body), nil))
	if !fits(out) {
		t.Fatalf("stored body = %d characters", utf8.RuneCount(out))
	}
	m := decode(t, out)
	if m["event"] != "email_unsubscribed" || m["contact_email"] != "dana@example.com" {
		t.Errorf("stage %q dropped a parser field: %v", stage, m)
	}
	if cut, ok := m[truncatedFieldsKey].([]any); ok {
		for _, c := range cut {
			if n := utf8.RuneCountInString(c.(string)); n > maxCutPathRunes {
				t.Errorf("a listed path is %d characters", n)
			}
		}
	}
}

// The read limit is 1 MB.
func TestTheReadLimitIsOneMB(t *testing.T) {
	if maxRequestBytes != 1<<20 {
		t.Errorf("maxRequestBytes = %d, want 1 MB", maxRequestBytes)
	}
}

// Each size fallback, reached on its own.
func TestEachSizeFallback(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"event": "email_replied", "contact_email": "a@example.com"}
	}
	// 1. Only long strings: capped, everything kept.
	capped := base()
	capped["past_conversations"] = strings.Repeat("p", 30<<10)
	// 2. Many fields the parsers do not read: only the parser fields kept.
	kept := base()
	for i := range 10 {
		kept["extra"+strconv.Itoa(i)] = strings.Repeat("e", 15<<10)
	}
	// 3. The parser fields themselves too long: shortened.
	short := base()
	for _, f := range []string{"contact_stage", "contact_name", "contact_title", "account_domain"} {
		short[f] = strings.Repeat("s", 15<<10)
	}
	// 4. A parser field that is not a string and too long: made a short string.
	nonString := base()
	nonString["contact_id"] = json.Number(strings.Repeat("9", 60000))
	for name, c := range map[string]struct {
		body map[string]any
		want string
	}{
		"capped": {capped, stageCapped}, "kept": {kept, stageKept}, "shortened": {short, stageKeptShort}, "non-string": {nonString, stageNonString},
	} {
		out, stage := storeBody(readBody(marshal(t, c.body), nil))
		if stage != c.want {
			t.Errorf("%s: stage %q, want %q", name, stage, c.want)
		}
		if !fits(out) {
			t.Errorf("%s: %d characters", name, utf8.RuneCount(out))
		}
		if m := decode(t, out); m["event"] != "email_replied" || m["contact_email"] != "a@example.com" {
			t.Errorf("%s: parser fields lost: %v", name, m)
		}
	}
}
