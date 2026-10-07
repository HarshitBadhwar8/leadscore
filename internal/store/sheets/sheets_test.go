package sheets_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

func tabNamed(t *testing.T, book *sheetsapi.Spreadsheet, name string) *sheetsapi.Sheet {
	t.Helper()
	for _, sh := range book.Sheets {
		if sh.Properties.Title == name {
			return sh
		}
	}
	t.Fatalf("no tab %q", name)
	return nil
}

func commit(t *testing.T, s *sheets.Store, writes ...api.TableWrite) {
	t.Helper()
	if err := s.Commit(t.Context(), writes); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// A run's phase writes many tables; the Sheets store sends them as one
// batchUpdate, which Sheets applies all-or-nothing.
func TestOneBatchUpdatePerCommit(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	m, err := codec.Load(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: at})
	m.Put(model.TableIdentities, model.Identity{Key: "a@x.example", Kind: "email", LeadID: "L1", SourceID: "csv", FirstSeenAt: at})
	m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: "e1", FirstReceivedAt: at, RunID: "r1"})
	m.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "last_result", Value: "healthy", UpdatedAt: at})
	m.SetState("cursor:csv", "1")
	before := f.Sheets.Calls("batchUpdate")
	w := codec.Encode(m)
	if err := s.Commit(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if n := f.Sheets.Calls("batchUpdate") - before; n != 1 {
		t.Errorf("one commit sent %d batchUpdates, want 1", n)
	}
	// A commit of nothing new sends none.
	m.Committed(w)
	before = f.Sheets.Calls("batchUpdate")
	if err := s.Commit(t.Context(), codec.Encode(m)); err != nil {
		t.Fatal(err)
	}
	if n := f.Sheets.Calls("batchUpdate") - before; n != 0 {
		t.Errorf("an empty commit sent %d batchUpdates", n)
	}
}

// Tabs are created by their first write at exactly their column count, with
// a frozen header row; Health is eight columns wide so H1 can hold the
// formula; Ranked's derived columns sit after company_domain.
func TestTabsCreatedAtExactWidth(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	commit(t, s,
		api.TableWrite{Table: model.TablePeople, Op: api.OpAppend, Rows: []api.Row{{"lead_id": "L1"}}},
		api.TableWrite{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"},
			Rows: []api.Row{{"kind": "result", "key": "last_result", "value": "healthy"}}},
		api.TableWrite{Table: model.TableRanked, Op: api.OpReplace, Rows: []api.Row{{"lead_id": "L1", "tier": "A", "priority": "P1"}}},
	)
	book := f.Sheets.Spreadsheet(s.SpreadsheetID())
	for name, width := range map[string]int64{model.TablePeople: 7, model.TableHealth: 8, model.TableRanked: 14} {
		gp := tabNamed(t, book, name).Properties.GridProperties
		if gp.ColumnCount != width || gp.FrozenRowCount != 1 {
			t.Errorf("%s: %d columns, %d frozen rows; want %d and 1", name, gp.ColumnCount, gp.FrozenRowCount, width)
		}
	}
	vr, err := s.Services().Sheets.Spreadsheets.Values.Get(s.SpreadsheetID(), "Ranked!1:1").Do()
	if err != nil {
		t.Fatal(err)
	}
	var header []string
	for _, v := range vr.Values[0] {
		header = append(header, v.(string))
	}
	want := []string{"lead_id", "email", "linkedin_url", "full_name", "company_domain", "priority", "tier",
		"account_score", "contact_score", "score", "status", "lane", "reasons", "rubric_version"}
	if !slices.Equal(header, want) {
		t.Errorf("Ranked header = %q, want %q", header, want)
	}
	// A later column is appended at the end, widening the tab by one.
	commit(t, s, api.TableWrite{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"},
		Rows: []api.Row{{"lead_id": "L1", "new_col": "x"}}})
	if gp := tabNamed(t, f.Sheets.Spreadsheet(s.SpreadsheetID()), model.TablePeople).Properties.GridProperties; gp.ColumnCount != 8 {
		t.Errorf("People after an added column: %d columns, want 8", gp.ColumnCount)
	}
}

// Every commit that writes Health rewrites the H1 formula, naming the cell
// that holds last_success_at and the schedule from Health's schedule row.
func TestHealthFormulaFollowsLastSuccess(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	health := func(rows ...api.Row) api.TableWrite {
		return api.TableWrite{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"}, Rows: rows}
	}
	commit(t, s, health(api.Row{"kind": "result", "key": "last_result", "value": "healthy"}))
	if got := f.Sheets.Cell(s.SpreadsheetID(), model.TableHealth, "H1"); got != `="`+sheets.NoSuccessMessage+`"` {
		t.Errorf("H1 before any success = %q", got)
	}
	commit(t, s, health(
		api.Row{"kind": "result", "key": "schedule", "value": "1h"},
		api.Row{"kind": "result", "key": "last_success_at", "value": "2026-10-07T10:00:00.000Z"}))
	want := `=IF(NOW()-DATEVALUE(LEFT(C4,10))-TIMEVALUE(MID(C4,12,8))>3*0.0416666667,"STALE: no successful run in 3 intervals","ok")`
	if got := f.Sheets.Cell(s.SpreadsheetID(), model.TableHealth, "H1"); got != want {
		t.Errorf("H1 =\n %s\nwant\n %s", got, want)
	}
	if !f.Sheets.IsFormula(s.SpreadsheetID(), model.TableHealth, "H1") {
		t.Error("H1 must be written as a formula (USER_ENTERED)")
	}
	// Rows above move; the formula follows. Without a schedule row: 15 minutes.
	commit(t, s, api.TableWrite{Table: model.TableHealth, Op: api.OpDelete, Key: []string{"kind", "key"},
		Rows: []api.Row{{"kind": "result", "key": "last_result"}, {"kind": "result", "key": "schedule"}}})
	want = `=IF(NOW()-DATEVALUE(LEFT(C2,10))-TIMEVALUE(MID(C2,12,8))>3*0.0104166667,"STALE: no successful run in 3 intervals","ok")`
	if got := f.Sheets.Cell(s.SpreadsheetID(), model.TableHealth, "H1"); got != want {
		t.Errorf("H1 after rows moved =\n %s\nwant\n %s", got, want)
	}
	// The formula is not a column of the table.
	rows, err := s.ReadTable(t.Context(), model.TableHealth)
	if err != nil || len(rows) != 1 || len(rows[0]) != 5 {
		t.Errorf("Health = %v, %v; want one five-column row", rows, err)
	}
}

// Tool tabs are written raw: a value that starts with "=" is text, never a
// formula. Tool tabs are read unformatted and people-owned tabs formatted.
func TestRawWritesAndValueForms(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	commit(t, s, api.TableWrite{Table: model.TableLog, Op: api.OpAppend, Rows: []api.Row{{"at": "x", "message": "=HYPERLINK(\"http://evil\")"}}})
	if f.Sheets.IsFormula(s.SpreadsheetID(), model.TableLog, "G2") {
		t.Error("a value starting with = was stored as a formula")
	}
	if err := f.Sheets.Put(s.SpreadsheetID(), model.TableOverrides, [][]any{
		{"person", "action", "value", "note"}, {"a@x.example", "status", "do_not_contact", 42}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{model.TableLog, model.TableOverrides} {
		if _, err := s.ReadTable(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	reads := strings.Join(f.Sheets.ValueReads(), "\n")
	for _, want := range []string{"UNFORMATTED_VALUE 'Log'", "FORMATTED_VALUE 'Overrides'"} {
		if !strings.Contains(reads, want) {
			t.Errorf("reads %q lack %q", reads, want)
		}
	}
	rows, _ := s.ReadTable(t.Context(), model.TableOverrides)
	if len(rows) != 1 || rows[0]["note"] != "42" {
		t.Errorf("Overrides = %v", rows)
	}
}

// Rewriting a tab keeps its sheet id, so protection, filters and team
// formulas on it survive.
func TestRewriteKeepsSheetID(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	rows := func(n int) []api.Row {
		var out []api.Row
		for i := range n {
			out = append(out, api.Row{"lead_id": fmt.Sprintf("L%d", i), "score": "1"})
		}
		return out
	}
	commit(t, s, api.TableWrite{Table: model.TableRanked, Op: api.OpReplace, Rows: rows(5)})
	id := tabNamed(t, f.Sheets.Spreadsheet(s.SpreadsheetID()), model.TableRanked).Properties.SheetId
	for _, n := range []int{2, 0, 7} {
		commit(t, s, api.TableWrite{Table: model.TableRanked, Op: api.OpReplace, Rows: rows(n)})
		got, err := s.ReadTable(t.Context(), model.TableRanked)
		if err != nil || len(got) != n {
			t.Fatalf("Ranked after replacing with %d rows: %d rows, %v", n, len(got), err)
		}
		if again := tabNamed(t, f.Sheets.Spreadsheet(s.SpreadsheetID()), model.TableRanked).Properties.SheetId; again != id {
			t.Fatalf("Ranked's sheet id changed from %d to %d", id, again)
		}
	}
	// Deleting every row and appending again works (Sheets keeps one row).
	commit(t, s, api.TableWrite{Table: model.TableRanked, Op: api.OpDelete, Key: []string{"lead_id"}, Rows: rows(7)})
	commit(t, s, api.TableWrite{Table: model.TableRanked, Op: api.OpAppend, Rows: rows(1)})
	if got, _ := s.ReadTable(t.Context(), model.TableRanked); len(got) != 1 || got[0]["lead_id"] != "L0" {
		t.Errorf("Ranked = %v", got)
	}
}

// A commit over 9MB is ErrTooLarge before anything is sent; one Sheets
// itself refuses as too large is ErrTooLarge too.
func TestTooLarge(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	big := strings.Repeat("x", 40_000)
	var rows []api.Row
	for i := range 265 { // about 10.6MB: over both 9MB and Sheets' 10MB
		rows = append(rows, api.Row{"at": fmt.Sprint(i), "message": big})
	}
	before := f.Sheets.Calls("batchUpdate")
	err := s.Commit(t.Context(), []api.TableWrite{{Table: model.TableLog, Op: api.OpAppend, Rows: rows}})
	if !errors.Is(err, api.ErrTooLarge) {
		t.Fatalf("a 12MB commit = %v, want ErrTooLarge", err)
	}
	if f.Sheets.Calls("batchUpdate") != before {
		t.Error("a too-large commit was sent")
	}
	defer func(old int) { sheets.MaxCommitBytes = old }(sheets.MaxCommitBytes)
	sheets.MaxCommitBytes = 64 << 20
	if err := s.Commit(t.Context(), []api.TableWrite{{Table: model.TableLog, Op: api.OpAppend, Rows: rows}}); !errors.Is(err, api.ErrTooLarge) {
		t.Fatalf("a commit Sheets refuses as too large = %v, want ErrTooLarge", err)
	}
	if got, _ := s.ReadTable(t.Context(), model.TableLog); len(got) != 0 {
		t.Errorf("Log after refused commits has %d rows", len(got))
	}
}

// AppendEvents keeps retrying Google's "slow down" until it is stored, and
// gives up only when its context ends.
func TestAppendEventsRetriesSlowDown(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	ev := api.RawEvent{Kind: "apollo_visit", ReceivedAt: time.Now().UTC(), Body: []byte(`{"a":1}`)}
	f.Sheets.SlowDown(3)
	if err := s.AppendEvents(t.Context(), []api.RawEvent{ev}); err != nil {
		t.Fatalf("AppendEvents after three slow-downs: %v", err)
	}
	got, _, err := s.ReadEvents(t.Context(), "")
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadEvents = %d events, %v", len(got), err)
	}
	f.Sheets.SlowDown(1 << 30)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := s.AppendEvents(ctx, []api.RawEvent{ev}); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AppendEvents under endless slow-downs = %v, want the context's deadline", err)
	}
	f.Sheets.SlowDown(0)
}

// Events land in the tab of their received month. A new month's tab is
// created by the first append, protected with the editors of the existing
// Events tabs (both accounts), and receivers racing to create it all succeed.
func TestMonthlyEventTabs(t *testing.T) {
	f := newFakeGoogle(t)
	f.GCS.CreateBucket("lease")
	oct := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	id, err := sheets.Create(t.Context(), mustConnect(t, f), sheets.Template{Title: "leadscore",
		Accounts: sheets.Accounts{Run: "run@p.iam.gserviceaccount.com", Receiver: "recv@p.iam.gserviceaccount.com"}, Now: oct})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sheets.Open(t.Context(), f.cfg(id))
	if err != nil {
		t.Fatal(err)
	}
	nov := oct.Add(2 * time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.AppendEvents(t.Context(), []api.RawEvent{{Kind: "apollo_visit", ReceivedAt: nov, Body: []byte(fmt.Sprint(i))}})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a racing append: %v", err)
		}
	}
	book := f.Sheets.Spreadsheet(id)
	tab := tabNamed(t, book, "Events 2026-11")
	if len(tab.ProtectedRanges) != 1 {
		t.Fatalf("Events 2026-11 has %d protected ranges, want 1", len(tab.ProtectedRanges))
	}
	if users := tab.ProtectedRanges[0].Editors.Users; !slices.Equal(users, []string{"run@p.iam.gserviceaccount.com", "recv@p.iam.gserviceaccount.com"}) {
		t.Errorf("Events 2026-11 editors = %v", users)
	}
	if gp := tab.Properties.GridProperties; gp.ColumnCount != 4 || gp.FrozenRowCount != 1 {
		t.Errorf("Events 2026-11 grid = %+v", gp)
	}
	got, cursor, err := s.ReadEvents(t.Context(), "")
	if err != nil || len(got) != 8 {
		t.Fatalf("ReadEvents = %d, %v; want the 8 racing events once each", len(got), err)
	}
	// During the grace hour a late receiver appends to October's tab: the
	// saved cursor (past every November event) still returns it.
	late := api.RawEvent{Kind: "apollo_reply", ReceivedAt: oct, Body: []byte("late")}
	if err := s.AppendEvents(t.Context(), []api.RawEvent{late}); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.ReadEvents(t.Context(), cursor)
	if err != nil || len(got) != 1 || string(got[0].Body) != "late" {
		t.Fatalf("read after a grace-hour append = %v, %v", got, err)
	}
}

func mustConnect(t *testing.T, f *fakeGoogle) *sheets.Services {
	t.Helper()
	svc, err := sheets.Connect(t.Context(), f.cfg(""))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// A tool tab the run creates later (an export lane's) is protected like the
// tool tabs setup made: for the run account only. People-owned tabs are not.
func TestNewToolTabCopiesProtection(t *testing.T) {
	f := newFakeGoogle(t)
	id, err := sheets.Create(t.Context(), mustConnect(t, f), sheets.Template{Title: "leadscore",
		Accounts: sheets.Accounts{Run: "run@p.iam.gserviceaccount.com", Receiver: "recv@p.iam.gserviceaccount.com"}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := sheets.Open(t.Context(), f.cfg(id))
	commit(t, s, api.TableWrite{Table: "Export warm", Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{{"lead_id": "L1"}}})
	tab := tabNamed(t, f.Sheets.Spreadsheet(id), "Export warm")
	if len(tab.ProtectedRanges) != 1 || !slices.Equal(tab.ProtectedRanges[0].Editors.Users, []string{"run@p.iam.gserviceaccount.com"}) {
		t.Errorf("Export warm protection = %+v", tab.ProtectedRanges)
	}
}

// The lease is a file in the bucket; LeaseInfo shows it; release deletes it.
func TestLeaseFile(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	l, err := s.Lease(t.Context(), "run-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, gen, ok := f.GCS.Object("lease", sheets.LeaseObject)
	if !ok || gen == 0 || !strings.Contains(string(data), `"owner":"run-1"`) || !strings.Contains(string(data), `"expires_at":"20`) {
		t.Errorf("lease file = %s (gen %d, %v)", data, gen, ok)
	}
	owner, exp, err := s.LeaseInfo(t.Context())
	if err != nil || owner != "run-1" || !exp.After(time.Now()) {
		t.Errorf("LeaseInfo = %q, %v, %v", owner, exp, err)
	}
	if err := l.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.GCS.Object("lease", sheets.LeaseObject); ok {
		t.Error("release left the lease file")
	}
	if owner, _, err := s.LeaseInfo(t.Context()); owner != "" || err != nil {
		t.Errorf("LeaseInfo with no lease = %q, %v", owner, err)
	}
	// Without a bucket configured, taking the lease says what to set.
	cfg := f.cfg(s.SpreadsheetID())
	delete(cfg, "lease_bucket")
	nb, _ := sheets.Open(t.Context(), cfg)
	if _, err := nb.Lease(t.Context(), "x", time.Minute); err == nil || !strings.Contains(err.Error(), "lease_bucket") {
		t.Errorf("Lease without a bucket = %v", err)
	}
}

// The spreadsheet id is required, and the backend registers as `sheets`.
func TestOpenNeedsSpreadsheet(t *testing.T) {
	if _, err := sheets.Open(t.Context(), api.Config{"base_url": "http://127.0.0.1:1"}); err == nil ||
		!strings.Contains(err.Error(), "setup sheet") {
		t.Errorf("Open without a spreadsheet = %v", err)
	}
	f := newFakeGoogle(t)
	factory, ok := api.BackendFactory("sheets")
	if !ok {
		t.Fatal("no sheets backend registered")
	}
	b, l, err := factory(f.cfg(f.Sheets.NewSpreadsheet("x")))
	if err != nil || b == nil || l == nil {
		t.Errorf("the sheets backend factory = %v, %v, %v", b, l, err)
	}
}

// A person sorts Overrides between the store's check and its batchUpdate,
// so the delete by position hits the wrong row (here, someone's
// `unsubscribed` status). The store notices after the write, puts that row
// back, and fails the commit; the next try deletes the intended row.
func TestOverridesDeleteSurvivesASort(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	id := s.SpreadsheetID()
	if err := f.Sheets.Put(id, model.TableOverrides, [][]any{
		{"person", "action", "value", "note"},
		{"a@x.example", "retry", "", "done"},
		{"b@x.example", "status", "unsubscribed", "asked by email"},
	}); err != nil {
		t.Fatal(err)
	}
	f.Sheets.OnBatchUpdate(func() {
		f.Sheets.OnBatchUpdate(nil)
		f.Sheets.SwapRows(id, model.TableOverrides, 1, 2)
	})
	del := api.TableWrite{Table: model.TableOverrides, Op: api.OpDelete, Key: []string{"person", "action", "value", "note"},
		Rows: []api.Row{{"person": "a@x.example", "action": "retry", "value": "", "note": "done"}}}
	err := s.Commit(t.Context(), []api.TableWrite{del})
	if !errors.Is(err, api.ErrCommittedWithProblems) || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("Commit = %v; want ErrCommittedWithProblems after putting the row back", err)
	}
	if got := overridesPeople(t, s); !slices.Equal(got, []string{"a@x.example", "b@x.example"}) {
		t.Fatalf("Overrides after the race = %v; the unsubscribed row must be back", got)
	}
	// The next try deletes the intended row only.
	commit(t, s, del)
	if got := overridesPeople(t, s); !slices.Equal(got, []string{"b@x.example"}) {
		t.Errorf("Overrides after the retry = %v", got)
	}
}

// A row typed at the instant of the write is deleted in place of the
// intended row: the store cannot know it, but the intended row is still
// there, so the commit fails and says to check the tab.
func TestOverridesDeleteInsertAtWrite(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	id := s.SpreadsheetID()
	f.Sheets.Put(id, model.TableOverrides, [][]any{{"person", "action", "value", "note"}, {"a@x.example", "retry", "", ""}, {"b@x.example", "retry", "", ""}})
	f.Sheets.OnBatchUpdate(func() {
		f.Sheets.OnBatchUpdate(nil)
		f.Sheets.InsertRow(id, model.TableOverrides, 1, []string{"c@x.example", "status", "blocked", ""})
	})
	err := s.Commit(t.Context(), []api.TableWrite{{Table: model.TableOverrides, Op: api.OpDelete,
		Key: []string{"person", "action"}, Rows: []api.Row{{"person": "a@x.example", "action": "retry"}}}})
	if !errors.Is(err, api.ErrCommittedWithProblems) || !strings.Contains(err.Error(), "check the tab") {
		t.Errorf("Commit = %v; want ErrCommittedWithProblems naming the tab to check", err)
	}
}

// Overrides edited between the commit's read and its pre-send check: nothing
// is written.
func TestOverridesChangedBeforeSend(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	id := s.SpreadsheetID()
	f.Sheets.Put(id, model.TableOverrides, [][]any{{"person", "action", "value", "note"}, {"a@x.example", "retry", "", ""}})
	reads := 0
	f.Sheets.OnValuesRead(func() {
		if reads++; reads == 2 { // the pre-send check
			f.Sheets.InsertRow(id, model.TableOverrides, 1, []string{"z@x.example", "status", "blocked", ""})
		}
	})
	defer f.Sheets.OnValuesRead(nil)
	before := f.Sheets.Calls("batchUpdate")
	err := s.Commit(t.Context(), []api.TableWrite{{Table: model.TableOverrides, Op: api.OpDelete,
		Key: []string{"person", "action"}, Rows: []api.Row{{"person": "a@x.example", "action": "retry"}}}})
	if err == nil || f.Sheets.Calls("batchUpdate") != before {
		t.Errorf("Commit = %v with %d batchUpdates; want a refusal with none", err, f.Sheets.Calls("batchUpdate")-before)
	}
	if got := overridesPeople(t, s); !slices.Equal(got, []string{"a@x.example", "z@x.example"}) {
		t.Errorf("Overrides = %v", got)
	}
}

func overridesPeople(t *testing.T, s *sheets.Store) []string {
	t.Helper()
	rows, err := s.ReadTable(t.Context(), model.TableOverrides)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r["person"])
	}
	slices.Sort(out)
	return out
}

// Only the cells an upsert changes are written, so a team's formula in an
// extra column of the row survives.
func TestUpsertWritesOnlyChangedCells(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	commit(t, s, api.TableWrite{Table: model.TablePushes, Op: api.OpUpsert, Key: []string{"lead_id", "lane_id", "step"},
		Rows: []api.Row{{"lead_id": "L1", "lane_id": "warm", "step": "contact", "state": "pending"}}})
	id := s.SpreadsheetID()
	sid := tabNamed(t, f.Sheets.Spreadsheet(id), model.TablePushes).Properties.SheetId
	formula := `=LEN(A2)`
	_, err := s.Services().Sheets.Spreadsheets.BatchUpdate(id, &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: []*sheetsapi.Request{
		{AppendDimension: &sheetsapi.AppendDimensionRequest{SheetId: sid, Dimension: "COLUMNS", Length: 1}},
		{UpdateCells: &sheetsapi.UpdateCellsRequest{Start: &sheetsapi.GridCoordinate{SheetId: sid, RowIndex: 1, ColumnIndex: 13},
			Rows:   []*sheetsapi.RowData{{Values: []*sheetsapi.CellData{{UserEnteredValue: &sheetsapi.ExtendedValue{FormulaValue: &formula}}}}},
			Fields: "userEnteredValue"}},
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	// A team formula in a named column the upsert names but does not change.
	destFormula := `=CONCAT("seq","")`
	_, err = s.Services().Sheets.Spreadsheets.BatchUpdate(id, &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: []*sheetsapi.Request{
		{UpdateCells: &sheetsapi.UpdateCellsRequest{Start: &sheetsapi.GridCoordinate{SheetId: sid, RowIndex: 1, ColumnIndex: 4},
			Rows:   []*sheetsapi.RowData{{Values: []*sheetsapi.CellData{{UserEnteredValue: &sheetsapi.ExtendedValue{FormulaValue: &destFormula}}}}},
			Fields: "userEnteredValue"}},
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	commit(t, s, api.TableWrite{Table: model.TablePushes, Op: api.OpUpsert, Key: []string{"lead_id", "lane_id", "step"},
		Rows: []api.Row{{"lead_id": "L1", "lane_id": "warm", "step": "contact", "dest": destFormula, "state": "done", "vendor_id": "v1"}}})
	if !f.Sheets.IsFormula(id, model.TablePushes, "N2") {
		t.Error("the team's formula in N2 was overwritten")
	}
	if !f.Sheets.IsFormula(id, model.TablePushes, "E2") {
		t.Error("the formula in E2 (dest, named but unchanged) was rewritten as text")
	}
	if got := f.Sheets.Cell(id, model.TablePushes, "F2"); got != "done" {
		t.Errorf("state = %q", got)
	}
}

// A key column a person deleted does not shift the other key columns' names:
// a duplicate append is still caught.
func TestDuplicateAppendWithAKeyColumnGone(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	commit(t, s, api.TableWrite{Table: model.TableAppliedRows, Op: api.OpAppend, Rows: []api.Row{{"row_id": "r1", "row_hash": "h"}}})
	id := s.SpreadsheetID()
	sid := tabNamed(t, f.Sheets.Spreadsheet(id), model.TableAppliedRows).Properties.SheetId
	_, err := s.Services().Sheets.Spreadsheets.BatchUpdate(id, &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: []*sheetsapi.Request{
		{DeleteDimension: &sheetsapi.DeleteDimensionRequest{Range: &sheetsapi.DimensionRange{SheetId: sid, Dimension: "COLUMNS", StartIndex: 0, EndIndex: 1}}},
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	err = s.Commit(t.Context(), []api.TableWrite{{Table: model.TableAppliedRows, Op: api.OpAppend, Rows: []api.Row{{"row_id": "r1"}}}})
	if err == nil {
		t.Error("appending a key the table holds must fail, with source_id's column gone")
	}
}

// A new column goes after the last used column, not into an unnamed column
// that holds a person's stray data.
func TestNewColumnAfterLastUsed(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	id := s.SpreadsheetID()
	f.Sheets.Put(id, "Custom", [][]any{{"a", "b"}, {"1", "2", "", "stray"}})
	commit(t, s, api.TableWrite{Table: "Custom", Op: api.OpAppend, Rows: []api.Row{{"a": "3", "new_col": "x"}}})
	if got := f.Sheets.Cell(id, "Custom", "E1"); got != "new_col" {
		t.Errorf("E1 = %q, want new_col", got)
	}
	if got := f.Sheets.Cell(id, "Custom", "C1") + f.Sheets.Cell(id, "Custom", "D2"); got != "stray" {
		t.Errorf("C1 and D2 = %q; the unnamed column must be left alone", got)
	}
	rows, _ := s.ReadTable(t.Context(), "Custom")
	if len(rows) != 2 || rows[1]["new_col"] != "x" {
		t.Errorf("Custom = %v", rows)
	}
}

// A value longer than a cell holds is refused before anything is sent,
// naming the tab's row and column.
func TestCellTooLong(t *testing.T) {
	f := newFakeGoogle(t)
	s := openStore(t, f)
	before := f.Sheets.Calls("batchUpdate")
	err := s.Commit(t.Context(), []api.TableWrite{{Table: model.TableLog, Op: api.OpAppend,
		Rows: []api.Row{{"at": "x", "message": strings.Repeat("é", 50_001)}}}})
	if err == nil || !strings.Contains(err.Error(), "Log") || !strings.Contains(err.Error(), "column message") {
		t.Errorf("Commit = %v", err)
	}
	if f.Sheets.Calls("batchUpdate") != before {
		t.Error("a too-long value was sent")
	}
}
