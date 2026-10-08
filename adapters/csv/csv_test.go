// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package csv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

var fixedNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newSource(t *testing.T, cfg api.Config) *Source {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%v): %v", cfg, err)
	}
	src := s.(*Source)
	src.now = func() time.Time { return fixedNow }
	return src
}

func fetch(t *testing.T, s *Source) ([]api.InputRow, []api.Event) {
	t.Helper()
	rows, events, next, err := s.Fetch(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if next != "" {
		t.Errorf("a snapshot source returns no cursor; got %q", next)
	}
	return rows, events
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.csv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegisteredAsCSV(t *testing.T) {
	f, ok := api.SourceFactory("csv")
	if !ok {
		t.Fatal("source type csv is not registered")
	}
	s, err := f(api.Config{"id": "conf", "type": "csv", "path": "testdata/leads_bom.csv", "events": false})
	if err != nil || s.ID() != "conf" {
		t.Fatalf("factory = %v, %v", s, err)
	}
}

func TestNewRefusesABadBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  api.Config
		want string
	}{
		{"no id", api.Config{"path": "a.csv"}, "`id`"},
		{"no path", api.Config{"id": "a"}, "`path`"},
		{"events not a bool", api.Config{"id": "a", "path": "a.csv", "events": "yes"}, "`events`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// The BOM is stripped, headers come back as written, ragged rows are padded,
// a stray cell past the last header is dropped, and blank lines are skipped.
func TestInputRowsFromABOMFile(t *testing.T) {
	rows, events := fetch(t, newSource(t, api.Config{"id": "conf", "path": "testdata/leads_bom.csv"}))
	if events != nil {
		t.Fatalf("a plain source returns no events; got %v", events)
	}
	headers := []string{"Email", "Full Name", "Company", "Notes"}
	want := []api.InputRow{
		{SourceID: "conf", Headers: headers, Columns: map[string]string{
			"Email": "priya@acme.example", "Full Name": "Priya Rao", "Company": "Acme", "Notes": "met at booth, keen"}},
		{SourceID: "conf", Headers: headers, Columns: map[string]string{
			"Email": "sam@globex.example", "Full Name": "Sam Lee", "Company": "", "Notes": ""}},
		{SourceID: "conf", Headers: headers, Columns: map[string]string{
			"Email": "jo@initech.example", "Full Name": "Jo Park", "Company": "Initech", "Notes": "ok"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows:\n got %#v\nwant %#v", rows, want)
	}
	if strings.HasPrefix(rows[0].Headers[0], "\xEF\xBB\xBF") {
		t.Error("the BOM leaked into the first header")
	}
}

// Parse only: no alias is applied, no email checked, values are kept as written.
func TestInputRowsAreRaw(t *testing.T) {
	path := writeFile(t, "Work Email,Email,Email\nnot-an-email, second ,third\n")
	rows, _ := fetch(t, newSource(t, api.Config{"id": "x", "path": path}))
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	r := rows[0]
	if !reflect.DeepEqual(r.Headers, []string{"Work Email", "Email", "Email"}) {
		t.Errorf("headers = %q", r.Headers)
	}
	// TrimLeadingSpace drops the space before "second"; the trailing one stays.
	want := map[string]string{"Work Email": "not-an-email", "Email": "second "}
	if !reflect.DeepEqual(r.Columns, want) {
		t.Errorf("columns = %q, want %q (a repeated header keeps its first value)", r.Columns, want)
	}
}

func TestHeaderOnlyFileHasNoRows(t *testing.T) {
	rows, events := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, "email,name\n")}))
	if len(rows) != 0 || events != nil {
		t.Fatalf("rows = %v, events = %v", rows, events)
	}
}

func TestFetchFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"empty file", "", "header row"},
		{"bare quote", "email,name\na@b.example,\"x\"y\n", "line 2"},
		{"not UTF-8", "email,name\na@b.example,Jos\xe9\n", "line 2 is not UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSource(t, api.Config{"id": "x", "path": writeFile(t, tc.body)})
			_, _, _, err := s.Fetch(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	s := newSource(t, api.Config{"id": "x", "path": filepath.Join(t.TempDir(), "missing.csv")})
	if _, _, _, err := s.Fetch(context.Background(), ""); err == nil {
		t.Error("a missing file must fail the fetch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := newSource(t, api.Config{"id": "x", "path": "testdata/leads_bom.csv"}).Fetch(ctx, ""); err == nil {
		t.Error("a cancelled context must fail the fetch")
	}
}

// An Apollo visitor export has no event column, so every row is visit_<source id>.
// Its headers resolve through the alias table: Website fills Domain, Last Visited
// At is the time, and the rest are Attrs.
func TestApolloVisitorExport(t *testing.T) {
	rows, events := fetch(t, newSource(t, api.Config{"id": "site", "path": "testdata/apollo_visitors.csv", "events": true}))
	if rows != nil {
		t.Fatalf("an events source returns no rows; got %v", rows)
	}
	want := []api.Event{
		{
			Kind: "visit_site", Email: "priya@acme.example", LinkedInURL: "https://www.linkedin.com/in/example-priya",
			// Passed through as written: merge reduces it to a host.
			Domain: "https://www.Acme.example/", At: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
			ReceivedAt: fixedNow, Origin: "site",
			Attrs: map[string]string{"firstname": "Priya", "lastname": "Rao", "title": "VP Engineering",
				"company": "Acme", "page": "/pricing"},
		},
		{
			Kind: "visit_site", Domain: "initech.example", At: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
			ReceivedAt: fixedNow, Origin: "site",
			Attrs: map[string]string{"company": "Initech", "page": "/docs"},
		},
		{
			Email: "sam@globex.example", Domain: "globex.example", ReceivedAt: fixedNow, Origin: "site",
			Attrs: map[string]string{"firstname": "Sam", "lastname": "Lee", "title": "CTO", "company": "Globex",
				"page": "/pricing", "reject": "line 4: `at` is not RFC 3339 or YYYY-MM-DD"},
		},
		{
			ReceivedAt: fixedNow, Origin: "site",
			Attrs: map[string]string{"page": "/blog", "reject": "line 5: no email, linkedin_url or domain"},
		},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events:\n got %#v\nwant %#v", events, want)
	}
}

const forbiddenReason = "a sent, reply, opt-out or deal kind may not come from a file"

func TestEventRows(t *testing.T) {
	_, events := fetch(t, newSource(t, api.Config{"id": "offline", "path": "testdata/events.csv", "events": true}))
	type got struct {
		kind, reject string
		at           time.Time
	}
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	want := []got{
		{kind: "visit_pricing", at: at},
		{kind: "demo_request", at: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)},
		{reject: "line 4: " + forbiddenReason},
		{reject: "line 5: " + forbiddenReason},
		{reject: "line 6: " + forbiddenReason},
		{reject: "line 7: no event kind"},
		{reject: "line 8: no `at` time"},
		{reject: "line 9: no email, linkedin_url or domain"},
		{kind: "company_visit", at: at},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(events), len(want), events)
	}
	for i, e := range events {
		g := got{kind: e.Kind, reject: e.Attrs["reject"], at: e.At}
		if g != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, g, want[i])
		}
		if e.ID != "" || e.Origin != "offline" || !e.ReceivedAt.Equal(fixedNow) {
			t.Errorf("event %d: id %q origin %q received %v", i, e.ID, e.Origin, e.ReceivedAt)
		}
	}
	if e := events[0]; e.Email != "priya@acme.example" || e.Attrs["page"] != "/pricing" {
		t.Errorf("first event = %+v", e)
	}
	// A column named reject must not mark a good row as rejected.
	if e := events[1]; e.LinkedInURL != "https://www.linkedin.com/in/example-sam" || e.Attrs["reject"] != "" {
		t.Errorf("second event = %+v", e)
	}
	// Domain is filled from the domain column only; never derived from an email.
	if events[0].Domain != "" || events[8].Domain != "initech.example" {
		t.Errorf("domains = %q, %q", events[0].Domain, events[8].Domain)
	}
}

func TestEventRowHeaders(t *testing.T) {
	// Aliased spellings, a repeated field (first wins), an offset time, and an
	// empty Attrs value left out.
	body := "Event,Visited At,Work Email,Email,Company Name,Notes\n" +
		"visit_home,2026-08-20T15:30:00.250+05:30,first@acme.example,second@acme.example,Acme,\n"
	_, events := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, body), "events": true}))
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	e := events[0]
	if e.Kind != "visit_home" || e.Email != "first@acme.example" {
		t.Errorf("event = %+v", e)
	}
	if want := time.Date(2026, 8, 20, 10, 0, 0, 250e6, time.UTC); !e.At.Equal(want) || e.At.Location() != time.UTC {
		t.Errorf("at = %v, want %v in UTC", e.At, want)
	}
	if !reflect.DeepEqual(e.Attrs, map[string]string{"company": "Acme"}) {
		t.Errorf("attrs = %v", e.Attrs)
	}
}

func TestForbiddenKinds(t *testing.T) {
	for _, k := range []string{"sent", "SENT", "replied", "replied_positive", "unsubscribed", "reply", "optout",
		"deal_open", "deal_won", "deal_lost"} {
		if !forbidden(k) {
			t.Errorf("%q must be forbidden", k)
		}
	}
	for _, k := range []string{"visit_pricing", "demo_request", "webinar", "dealer_visit"} {
		if forbidden(k) {
			t.Errorf("%q must be allowed", k)
		}
	}
}

// Reject reasons go to the row_rejected log, which must carry no email, so a
// reason never echoes a cell, even a kind that holds an address.
func TestRejectReasonsCarryNoCellValue(t *testing.T) {
	body := "event,at,email\n" +
		"replied a@x.example,2026-08-20,a@x.example\n" +
		"deal_jane@acmeco.example,2026-08-20,a@x.example\n" +
		"replied_jane@acmeco.example,2026-08-20,a@x.example\n" +
		"visit_x,jane@acmeco.example,a@x.example\n"
	_, events := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, body), "events": true}))
	_, fixture := fetch(t, newSource(t, api.Config{"id": "x", "path": "testdata/events.csv", "events": true}))
	_, apollo := fetch(t, newSource(t, api.Config{"id": "x", "path": "testdata/apollo_visitors.csv", "events": true}))
	for _, e := range append(append(events, fixture...), apollo...) {
		if r := e.Attrs["reject"]; strings.Contains(r, "@") || strings.Contains(r, "jane") {
			t.Errorf("reason %q echoes a cell", r)
		}
	}
	for i, e := range events {
		if e.Kind != "" || e.Attrs["reject"] == "" {
			t.Errorf("event %d must be rejected: %+v", i, e)
		}
	}
}

func TestKindsAreLowercasedAndPlain(t *testing.T) {
	body := "event,at,email\n" +
		"Visit_Pricing,2026-08-20,a@x.example\n" +
		"ſent,2026-08-20,a@x.example\n" + // long s: uppercases to S
		"se\u200bnt,2026-08-20,a@x.example\n" + // zero-width space
		"demo-request,2026-08-20,a@x.example\n"
	_, events := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, body), "events": true}))
	if events[0].Kind != "visit_pricing" {
		t.Errorf("kind = %q, want it lowercased", events[0].Kind)
	}
	for i, e := range events[1:] {
		want := fmt.Sprintf("line %d: an event kind may use only a-z, 0-9 and _", i+3)
		if e.Kind != "" || e.Attrs["reject"] != want {
			t.Errorf("line %d: kind %q reject %q, want %q", i+3, e.Kind, e.Attrs["reject"], want)
		}
	}
}

func TestEventsFileNeedsTimeAndPersonColumns(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no at column", "event,email\nvisit_x,a@x.example\n", "`at` column"},
		{"no person column", "event,at,page\nvisit_x,2026-08-20,/p\n", "email, linkedin_url or domain column"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSource(t, api.Config{"id": "x", "path": writeFile(t, tc.body), "events": true})
			_, _, _, err := s.Fetch(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// A header with no a-z0-9 keeps its text as the Attrs key instead of vanishing.
func TestUnsquashableHeaderKeepsItsText(t *testing.T) {
	body := "at,email,#,日本\n2026-08-20,a@x.example,7,東京\n"
	_, events := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, body), "events": true}))
	want := map[string]string{"#": "7", "日本": "東京"}
	if !reflect.DeepEqual(events[0].Attrs, want) {
		t.Errorf("attrs = %v, want %v", events[0].Attrs, want)
	}
}

func TestBadFilesFailWithAFix(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"CR-only line endings", "email,name\ra@x.example,A\rb@x.example,B\r", "line endings are CR only; save the file as CSV UTF-8"},
		{"blank header row", ",,\na@x.example,A,\n", "header row (line 1) is blank"},
		{"bare quote", "email,company\na@x.example,Acme \"Inc\"\n", `double the quote inside`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSource(t, api.Config{"id": "x", "path": writeFile(t, tc.body)})
			_, _, _, err := s.Fetch(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func setLimit[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func fetchErr(t *testing.T, body string) error {
	t.Helper()
	_, _, _, err := newSource(t, api.Config{"id": "x", "path": writeFile(t, body)}).Fetch(context.Background(), "")
	return err
}

func TestFileSizeLimit(t *testing.T) {
	setLimit(t, &maxFileBytes, 1<<20)
	body := "email\n" + strings.Repeat("a@x.example\n", (1<<20)/12+1)
	if err := fetchErr(t, body); err == nil || !strings.Contains(err.Error(), "larger than 1 MB") {
		t.Fatalf("err = %v", err)
	}
	if err := fetchErr(t, body[:1<<20]); err != nil {
		t.Fatalf("a file at the limit must read: %v", err)
	}
}

func TestColumnLimit(t *testing.T) {
	header := func(n int) string {
		h := make([]string, n)
		for i := range h {
			h[i] = fmt.Sprintf("c%d", i)
		}
		return strings.Join(h, ",") + "\nx\n"
	}
	if err := fetchErr(t, header(1001)); err == nil || !strings.Contains(err.Error(), "1001 columns, more than the 1000") {
		t.Fatalf("err = %v", err)
	}
	if err := fetchErr(t, header(1000)); err != nil {
		t.Fatalf("1000 columns must read: %v", err)
	}
}

// Rows times columns is capped, so many short rows under a wide header cannot
// fan out into a map entry per padded cell without bound.
func TestCellLimit(t *testing.T) {
	setLimit(t, &maxCells, 100)
	wide := "a,b,c,d,e,f,g,h,i,j\n"
	if err := fetchErr(t, wide+strings.Repeat("x\n", 11)); err == nil || !strings.Contains(err.Error(), "more than 100 cells") {
		t.Fatalf("err = %v", err)
	}
	if err := fetchErr(t, wide+strings.Repeat("x\n", 10)); err != nil {
		t.Fatalf("100 cells must read: %v", err)
	}
}

func TestRowsShareOneHeadersSlice(t *testing.T) {
	rows, _ := fetch(t, newSource(t, api.Config{"id": "conf", "path": "testdata/leads_bom.csv"}))
	if len(rows) < 2 || &rows[0].Headers[0] != &rows[1].Headers[0] {
		t.Fatal("rows must share one Headers slice, not a copy each")
	}
}

// Cells past the header are counted too, so one ragged line of commas cannot
// make the reader allocate millions of cells (the bound runs before parsing).
func TestRaggedRowsCountTowardTheCellLimit(t *testing.T) {
	huge := "a\nx" + strings.Repeat(",", maxCells) + "\n"
	if err := fetchErr(t, huge); err == nil || !strings.Contains(err.Error(), "cells") {
		t.Fatalf("a %d-cell ragged row must fail; err = %v", maxCells+1, err)
	}
	setLimit(t, &maxCells, 100)
	wide := "x" + strings.Repeat(",y", 30) + "\n" // 31 cells under a 1-column header
	if err := fetchErr(t, "a\n"+strings.Repeat(wide, 4)); err == nil || !strings.Contains(err.Error(), "more than 100 cells") {
		t.Fatalf("124 ragged cells must fail; err = %v", err)
	}
}

// A ragged row keeps only the cells under a header, in their own slice.
func TestRaggedRowKeepsOnlyHeaderCells(t *testing.T) {
	_, recs, err := parse([]byte("a,b\nx,y,z,w\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := recs[0].Cells; len(got) != 2 || cap(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("cells = %q (cap %d), want [x y] in a slice of its own", got, cap(got))
	}
}

func TestLineEndings(t *testing.T) {
	// A quoted header holding a newline is valid CSV.
	rows, _ := fetch(t, newSource(t, api.Config{"id": "x", "path": writeFile(t, "email,\"Notes\nfield\"\r\na@x.example,hi\r\n")}))
	if len(rows) != 1 || rows[0].Columns["Notes\nfield"] != "hi" {
		t.Fatalf("rows = %#v", rows)
	}
	// An LF header with CR-only data rows is still CR endings.
	if err := fetchErr(t, "email,name\na@x.example,A\rb@x.example,B\r"); err == nil || !strings.Contains(err.Error(), "CR only") {
		t.Fatalf("err = %v", err)
	}
}

func TestVisitKindObeysTheKindRule(t *testing.T) {
	_, events := fetch(t, newSource(t, api.Config{"id": "My-Site", "path": "testdata/apollo_visitors.csv", "events": true}))
	if events[0].Kind != "visit_my_site" {
		t.Fatalf("kind = %q, want visit_my_site", events[0].Kind)
	}
}
