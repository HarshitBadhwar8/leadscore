package sheets

import (
	"net/http/httptest"
	"testing"
	"time"

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

// A monthly tab is deleted only once its month and the grace hour have
// passed, every row in it is at or below the committed cursor, and its month
// ended before olderThan.
func TestDeleteProcessedRules(t *testing.T) {
	sep := time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 0, 10, 0, 0, time.UTC)
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC) // inside the grace hour
	s, _ := clockStore(t, &now)
	ctx := t.Context()
	ev := func(at time.Time) []api.RawEvent {
		return []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: at, Body: []byte("{}")}}
	}
	for _, at := range []time.Time{sep, sep, oct} {
		if err := s.AppendEvents(ctx, ev(at)); err != nil {
			t.Fatal(err)
		}
	}
	_, committed, err := s.ReadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if committed != "2026-09:2,2026-10:1" {
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
	if err := s.AppendEvents(ctx, ev(sep)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DeleteProcessed(ctx, committed, olderThan); got != committed {
		t.Errorf("a tab with unread rows: cursor %q, want nothing deleted", got)
	}
	_, committed, _ = s.ReadEvents(ctx, committed)
	got, err := s.DeleteProcessed(ctx, committed, olderThan)
	if err != nil || got != "2026-10:1" {
		t.Fatalf("DeleteProcessed = %q, %v; want September dropped", got, err)
	}
	book, _ := s.meta(ctx)
	if tabOf(book, "Events 2026-09") != nil || tabOf(book, "Events 2026-10") == nil {
		t.Error("September's tab must be gone and October's kept")
	}
	// The old cursor names the deleted tab: the log shrank below it.
	if _, _, err := s.ReadEvents(ctx, committed); err == nil {
		t.Error("a cursor naming a deleted tab must be ErrEventsShrank")
	}
}

func TestCursorForm(t *testing.T) {
	for _, bad := range []api.Cursor{"42", "2026-13:1", "2026-10", "2026-10:-1", "x:1"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("parseCursor(%q) accepted a bad cursor", bad)
		}
	}
	p, err := parseCursor("2026-10:3,2026-09:2")
	if err != nil || p.encode() != "2026-09:2,2026-10:3" {
		t.Errorf("round trip = %q, %v", p.encode(), err)
	}
}
