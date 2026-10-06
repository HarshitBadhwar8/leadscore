package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "leadscore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTableName(t *testing.T) {
	for in, want := range map[string]string{
		"People": "people", "Company facts": "company_facts", "Applied overrides": "applied_overrides",
		"Export warm": "export_warm", "State": "state",
	} {
		if got := TableName(in); got != want {
			t.Errorf("TableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisteredFactory(t *testing.T) {
	f, ok := api.BackendFactory("sqlite")
	if !ok {
		t.Fatal("the sqlite backend is not registered")
	}
	b, l, err := f(api.Config{"type": "sqlite", "path": filepath.Join(t.TempDir(), "x.db")})
	if err != nil || b == nil || l == nil {
		t.Fatalf("factory = %v, %v, %v", b, l, err)
	}
	b.(*Store).Close()
	if _, _, err := f(api.Config{"type": "sqlite"}); err == nil || !strings.Contains(err.Error(), "store.path") {
		t.Errorf("a missing path must name store.path, got %v", err)
	}
}

func TestPragmas(t *testing.T) {
	s := open(t)
	var mode string
	var timeout int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != int(busyTimeout.Milliseconds()) {
		t.Errorf("busy_timeout = %d, %v", timeout, err)
	}
}

// The RFC's unique keys and indexes, created with the tables.
func TestKeysAndIndexes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	var writes []api.TableWrite
	for _, name := range []string{model.TablePushes, model.TableSeenEvents, model.TableIdentities,
		model.TableAppliedRows, model.TableOutcomes, model.TableWindowEvents} {
		writes = append(writes, api.TableWrite{Table: name, Op: api.OpAppend, Rows: []api.Row{{}}})
	}
	if err := s.Commit(ctx, writes); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"pushes_key":        "CREATE UNIQUE INDEX \"pushes_key\" ON \"pushes\" (\"lead_id\", \"lane_id\", \"step\")",
		"seen_events_key":   "CREATE UNIQUE INDEX \"seen_events_key\" ON \"seen_events\" (\"event_key\")",
		"identities_key":    "CREATE UNIQUE INDEX \"identities_key\" ON \"identities\" (\"key\")",
		"applied_rows_key":  "CREATE UNIQUE INDEX \"applied_rows_key\" ON \"applied_rows\" (\"source_id\", \"row_id\")",
		"outcomes_key":      "CREATE UNIQUE INDEX \"outcomes_key\" ON \"outcomes\" (\"lead_id\")",
		"window_events_at":  "CREATE INDEX \"window_events_at\" ON \"window_events\" (\"at\")",
		"window_events_key": "CREATE UNIQUE INDEX \"window_events_key\" ON \"window_events\" (\"event_key\")",
	}
	for name, sql := range want {
		var got string
		if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&got); err != nil || got != sql {
			t.Errorf("index %s = %q, %v; want %q", name, got, err, sql)
		}
	}
	// A duplicate key is refused, and the whole commit with it.
	err := s.Commit(ctx, []api.TableWrite{
		{Table: model.TableState, Op: api.OpAppend, Rows: []api.Row{{"key": "k", "value": "v"}}},
		{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "e"}, {"event_key": "e"}}},
	})
	if err == nil {
		t.Error("appending a duplicate seen event must fail")
	}
	if rows, _ := s.ReadTable(ctx, model.TableState); len(rows) != 0 {
		t.Errorf("a failed commit left State rows: %v", rows)
	}
}

func TestColumnOrder(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Commit(ctx, []api.TableWrite{{Table: model.TableRanked, Op: api.OpReplace,
		Rows: []api.Row{{"lead_id": "L1", "tier": "A", "priority": "1", "zz_unknown": ""}}}})
	if err != nil {
		t.Fatal(err)
	}
	cols, err := columns(ctx, s.db, "ranked")
	if err != nil {
		t.Fatal(err)
	}
	want := "lead_id email linkedin_url full_name company_domain priority tier zz_unknown account_score contact_score " +
		"score status lane reasons rubric_version"
	if got := strings.Join(cols, " "); got != want {
		t.Errorf("ranked columns\n got %s\nwant %s", got, want)
	}
	err = s.Commit(ctx, []api.TableWrite{{Table: model.TableRanked, Op: api.OpAppend, Rows: []api.Row{{"lead_id": "L2", "fit": "x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	cols, _ = columns(ctx, s.db, "ranked")
	if cols[len(cols)-1] != "fit" {
		t.Errorf("an added column goes last: %v", cols)
	}
}

func TestCommitRefusesEvents(t *testing.T) {
	s := open(t)
	err := s.Commit(context.Background(), []api.TableWrite{{Table: "Events", Op: api.OpAppend, Rows: []api.Row{{"kind": "x"}}}})
	if err == nil {
		t.Error("Events must be written only through AppendEvents")
	}
}

func TestBadCursor(t *testing.T) {
	s := open(t)
	if _, _, err := s.ReadEvents(context.Background(), "Events 2026-10:4"); err == nil {
		t.Error("a cursor that is not a sequence number must fail")
	}
	if _, err := s.DeleteProcessed(context.Background(), "-1", time.Now()); err == nil {
		t.Error("a negative cursor must fail")
	}
	evs, next, err := s.ReadEvents(context.Background(), "")
	if err != nil || len(evs) != 0 || next != "" {
		t.Errorf("reading an empty store = %v, %q, %v", evs, next, err)
	}
}

// serve appends while a run holds the write lock: the append waits, then lands.
func TestAppendWaitsForAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadscore.db")
	run, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	serve, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer serve.Close()
	ctx := context.Background()
	tx, err := run.db.BeginTx(ctx, nil) // immediate: holds the write lock
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE "people" ("lead_id" TEXT)`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		tx.Commit()
	}()
	start := time.Now()
	if err := serve.AppendEvents(ctx, []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: time.Now(), Body: []byte("{}")}}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("the append did not wait for the lock")
	}
	evs, _, err := run.ReadEvents(ctx, "")
	if err != nil || len(evs) != 1 {
		t.Errorf("the run reads %d events, %v", len(evs), err)
	}
}

func TestMarkOpenedBy(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := markOpenedBy(ctx, s, "c0ffee"); err != nil {
		t.Fatal(err)
	}
	if err := markOpenedBy(ctx, s, "beef"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ReadTable(ctx, model.TableState)
	if len(rows) != 1 || rows[0]["key"] != "opened_by" || rows[0]["value"] != "beef" {
		t.Errorf("State = %v", rows)
	}
	if err := MarkOpenedBy(ctx, s); err != nil {
		t.Errorf("MarkOpenedBy: %v", err)
	}
	if err := markOpenedBy(ctx, otherBackend{}, "x"); err != nil {
		t.Errorf("another store is left alone: %v", err)
	}
}

type otherBackend struct{}

func (otherBackend) ReadTable(context.Context, string) ([]api.Row, error) { return nil, nil }
func (otherBackend) Lease(context.Context, string, time.Duration) (api.RunLease, error) {
	return nil, nil
}
func (otherBackend) Commit(context.Context, []api.TableWrite) error {
	panic("MarkOpenedBy must not write another store")
}
