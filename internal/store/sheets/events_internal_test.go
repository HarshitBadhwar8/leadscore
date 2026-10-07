package sheets

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
)

func clockStore(t *testing.T, now *time.Time) (*Store, *fakesheets.Server) {
	t.Helper()
	fs := fakesheets.New()
	srv := httptest.NewServer(gcs.Route(gcs.New(), fs))
	t.Cleanup(srv.Close)
	s, err := Open(t.Context(), api.Config{"spreadsheet": fs.NewSpreadsheet("x"), "base_url": srv.URL, "_http_client": srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return *now }
	return s, fs
}

func visit(at time.Time) []api.RawEvent {
	return []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: at, Body: []byte(at.Format(time.RFC3339))}}
}

// months strips the row ids from a cursor: "2026-09:2,2026-10:1".
func months(c api.Cursor) string {
	p, _ := parseCursor(c)
	var out []string
	for _, m := range slices.Sorted(maps.Keys(p)) {
		out = append(out, m+":"+strconv.Itoa(p[m].n))
	}
	return strings.Join(out, ",")
}

// A monthly tab is deleted only once its month and the grace hour have
// passed, every row in it is at or below the committed cursor, its month
// ended before olderThan, and it is not the newest Events tab.
func TestDeleteProcessedRules(t *testing.T) {
	sep := time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 0, 10, 0, 0, time.UTC)
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC) // inside the grace hour
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	for _, at := range []time.Time{sep, sep, oct} {
		if err := s.AppendEvents(ctx, visit(at)); err != nil {
			t.Fatal(err)
		}
	}
	_, committed, err := s.ReadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if months(committed) != "2026-09:2,2026-10:1" {
		t.Fatalf("cursor = %q", committed)
	}
	olderThan := oct.Add(time.Hour)
	if got, _ := s.DeleteProcessed(ctx, committed, olderThan); got != committed {
		t.Errorf("inside the grace hour: cursor %q, want %q (nothing deleted)", got, committed)
	}
	now = now.Add(time.Hour)
	if got, _ := s.DeleteProcessed(ctx, committed, sep); got != committed {
		t.Errorf("olderThan inside September: cursor %q, want nothing deleted", got)
	}
	// A late September event past the committed cursor keeps the tab.
	if err := s.AppendEvents(ctx, visit(sep)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DeleteProcessed(ctx, committed, olderThan); got != committed {
		t.Errorf("a tab with unread rows: cursor %q, want nothing deleted", got)
	}
	_, committed, _ = s.ReadEvents(ctx, committed)
	got, err := s.DeleteProcessed(ctx, committed, olderThan)
	if err != nil || months(got) != "2026-10:1" {
		t.Fatalf("DeleteProcessed = %q, %v; want September dropped", got, err)
	}
	book, _ := s.meta(ctx)
	if tabOf(book, "Events 2026-09") != nil || tabOf(book, "Events 2026-10") == nil {
		t.Error("September's tab must be gone and October's kept")
	}
	// The newest tab is never deleted, however old.
	now = now.AddDate(0, 3, 0)
	if again, _ := s.DeleteProcessed(ctx, got, now); again != got {
		t.Errorf("the newest Events tab was deleted: cursor %q", again)
	}
}

// A crash after DeleteProcessed and before the engine saved the shorter
// cursor: the next run reads from the old cursor, which names the deleted
// tab. The deletion was recorded with it, so the read goes on.
func TestCrashAfterDeleteProcessed(t *testing.T) {
	sep := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	for _, at := range []time.Time{sep, oct} {
		if err := s.AppendEvents(ctx, visit(at)); err != nil {
			t.Fatal(err)
		}
	}
	_, saved, _ := s.ReadEvents(ctx, "")
	if _, err := s.DeleteProcessed(ctx, saved, now); err != nil {
		t.Fatal(err)
	}
	// Crash: the shorter cursor was never saved. A new event arrives.
	late := oct.Add(time.Hour)
	if err := s.AppendEvents(ctx, visit(late)); err != nil {
		t.Fatal(err)
	}
	evs, next, err := s.ReadEvents(ctx, saved)
	if err != nil {
		t.Fatalf("reading from the cursor saved before the delete: %v", err)
	}
	if len(evs) != 1 || !evs[0].ReceivedAt.Equal(late) || strings.Contains(string(next), "2026-09") {
		t.Errorf("events %v, cursor %q; want only the new event, and September dropped", evs, next)
	}
	// A tab that vanished without being recorded is still a shrink.
	if _, _, err := s.ReadEvents(ctx, "2026-08:3:abc"); !errors.Is(err, api.ErrEventsShrank) {
		t.Errorf("an unrecorded missing tab = %v, want ErrEventsShrank", err)
	}
}

// Sorting or inserting rows in an Events tab moves the row under the cursor:
// the read refuses (ErrEventsShrank) instead of skipping or re-reading events.
func TestEventRowsMoved(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for name, move := range map[string]func(fs *fakesheets.Server, id string){
		"sorted":   func(fs *fakesheets.Server, id string) { fs.SwapRows(id, "Events 2026-10", 2, 3) },
		"inserted": func(fs *fakesheets.Server, id string) { fs.InsertRow(id, "Events 2026-10", 1, []string{"x", "y"}) },
	} {
		t.Run(name, func(t *testing.T) {
			s, fs := clockStore(t, &now)
			ctx := t.Context()
			for i := range 3 {
				if err := s.AppendEvents(ctx, visit(now.Add(time.Duration(i)*time.Minute))); err != nil {
					t.Fatal(err)
				}
			}
			_, cursor, _ := s.ReadEvents(ctx, "")
			move(fs, s.id)
			if _, _, err := s.ReadEvents(ctx, cursor); !errors.Is(err, api.ErrEventsShrank) {
				t.Errorf("read after rows were %s = %v, want ErrEventsShrank", name, err)
			}
		})
	}
}

// Rows deleted below the cursor: Sheets refuses a range under the grid, so
// the store compares the cursor with the tab's row count first and returns
// ErrEventsShrank rather than a plain error.
func TestEventRowsDeleted(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	for i := range 4 {
		if err := s.AppendEvents(ctx, visit(now.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor, _ := s.ReadEvents(ctx, "")
	book, _ := s.meta(ctx)
	id := tabOf(book, "Events 2026-10").Properties.SheetId
	if err := s.batchUpdate(ctx, []*sheetsapi.Request{{DeleteDimension: &sheetsapi.DeleteDimensionRequest{
		Range: &sheetsapi.DimensionRange{SheetId: id, Dimension: "ROWS", StartIndex: 2, EndIndex: 5}}}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadEvents(ctx, cursor); !errors.Is(err, api.ErrEventsShrank) {
		t.Errorf("read after rows were deleted = %v, want ErrEventsShrank", err)
	}
}

// A person-made tab named with other case ("events 2026-11") is the month's
// tab: Sheets ignores case in tab names, so creating "Events 2026-11" would
// fail forever.
func TestEventsTabNameCase(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, fs := clockStore(t, &now)
	fs.Put(s.id, "events 2026-11", [][]any{{"seq", "received_at", "kind", "body"}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // a refused tab name would retry forever
	defer cancel()
	if err := s.AppendEvents(ctx, visit(now)); err != nil {
		t.Fatal(err)
	}
	evs, _, err := s.ReadEvents(ctx, "")
	if err != nil || len(evs) != 1 {
		t.Errorf("ReadEvents = %d, %v", len(evs), err)
	}
}

// When no protected Events tab is left, a new one gets the tool tabs'
// editors plus the appending account, as Drive names it.
func TestNewEventsTabProtectionFallback(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, fs := clockStore(t, &now)
	ctx := t.Context()
	fs.SetCaller("recv@p.iam.gserviceaccount.com")
	commitOrFail := func(reqs ...*sheetsapi.Request) {
		if err := s.batchUpdate(ctx, reqs, 1); err != nil {
			t.Fatal(err)
		}
	}
	commitOrFail(addTab(77, "People", 7, true), protectRequest(77, []string{"run@p.iam.gserviceaccount.com"}, toolTabNote))
	if err := s.AppendEvents(ctx, visit(now)); err != nil {
		t.Fatal(err)
	}
	book, _ := s.meta(ctx)
	prs := tabOf(book, "Events 2026-11").ProtectedRanges
	if len(prs) != 1 || !slices.Equal(prs[0].Editors.Users, []string{"run@p.iam.gserviceaccount.com", "recv@p.iam.gserviceaccount.com"}) {
		t.Errorf("protection = %+v", prs)
	}
	if !tabOf(book, "Events 2026-11").Properties.Hidden {
		t.Error("a new Events tab must be hidden")
	}
}

func TestCursorForm(t *testing.T) {
	for _, bad := range []api.Cursor{"42", "2026-13:1", "2026-10", "2026-10:-1", "x:1", "2026-10:1:a:b"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("parseCursor(%q) accepted a bad cursor", bad)
		}
	}
	p, err := parseCursor("2026-10:3:ab,2026-09:2:cd")
	if err != nil || p.encode() != "2026-09:2:cd,2026-10:3:ab" {
		t.Errorf("round trip = %q, %v", p.encode(), err)
	}
}

// The 10-million-cell answer is ErrTooLarge too.
func TestTooLargeAnswers(t *testing.T) {
	for msg, want := range map[string]bool{
		"This action would increase the number of cells in the workbook above the limit of 10000000 cells.": true,
		"Request payload size exceeds the limit: 10485760 bytes.":                                           true,
		"Unable to parse range: Foo": false,
	} {
		if got := tooLarge(&googleapi.Error{Code: http.StatusBadRequest, Message: msg}); got != want {
			t.Errorf("tooLarge(%q) = %v", msg, got)
		}
	}
}

// A quiet month: October's events were all read and are old, November had
// none, and December's first event arrives. October's tab is the newest, so
// DeleteProcessed keeps it, and December's tab copies its protection (both
// accounts) rather than ending up unprotected.
func TestQuietMonthKeepsProtection(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	acc := Accounts{Run: "run@p.iam.gserviceaccount.com", Receiver: "recv@p.iam.gserviceaccount.com"}
	id, err := Create(ctx, s.svc, Template{Title: "leadscore", Accounts: acc, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	s = New(s.svc, id, "")
	s.now = func() time.Time { return now }
	if err := s.AppendEvents(ctx, visit(now)); err != nil {
		t.Fatal(err)
	}
	_, committed, _ := s.ReadEvents(ctx, "")
	now = time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	if got, err := s.DeleteProcessed(ctx, committed, now); err != nil || got != committed {
		t.Fatalf("DeleteProcessed = %q, %v; the newest Events tab must stay", got, err)
	}
	if err := s.AppendEvents(ctx, visit(now)); err != nil {
		t.Fatal(err)
	}
	book, _ := s.meta(ctx)
	prs := tabOf(book, "Events 2026-12").ProtectedRanges
	if len(prs) != 1 || !slices.Equal(prs[0].Editors.Users, acc.List()) {
		t.Errorf("December's protection = %+v", prs)
	}
}

// A month DeleteProcessed recorded as deleted, then made again by a lagging
// receiver: the old cursor's mark is for the deleted tab, so the new tab is
// read whole; a mark taken from the new tab is honored after that.
func TestRecreatedMonth(t *testing.T) {
	sep := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	for _, at := range []time.Time{sep, sep, oct} {
		if err := s.AppendEvents(ctx, visit(at)); err != nil {
			t.Fatal(err)
		}
	}
	_, saved, _ := s.ReadEvents(ctx, "")
	if _, err := s.DeleteProcessed(ctx, saved, now); err != nil {
		t.Fatal(err)
	}
	late := sep.Add(time.Hour)
	if err := s.AppendEvents(ctx, visit(late)); err != nil { // a lagging receiver re-creates September
		t.Fatal(err)
	}
	evs, next, err := s.ReadEvents(ctx, saved)
	if err != nil || len(evs) != 1 || !evs[0].ReceivedAt.Equal(late) {
		t.Fatalf("read from the old cursor = %v, %v; want the re-created tab's event", evs, err)
	}
	if again, _, err := s.ReadEvents(ctx, next); err != nil || len(again) != 0 {
		t.Errorf("read from the new cursor = %v, %v; want nothing (the mark is the new tab's)", again, err)
	}
	if err := s.AppendEvents(ctx, visit(late)); err != nil {
		t.Fatal(err)
	}
	if more, _, err := s.ReadEvents(ctx, next); err != nil || len(more) != 1 {
		t.Errorf("read after one more append = %v, %v; want just it", more, err)
	}
}

// Drive cannot say who is appending: a new Events tab with no protected
// sibling fails loudly rather than being protected against its own writer.
func TestNewEventsTabCallerUnknown(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, fs := clockStore(t, &now)
	ctx := t.Context()
	fs.FailAbout(true)
	if err := s.batchUpdate(ctx, []*sheetsapi.Request{addTab(77, "People", 7, true),
		protectRequest(77, []string{"run@p.iam.gserviceaccount.com"}, toolTabNote)}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents(ctx, visit(now)); err == nil || !strings.Contains(err.Error(), "signed in") {
		t.Errorf("AppendEvents = %v; want a failure naming the unknown account", err)
	}
	book, _ := s.meta(ctx)
	if tabOf(book, "Events 2026-11") != nil {
		t.Error("no Events tab may be created unprotected for its writer")
	}
}
