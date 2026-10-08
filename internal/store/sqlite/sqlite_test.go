package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	t.Cleanup(func() { _ = s.Close() })
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
	_ = b.(*Store).Close()
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

// The store's unique keys and indexes, created with the tables.
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
	defer func() { _ = run.Close() }()
	serve, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serve.Close() }()
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
		_ = tx.Commit()
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
	// Outside a container, serve clears opened_by; inside, it writes its hostname.
	defer func(f func() bool) { InContainer = f }(InContainer)
	InContainer = func() bool { return false }
	if err := MarkOpenedBy(ctx, s); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ReadTable(ctx, model.TableState); rows[0]["value"] != "" {
		t.Errorf("outside a container opened_by = %q, want cleared", rows[0]["value"])
	}
	InContainer = func() bool { return true }
	if err := MarkOpenedBy(ctx, s); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if rows, _ := s.ReadTable(ctx, model.TableState); rows[0]["value"] != host {
		t.Errorf("inside a container opened_by = %q, want %q", rows[0]["value"], host)
	}
	if err := markOpenedBy(ctx, otherBackend{}, "x"); err != nil {
		t.Errorf("another store is left alone: %v", err)
	}
}

// SQLite column names ignore case: a row naming "Score" writes the existing
// "score" column instead of failing every commit.
func TestColumnCase(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Commit(ctx, []api.TableWrite{{Table: "Export warm", Op: api.OpAppend, Rows: []api.Row{{"lead_id": "L1", "Score": "1"}}}})
	if err != nil {
		t.Fatalf("a known column in another case: %v", err)
	}
	err = s.Commit(ctx, []api.TableWrite{
		{Table: "Export warm", Op: api.OpUpsert, Key: []string{"LEAD_ID"}, Rows: []api.Row{{"LEAD_ID": "L1", "score": "2", "Extra": "x"}}},
		{Table: "Export warm", Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{{"lead_id": "L1", "EXTRA": "y"}}},
	})
	if err != nil {
		t.Fatalf("columns in another case than stored: %v", err)
	}
	rows, _ := s.ReadTable(ctx, "Export warm")
	if len(rows) != 1 || rows[0]["score"] != "2" || rows[0]["Extra"] != "y" || rows[0]["lead_id"] != "L1" {
		t.Errorf("rows = %v", rows)
	}
	err = s.Commit(ctx, []api.TableWrite{{Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"Note": "a"}, {"note": "b"}}}})
	if err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Errorf("one write naming Note and note = %v, want refused", err)
	}
}

func TestRefusedNames(t *testing.T) {
	s := open(t)
	for name, w := range map[string]api.TableWrite{
		"rowid":        {Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"rowid": "1"}}},
		"OID":          {Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"OID": "1"}}},
		"_rowid_ key":  {Table: model.TablePeople, Op: api.OpDelete, Key: []string{"_RowID_"}, Rows: []api.Row{{}}},
		"empty column": {Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"": "1"}}},
		"NUL column":   {Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"a\x00b": "1"}}},
		"NUL table":    {Table: "Log\x00", Op: api.OpAppend, Rows: []api.Row{{"a": "1"}}},
		"empty table":  {Table: "", Op: api.OpAppend, Rows: []api.Row{{"a": "1"}}},
		"oid trim":     {Table: model.TableLog, Op: api.OpTrim, Column: "oid", Before: time.Now()},
	} {
		if err := s.Commit(context.Background(), []api.TableWrite{w}); err == nil {
			t.Errorf("%s: Commit must refuse it", name)
		}
	}
}

// Lane ids hold letters, digits, "-" and "_" and are unique ignoring case (the
// rubric's lane-id rule; the model refuses the rest). Over such ids TableName
// is one to one: two lanes share a SQLite table exactly when they match
// ignoring case, which the model refuses.
func TestTableNameOneToOne(t *testing.T) {
	lanes := []string{"a-b", "a_b", "ab", "a__b", "a--b", "a-_b", "a_-b", "facts", "applied", "Warm", "warm",
		"my_lane", "my-lane", "A1", "a1", "rows", "x"}
	for i, a := range lanes {
		for _, b := range lanes[i+1:] {
			same := TableName(model.ExportTable(a)) == TableName(model.ExportTable(b))
			if same != strings.EqualFold(a, b) {
				t.Errorf("lanes %q and %q: same table = %v", a, b, same)
			}
		}
		for _, d := range model.Tables {
			if !d.Pattern && TableName(d.Name) == TableName(model.ExportTable(a)) {
				t.Errorf("lane %q shares %s with table %s", a, TableName(d.Name), d.Name)
			}
		}
	}
	// The colliding cases the model refuses, so they never reach the store.
	m := model.New()
	if err := m.Put(model.ExportTable("my lane"), model.ExportRow{LeadID: "L1"}); err == nil {
		t.Error(`lane "my lane" (TableName export_my_lane, like my_lane) must be refused`)
	}
	if err := m.Put(model.ExportTable("warm"), model.ExportRow{LeadID: "L1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(model.ExportTable("Warm"), model.ExportRow{LeadID: "L1"}); err == nil {
		t.Error(`lane "Warm" after "warm" must be refused`)
	}
}

func TestOpenRefusesDSNPaths(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{filepath.Join(dir, "a?mode=ro"), "file:" + filepath.Join(dir, "a.db"), "FILE:x.db", ""} {
		if s, err := Open(p); err == nil {
			_ = s.Close()
			t.Errorf("Open(%q) must be refused", p)
		}
	}
}

func TestFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file modes")
	}
	path := filepath.Join(t.TempDir(), "leadscore.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.AppendEvents(context.Background(), []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), mode)
		}
	}
}

func TestExistingFileMadeOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file modes")
	}
	path := filepath.Join(t.TempDir(), "leadscore.db")
	for _, p := range []string{path, path + "-wal"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil { // past the umask
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 600", filepath.Base(p), fi.Mode().Perm(), err)
		}
	}
}

func TestRefusedArguments(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.AppendEvents(ctx, []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: time.Now()}, {Kind: "apollo_reply"}}); err == nil {
		t.Error("an event with no received time must be refused")
	}
	if evs, _, _ := s.ReadEvents(ctx, ""); len(evs) != 0 {
		t.Error("a refused batch must store nothing")
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := s.Lease(ctx, "run", ttl); err == nil {
			t.Errorf("Lease with ttl %v must be refused", ttl)
		}
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

// OpenReadOnly takes a relative path (made absolute: a relative file: URI
// would read as an authority) and a "#" in the name, as Open does.
func TestOpenReadOnlyPaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, name := range []string{"store.db", "team #2.db", filepath.Join("sub dir", "x#y.db")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		w, err := Open(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := w.Commit(context.Background(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
			Rows: []api.Row{{"key": "schema_version", "value": "1.0"}}}}); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		r, err := OpenReadOnly(name)
		if err != nil {
			t.Fatalf("%s: OpenReadOnly: %v", name, err)
		}
		rows, err := r.ReadTable(context.Background(), model.TableState)
		_ = r.Close()
		if err != nil || len(rows) != 1 || rows[0]["value"] != "1.0" {
			t.Errorf("%s: rows %v, %v", name, rows, err)
		}
	}
	if _, err := OpenReadOnly("missing.db"); err == nil {
		t.Error("OpenReadOnly opened a file that does not exist")
	}
}
