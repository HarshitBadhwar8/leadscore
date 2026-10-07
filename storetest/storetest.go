// Package storetest is the conformance suite a plug-in store runs against
// itself. The built-in SQLite and Sheets stores run it
// too.
//
// The signatures use internal/api's names, which are the same types as
// leadscore.Backend and leadscore.EventLog (the root aliases them), so this
// package never imports the root.
package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

type Table struct {
	Name         string   // the table's name; for a pattern, the prefix ("Events ", "Export ")
	Columns      []string // fixed columns in order
	Pattern      bool     // Events YYYY-MM and Export <lane id>
	DynamicAfter string   // Ranked: derived-name columns go after this column
}

// Schema is every tool table, in the store's table order.
var Schema = func() []Table {
	out := make([]Table, len(model.Tables))
	for i, d := range model.Tables {
		out[i] = Table{Name: d.Name, Columns: append([]string(nil), d.Columns...), Pattern: d.Pattern,
			DynamicAfter: d.DynamicAfter}
	}
	return out
}()

// Run checks a plug-in store: round-trip of every table, Commit all-or-nothing for
// every op (a failure injected as an OpUpsert with no Key placed last), batch
// append, ordering, a slow append interleaved with a read, crash between phases,
// added columns and unknown columns kept, many callers racing Lease with exactly
// one winner (also on an expired lease), release by a non-owner refused, OpTrim,
// DeleteProcessed dropping a partition, ErrEventsShrank (a cursor saved from
// one store read against a fresh store), and a commit that fails while applying
// (its last write an OpAppend of a key a keyed table already holds) leaving
// nothing applied.
//
// open returns a new, empty store each time it is called. Events are appended
// with ReceivedAt set as a receiver sets it, so a store that partitions events
// by month partitions them by ReceivedAt.
//
// Call it from an external test package (package sqlite_test, not package
// sqlite), so a store inside this module can be tested without an import cycle.
func Run(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	cases := []struct {
		name string
		f    func(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog))
	}{
		{"RoundTripEveryTable", roundTripEveryTable},
		{"RoundTripModel", roundTripModel},
		{"CommitAllOrNothing", commitAllOrNothing},
		{"CommitRollsBack", commitRollsBack},
		{"BatchAppend", batchAppend},
		{"Ordering", ordering},
		{"SlowAppendInterleavedWithRead", slowAppendInterleavedWithRead},
		{"CrashBetweenPhases", crashBetweenPhases},
		{"AddedColumns", addedColumns},
		{"UnknownColumnsKept", unknownColumnsKept},
		{"LeaseRace", leaseRace},
		{"ExpiredLeaseRace", expiredLeaseRace},
		{"OpTrim", opTrim},
		{"DeleteProcessedDropsPartition", deleteProcessedDropsPartition},
		{"EventsShrank", eventsShrank},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.f(t, open) })
	}
}

// Values a store must keep exactly as text: a formula, leading zeros, numbers,
// booleans and dates as text, spaces, quotes, newlines and non-ASCII.
var tricky = []string{
	"=1+1", "0123", "1e5", "TRUE", "2026-01-02", "  padded  ", `comma, "quoted"`, "line1\nline2",
	"ünïcødé ✓", "+44 20 7946 0000", "", "-0", "'apostrophe",
}

// tableName is the concrete name of a Schema table (a pattern gets a suffix).
func tableName(tb Table) string {
	if tb.Pattern {
		return tb.Name + "storetest"
	}
	return tb.Name
}

func sampleRows(tb Table, n int) []api.Row {
	def, _ := model.Def(tableName(tb))
	rows := make([]api.Row, n)
	for i := range rows {
		r := api.Row{}
		for j, c := range tb.Columns {
			if contains(def.Key, c) {
				r[c] = fmt.Sprintf("key-%d-%s", i, c)
			} else {
				r[c] = tricky[(i*len(tb.Columns)+j)%len(tricky)]
			}
		}
		if tb.DynamicAfter != "" {
			r["tier"] = fmt.Sprintf("T%d", i)
		}
		rows[i] = r
	}
	return rows
}

func roundTripEveryTable(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	for _, tb := range Schema {
		if tb.Name == model.EventsPrefix {
			continue // events go through the EventLog; see Ordering
		}
		name := tableName(tb)
		rows := sampleRows(tb, 3)
		if err := b.Commit(ctx, []api.TableWrite{{Table: name, Op: api.OpReplace, Rows: rows}}); err != nil {
			t.Fatalf("Commit %s: %v", name, err)
		}
		got, err := b.ReadTable(ctx, name)
		if err != nil {
			t.Fatalf("ReadTable %s: %v", name, err)
		}
		def, _ := model.Def(name)
		sameRows(t, name, got, rows, def.Key)
	}
	if rows, err := b.ReadTable(ctx, "Storetest missing"); err != nil || len(rows) != 0 {
		t.Errorf("a missing table must read as no rows and no error, got %v, %v", rows, err)
	}
}

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)

// fullModel puts one fully populated row in every table the model holds.
func fullModel(m *model.Model) {
	at := func(d int) time.Time { return t0.Add(time.Duration(d) * time.Hour) }
	extra := map[string]string{"x_future": "kept =1"}
	m.Put(model.TableOverrides, model.Override{Person: "a@x.example", Action: "status", Value: "do_not_contact", Note: "n", Extra: extra})
	m.Put(model.TableAppliedOverrides, model.AppliedOverride{RowHash: "h1", AppliedAt: at(1), RunID: "r1", Extra: extra})
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: at(1), ApolloHeldAt: at(2), MergedInto: "L0",
		FirstSeen: map[string]time.Time{"visit": at(3)},
		Fields: map[string]model.Field{
			"company.domain": {Value: "x.example", SourceID: "csv", At: at(4), Derived: true},
			"title":          {Value: "=CTO", SourceID: "csv", At: at(4)},
		},
		Conflicts: map[string][]model.Conflict{"title": {{Value: "CEO", SourceID: "sheet"}}},
		Extra:     extra})
	m.Put(model.TableIdentities, model.Identity{Key: "a@x.example", Kind: "email", LeadID: "L1", SourceID: "csv", FirstSeenAt: at(1), Extra: extra})
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "x.example",
		Facts:     map[string]model.Fact{"employees": {Value: "0120", Origin: "enrichment", At: at(5)}},
		Previous:  map[string]model.Fact{"employees": {Value: "90", Origin: "input", At: at(2)}},
		Rollups:   map[string]any{"leads_seen": float64(3), "names": []any{"a", "b"}},
		FirstSeen: map[string]time.Time{"visit": at(3)}, EnrichedAt: at(5), NotFoundAt: at(6), Extra: extra})
	m.Put(model.TableWindowEvents, model.WindowEvent{EventKey: "e1", Subject: "lead", LeadID: "L1", Domain: "x.example",
		Kind: "visit_pricing", At: at(3), Attrs: map[string]string{"page": "/pricing"}, Extra: extra})
	m.Put(model.TableAppliedRows, model.AppliedRow{SourceID: "csv", RowID: "a@x.example", RowHash: "rh", LeadID: "L1", FirstAppliedAt: at(1), Extra: extra})
	m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: "e1", FirstReceivedAt: at(3), RunID: "r1", Extra: extra})
	m.Put(model.TableOutcomes, model.Outcome{LeadID: "L1", Status: "replied_positive", StatusAt: at(7), UnsubscribedAt: at(8),
		UnsubscribedOrigin: "event", ReplyStatus: "positive", ReplyAt: at(7), ContactedAt: at(6), DealID: "D1",
		DealStage: "open", DealCheckedAt: at(9), Extra: extra})
	m.Put(model.TableRanked, model.RankedRow{LeadID: "L1", Email: "a@x.example", LinkedInURL: "https://linkedin.com/in/a",
		FullName: "Ann", CompanyDomain: "x.example", Derived: map[string]string{"tier": "A", "priority": "0.5"},
		AccountScore: 1.25, ContactScore: -2, Score: 0.1, Status: "new", Lane: "warm", Reasons: "r1; r2", RubricVersion: "3"})
	m.Put(model.TablePushes, model.Push{LeadID: "L1", LaneID: "warm", Step: "contact", LaneKind: "cold", Dest: "sequence/q",
		State: "done", VendorID: "v1", Attempts: 2, CalledAt: at(6), IntentRun: "r1", LastError: "boom",
		FirstStartedAt: at(5), UpdatedAt: at(6), Extra: extra})
	m.Put(model.TableLog, model.LogEntry{At: at(6), RunID: "r1", Level: "info", LeadID: "L1", Email: "a@x.example",
		Kind: "tier_change", Message: "B -> A", RubricVersion: "3", Extra: extra})
	m.Put(model.TableHealth, model.HealthRow{Kind: "problem", Key: "silent:visit", Value: "3 days", FirstSeenAt: at(1), UpdatedAt: at(2), Extra: extra})
	m.Put(model.TableState, model.StateRow{Key: "cursor:csv", Value: "0042", Extra: extra})
	m.Put(model.ExportTable("warm"), model.ExportRow{LeadID: "L1", Email: "a@x.example", LinkedInURL: "li", FullName: "Ann",
		CompanyDomain: "x.example", Score: 7.5, Reasons: "r", FirstListedAt: at(1), Status: "new", DoNotContact: true,
		UpdatedAt: at(2), Extra: extra})
}

// commitAll encodes the named tables (all when none), commits and clears them.
func commitAll(t *testing.T, b api.Backend, m *model.Model, tables ...string) {
	t.Helper()
	w := codec.Encode(m, tables...)
	if err := b.Commit(t.Context(), w); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	m.Committed(w)
}

func load(t *testing.T, b api.Backend) *model.Model {
	t.Helper()
	m, err := codec.Load(t.Context(), b)
	if err != nil {
		t.Fatalf("codec.Load: %v", err)
	}
	return m
}

func roundTripModel(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	m := load(t, b)
	fullModel(m)
	commitAll(t, b, m)
	if w := codec.Encode(m); len(w) != 0 {
		t.Errorf("after Committed, Encode must return nothing; got %d writes", len(w))
	}
	// The export table is found through State's export_lane:warm.
	got := load(t, b)
	mv, gv := reflect.ValueOf(m).Elem(), reflect.ValueOf(got).Elem()
	for i := range mv.NumField() {
		f := mv.Type().Field(i)
		if !f.IsExported() || f.Name == "Log" { // runs do not load Log; checked below
			continue
		}
		want, have := mv.Field(i).Interface(), gv.Field(i).Interface()
		if !reflect.DeepEqual(have, want) {
			t.Errorf("%s did not round-trip:\n got %+v\nwant %+v", f.Name, have, want)
		}
	}
	logRows, err := b.ReadTable(t.Context(), model.TableLog)
	if err != nil || len(logRows) != 1 || logRows[0]["kind"] != "tier_change" || logRows[0]["at"] != model.FormatTime(t0.Add(6*time.Hour)) {
		t.Errorf("Log = %v, %v", logRows, err)
	}
	if w := codec.Encode(got); len(w) != 0 {
		t.Errorf("a freshly loaded store must encode nothing; got %+v", w)
	}
}

func commitAllOrNothing(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	seed := []api.Row{
		{"lead_id": "L1", "created_at": "2026-01-01T00:00:00.000Z", "merged_into": ""},
		{"lead_id": "L2", "created_at": "2026-03-01T00:00:00.000Z", "merged_into": ""},
	}
	ops := []struct {
		name string
		w    api.TableWrite
	}{
		{"OpReplace", api.TableWrite{Table: model.TablePeople, Op: api.OpReplace, Rows: []api.Row{{"lead_id": "L9"}}}},
		{"OpAppend", api.TableWrite{Table: model.TablePeople, Op: api.OpAppend, Rows: []api.Row{{"lead_id": "L9"}}}},
		{"OpUpsert", api.TableWrite{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"},
			Rows: []api.Row{{"lead_id": "L1", "merged_into": "L2"}, {"lead_id": "L9"}}}},
		{"OpDelete", api.TableWrite{Table: model.TablePeople, Op: api.OpDelete, Key: []string{"lead_id"},
			Rows: []api.Row{{"lead_id": "L1"}}}},
		{"OpTrim", api.TableWrite{Table: model.TablePeople, Op: api.OpTrim, Column: "created_at",
			Before: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			b, _ := open(t)
			ctx := t.Context()
			if err := b.Commit(ctx, []api.TableWrite{{Table: model.TablePeople, Op: api.OpReplace, Rows: seed}}); err != nil {
				t.Fatal(err)
			}
			err := b.Commit(ctx, []api.TableWrite{
				op.w,
				{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "e1"}}},
				{Table: model.TableHealth, Op: api.OpUpsert, Rows: []api.Row{{"kind": "result", "key": "k"}}}, // no Key
			})
			if err == nil {
				t.Fatal("a Commit with an OpUpsert that has no Key must fail")
			}
			got, err := b.ReadTable(ctx, model.TablePeople)
			if err != nil {
				t.Fatal(err)
			}
			sameRows(t, "People after a failed Commit", got, seed, []string{"lead_id"})
			for _, name := range []string{model.TableSeenEvents, model.TableHealth} {
				if rows, err := b.ReadTable(ctx, name); err != nil || len(rows) != 0 {
					t.Errorf("%s after a failed Commit = %v, %v; want nothing written", name, rows, err)
				}
			}
			// The same write alone succeeds, so the failure above was the injected one.
			if err := b.Commit(ctx, []api.TableWrite{op.w}); err != nil {
				t.Errorf("%s alone: %v", op.name, err)
			}
		})
	}
	t.Run("OpDeleteWithoutKey", func(t *testing.T) {
		b, _ := open(t)
		err := b.Commit(t.Context(), []api.TableWrite{{Table: model.TablePeople, Op: api.OpDelete, Rows: []api.Row{{"lead_id": "L1"}}}})
		if err == nil {
			t.Error("an OpDelete with no Key must be rejected")
		}
	})
}

// commitRollsBack fails a commit in its last write, at apply time rather than
// in validation: an OpAppend to a keyed table of a key the store already
// holds, or one the same commit already wrote (see OpAppend).
// Nothing from that commit may remain.
func commitRollsBack(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	seed := []api.Row{{"event_key": "e1", "first_received_at": "2026-01-01T00:00:00.000Z"}}
	if err := b.Commit(ctx, []api.TableWrite{{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: seed}}); err != nil {
		t.Fatal(err)
	}
	err := b.Commit(ctx, []api.TableWrite{
		{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{{"lead_id": "L1"}}},
		{Table: model.TableState, Op: api.OpReplace, Rows: []api.Row{{"key": "k", "value": "v"}}},
		{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "e2"}, {"event_key": "e1"}}},
	})
	if err == nil {
		t.Fatal("appending a key the store already holds must fail the commit")
	}
	for _, name := range []string{model.TablePeople, model.TableState} {
		if rows, err := b.ReadTable(ctx, name); err != nil || len(rows) != 0 {
			t.Errorf("%s after a rolled-back commit = %v, %v; want nothing", name, rows, err)
		}
	}
	got, err := b.ReadTable(ctx, model.TableSeenEvents)
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, "Seen events after a rolled-back commit", got, seed, []string{"event_key"})

	// A key written twice in one commit fails it the same way.
	err = b.Commit(ctx, []api.TableWrite{
		{Table: model.TableState, Op: api.OpReplace, Rows: []api.Row{{"key": "k", "value": "v"}}},
		{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "e3"}}},
		{Table: model.TableSeenEvents, Op: api.OpAppend, Rows: []api.Row{{"event_key": "e3"}}},
	})
	if err == nil {
		t.Fatal("appending one key twice in a commit must fail it")
	}
	if rows, err := b.ReadTable(ctx, model.TableState); err != nil || len(rows) != 0 {
		t.Errorf("State after a rolled-back commit = %v, %v; want nothing", rows, err)
	}
	got, _ = b.ReadTable(ctx, model.TableSeenEvents)
	sameRows(t, "Seen events after a rolled-back commit", got, seed, []string{"event_key"})
}

func event(i int, at time.Time) api.RawEvent {
	kind := "apollo_visit"
	if i%2 == 1 {
		kind = "apollo_reply"
	}
	return api.RawEvent{Kind: kind, ReceivedAt: at, Body: []byte(fmt.Sprintf(`{"n":%d,"text":"ünï, \"q\""}`, i))}
}

// now is the suite's "received now" time, to the millisecond (the stored time
// form).
func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func readAll(t *testing.T, l api.EventLog, cursor api.Cursor) ([]api.RawEvent, api.Cursor) {
	t.Helper()
	got, next, err := l.ReadEvents(t.Context(), cursor)
	if err != nil {
		t.Fatalf("ReadEvents(%q): %v", cursor, err)
	}
	return got, next
}

func sameEvents(t *testing.T, what string, got, want []api.RawEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d events, want %d", what, len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Kind != w.Kind || string(g.Body) != string(w.Body) || !g.ReceivedAt.Equal(w.ReceivedAt) {
			t.Fatalf("%s: event %d = {%s %s %s}, want {%s %s %s}", what, i, g.Kind, g.ReceivedAt, g.Body, w.Kind, w.ReceivedAt, w.Body)
		}
		if g.Seq == "" {
			t.Fatalf("%s: event %d has no Seq", what, i)
		}
	}
}

func batchAppend(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	_, l := open(t)
	if err := l.AppendEvents(t.Context(), nil); err != nil {
		t.Errorf("an empty batch: %v", err)
	}
	at := now()
	var batch []api.RawEvent
	for i := range 50 {
		batch = append(batch, event(i, at))
	}
	if err := l.AppendEvents(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, l, "")
	sameEvents(t, "one batch of 50", got, batch)
	seen := map[api.Cursor]bool{}
	for _, e := range got {
		if seen[e.Seq] {
			t.Fatalf("Seq %q used twice", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func ordering(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	_, l := open(t)
	at := now()
	var all []api.RawEvent
	for _, n := range []int{2, 1, 3} {
		var batch []api.RawEvent
		for range n {
			batch = append(batch, event(len(all)+len(batch), at))
		}
		if err := l.AppendEvents(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		all = append(all, batch...)
	}
	got, next := readAll(t, l, "")
	sameEvents(t, "read from the start", got, all)
	// Each Seq is a complete resume cursor.
	for i, e := range got {
		after, _ := readAll(t, l, e.Seq)
		sameEvents(t, fmt.Sprintf("read after event %d", i), after, all[i+1:])
	}
	if rest, again := readAll(t, l, next); len(rest) != 0 || again == "" {
		t.Errorf("reading from the last cursor = %d events, cursor %q; want none and a cursor", len(rest), again)
	}
	more := []api.RawEvent{event(100, at)}
	if err := l.AppendEvents(t.Context(), more); err != nil {
		t.Fatal(err)
	}
	got, _ = readAll(t, l, next)
	sameEvents(t, "read from the saved cursor after a new append", got, more)
}

// slowAppendInterleavedWithRead races several large appends against a reader
// that resumes from its saved cursor: every event must be returned exactly
// once, and each writer's events in its order. A store that numbers events at
// insert rather than at commit would let the reader skip one.
func slowAppendInterleavedWithRead(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	_, l := open(t)
	const writers, batches, perBatch = 4, 5, 10
	pad := strings.Repeat("x", 2000)
	at := now()
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range batches {
				var batch []api.RawEvent
				for i := range perBatch {
					batch = append(batch, api.RawEvent{Kind: "apollo_visit", ReceivedAt: at,
						Body: []byte(fmt.Sprintf(`{"w":%d,"n":%d,"pad":%q}`, w, b*perBatch+i, pad))})
				}
				if err := l.AppendEvents(t.Context(), batch); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var cursor api.Cursor
	var got []api.RawEvent
	read := func() {
		evs, next, err := l.ReadEvents(t.Context(), cursor)
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got, cursor = append(got, evs...), next
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			read()
			time.Sleep(time.Millisecond)
		}
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	read()

	seen := map[string]bool{}
	last := map[int]int{}
	for _, e := range got {
		if seen[string(e.Body)] {
			t.Fatalf("event returned twice: %.30s", e.Body)
		}
		seen[string(e.Body)] = true
		var w, n int
		fmt.Sscanf(string(e.Body), `{"w":%d,"n":%d`, &w, &n)
		if prev, ok := last[w]; ok && n <= prev {
			t.Fatalf("writer %d: event %d returned after %d", w, n, prev)
		}
		last[w] = n
	}
	if len(got) != writers*batches*perBatch {
		t.Fatalf("the reader saw %d events, want %d: an event was skipped", len(got), writers*batches*perBatch)
	}
}

// crashBetweenPhases commits phase 1, fails and then abandons phase 2, and
// starts over from a fresh load: phase 1 is all there, phase 2 none of it, and
// redoing phase 2 completes the store.
func crashBetweenPhases(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	phase1 := []string{model.TablePeople, model.TableIdentities, model.TableAppliedRows, model.TableSeenEvents, "State:cursor:"}
	phase2 := []string{model.TableCompanyFacts, model.TableHealth, model.TableLog, model.TableState}

	m := load(t, b)
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: t0})
	m.Put(model.TableIdentities, model.Identity{Key: "a@x.example", Kind: "email", LeadID: "L1", SourceID: "csv", FirstSeenAt: t0})
	m.Put(model.TableAppliedRows, model.AppliedRow{SourceID: "csv", RowID: "a@x.example", RowHash: "h", LeadID: "L1", FirstAppliedAt: t0})
	m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: "e1", FirstReceivedAt: t0, RunID: "r1"})
	m.SetState("cursor:csv", "7")
	m.SetState("last_run_at", model.FormatTime(t0))
	commitAll(t, b, m, phase1...)

	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "x.example", EnrichedAt: t0})
	m.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "last_result", Value: "healthy", UpdatedAt: t0})
	m.Put(model.TableLog, model.LogEntry{At: t0, RunID: "r1", Kind: "tier_change"})
	w := append(codec.Encode(m, phase2...), api.TableWrite{Table: model.TablePushes, Op: api.OpUpsert, Rows: []api.Row{{"lead_id": "L1"}}})
	if err := b.Commit(ctx, w); err == nil {
		t.Fatal("the injected phase 2 failure did not fail")
	}
	// Crash: the model is lost. The next run loads afresh.
	m2 := load(t, b)
	if _, ok := m2.People["L1"]; !ok {
		t.Error("phase 1 People row missing after the crash")
	}
	if _, ok := m2.Identities["a@x.example"]; !ok {
		t.Error("phase 1 Identities row missing after the crash")
	}
	if _, ok := m2.AppliedRows[model.K("csv", "a@x.example")]; !ok {
		t.Error("phase 1 Applied rows row missing after the crash")
	}
	if m2.StateValue("cursor:csv") != "7" {
		t.Errorf("phase 1 cursor = %q, want 7", m2.StateValue("cursor:csv"))
	}
	if v := m2.StateValue("last_run_at"); v != "" {
		t.Errorf("State.last_run_at = %q: a phase 2 key was written in phase 1", v)
	}
	if len(m2.CompanyFacts) != 0 || len(m2.Health) != 0 {
		t.Errorf("phase 2 rows present after its Commit failed: %v %v", m2.CompanyFacts, m2.Health)
	}
	if rows, _ := b.ReadTable(ctx, model.TableLog); len(rows) != 0 {
		t.Errorf("Log rows present after phase 2 failed: %v", rows)
	}
	// Redo phase 2 from the fresh load; phase 1 is not rewritten.
	m2.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "x.example", EnrichedAt: t0})
	m2.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "last_result", Value: "healthy", UpdatedAt: t0})
	m2.SetState("last_run_at", model.FormatTime(t0))
	m2.Put(model.TablePeople, m2.People["L1"]) // unchanged: no write
	for _, w := range codec.Encode(m2) {
		if w.Table == model.TablePeople {
			t.Errorf("an unchanged People row was encoded: %+v", w)
		}
	}
	commitAll(t, b, m2)
	m3 := load(t, b)
	if len(m3.CompanyFacts) != 1 || len(m3.Health) != 1 || m3.StateValue("last_run_at") == "" || len(m3.People) != 1 {
		t.Errorf("after redoing phase 2: facts %d, health %d, last_run_at %q, people %d",
			len(m3.CompanyFacts), len(m3.Health), m3.StateValue("last_run_at"), len(m3.People))
	}
}

func addedColumns(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	people := []api.Row{{"lead_id": "L1", "created_at": "2026-01-01T00:00:00.000Z"}}
	if err := b.Commit(ctx, []api.TableWrite{{Table: model.TablePeople, Op: api.OpAppend, Rows: people}}); err != nil {
		t.Fatal(err)
	}
	// A later write names a column the table lacks: the column is appended.
	err := b.Commit(ctx, []api.TableWrite{
		{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"},
			Rows: []api.Row{{"lead_id": "L2", "created_at": "2026-01-02T00:00:00.000Z", "added_col": "v2"}}},
		// A write naming a missing table creates it.
		{Table: "Storetest new", Op: api.OpAppend, Rows: []api.Row{{"a": "1", "b": "=2"}}},
		// A trim on a missing table, or on a column the table lacks, is a no-op.
		{Table: model.TableLog, Op: api.OpTrim, Column: "at", Before: t0},
		{Table: model.TablePeople, Op: api.OpTrim, Column: "gone_at", Before: t0},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadTable(ctx, model.TablePeople)
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, "People with an added column", got, []api.Row{
		{"lead_id": "L1", "created_at": "2026-01-01T00:00:00.000Z", "added_col": ""},
		{"lead_id": "L2", "created_at": "2026-01-02T00:00:00.000Z", "added_col": "v2"},
	}, []string{"lead_id"})
	got, err = b.ReadTable(ctx, "Storetest new")
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, "a table created by its first write", got, []api.Row{{"a": "1", "b": "=2"}}, nil)
}

func unknownColumnsKept(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	// A newer version wrote a column this one does not know, and a newer minor.
	err := b.Commit(ctx, []api.TableWrite{
		{Table: model.TablePeople, Op: api.OpReplace, Rows: []api.Row{
			{"lead_id": "L1", "created_at": "2026-01-01T00:00:00.000Z", "future_col": "keep me"}}},
		{Table: model.TableState, Op: api.OpReplace, Rows: []api.Row{
			{"key": "schema_version", "value": "1.7"}, {"key": "cursor:csv", "value": "3", "future_col": "s"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An upsert naming only some columns leaves the others alone.
	err = b.Commit(ctx, []api.TableWrite{{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"},
		Rows: []api.Row{{"lead_id": "L1", "merged_into": "L0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	m := load(t, b)
	p := m.People["L1"]
	if p.Extra["future_col"] != "keep me" || p.MergedInto != "L0" {
		t.Fatalf("loaded person = %+v", p)
	}
	p.CreatedAt = t0
	m.Put(model.TablePeople, p)
	m.SetState("cursor:csv", "4")
	commitAll(t, b, m)
	rows, err := b.ReadTable(ctx, model.TablePeople)
	if err != nil || len(rows) != 1 || rows[0]["future_col"] != "keep me" || rows[0]["created_at"] != model.FormatTime(t0) {
		t.Errorf("People after a rewrite = %v, %v; want future_col kept", rows, err)
	}
	m = load(t, b)
	if v := m.StateValue("schema_version"); v != "1.7" {
		t.Errorf("schema_version = %q: a newer minor must never be lowered", v)
	}
	if s := m.State["cursor:csv"]; s.Value != "4" || s.Extra["future_col"] != "s" {
		t.Errorf("State cursor row = %+v", s)
	}
	// A newer major version is refused.
	err = b.Commit(ctx, []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "schema_version", "value": "2.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Load(ctx, b); !errors.Is(err, codec.ErrNewerSchema) {
		t.Errorf("Load of a 2.0 store = %v, want ErrNewerSchema", err)
	}
	// A fresh store gets this binary's version on its first save.
	fresh, _ := open(t)
	m = load(t, fresh)
	commitAll(t, fresh, m)
	if v := load(t, fresh).StateValue("schema_version"); v != model.SchemaVersion {
		t.Errorf("fresh store schema_version = %q, want %s", v, model.SchemaVersion)
	}
}

// race has n callers take the lease at once and returns the winners.
func race(t *testing.T, b api.Backend, n int, prefix string) map[string]api.RunLease {
	t.Helper()
	var mu sync.Mutex
	won := map[string]api.RunLease{}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("%s-%d", prefix, i)
			l, err := b.Lease(t.Context(), owner, time.Minute)
			switch {
			case err == nil:
				mu.Lock()
				won[owner] = l
				mu.Unlock()
			case !errors.Is(err, api.ErrLeaseHeld):
				t.Errorf("Lease(%s): %v, want nil or ErrLeaseHeld", owner, err)
			}
		}()
	}
	wg.Wait()
	return won
}

func onlyWinner(t *testing.T, won map[string]api.RunLease) (string, api.RunLease) {
	t.Helper()
	if len(won) != 1 {
		t.Fatalf("%d callers hold the lease, want exactly 1", len(won))
	}
	for o, l := range won {
		return o, l
	}
	return "", nil
}

func leaseRace(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	owner, l := onlyWinner(t, race(t, b, 16, "run"))
	if err := l.Check(ctx); err != nil {
		t.Errorf("winner Check: %v", err)
	}
	if li, ok := b.(api.LeaseInspector); ok {
		if o, exp, err := li.LeaseInfo(ctx); err != nil || o != owner || !exp.After(time.Now()) {
			t.Errorf("LeaseInfo = %q, %v, %v; want %q and a future expiry", o, exp, err, owner)
		}
	}
	if _, err := b.Lease(ctx, "late", time.Minute); !errors.Is(err, api.ErrLeaseHeld) {
		t.Errorf("Lease while held = %v, want ErrLeaseHeld", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("owner Release: %v", err)
	}
	next, err := b.Lease(ctx, "next", time.Minute)
	if err != nil {
		t.Fatalf("Lease after release: %v", err)
	}
	if err := next.Release(ctx); err != nil {
		t.Error(err)
	}
}

func expiredLeaseRace(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	stale, err := b.Lease(ctx, "stale", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if err := stale.Check(ctx); !errors.Is(err, api.ErrLeaseLost) {
		t.Errorf("Check on an expired lease = %v, want ErrLeaseLost", err)
	}
	_, winner := onlyWinner(t, race(t, b, 16, "takeover"))
	if err := stale.Check(ctx); !errors.Is(err, api.ErrLeaseLost) {
		t.Errorf("Check after a takeover = %v, want ErrLeaseLost", err)
	}
	// Release by a non-owner is refused and leaves the winner holding it.
	if err := stale.Release(ctx); err == nil {
		t.Error("Release by the old owner after a takeover must fail")
	}
	if err := winner.Check(ctx); err != nil {
		t.Errorf("the winner lost the lease to a non-owner's Release: %v", err)
	}
	if _, err := b.Lease(ctx, "late", time.Minute); !errors.Is(err, api.ErrLeaseHeld) {
		t.Errorf("Lease after a refused release = %v, want ErrLeaseHeld", err)
	}
}

func opTrim(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	b, _ := open(t)
	ctx := t.Context()
	cut := t0
	ts := func(d time.Duration) string { return model.FormatTime(cut.Add(d)) }
	window := []api.Row{
		{"event_key": "old", "at": ts(-72 * time.Hour)},
		{"event_key": "just-old", "at": ts(-time.Millisecond)},
		{"event_key": "boundary", "at": ts(0)},
		{"event_key": "new", "at": ts(24 * time.Hour)},
	}
	logRows := []api.Row{{"at": ts(-time.Hour), "kind": "a"}, {"at": ts(time.Hour), "kind": "b"}}
	err := b.Commit(ctx, []api.TableWrite{
		{Table: model.TableWindowEvents, Op: api.OpAppend, Rows: window},
		{Table: model.TableLog, Op: api.OpAppend, Rows: logRows},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = b.Commit(ctx, []api.TableWrite{
		{Table: model.TableWindowEvents, Op: api.OpTrim, Column: "at", Before: cut},
		{Table: model.TableLog, Op: api.OpTrim, Column: "at", Before: cut},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := b.ReadTable(ctx, model.TableWindowEvents)
	sameRows(t, "Window events after a trim", got, window[2:], []string{"event_key"})
	got, _ = b.ReadTable(ctx, model.TableLog)
	sameRows(t, "Log after a trim", got, logRows[1:], nil)
}

func deleteProcessedDropsPartition(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	_, l := open(t)
	ctx := t.Context()
	recent := now()
	old := recent.AddDate(0, 0, -70) // at least two months back: its own partition
	olds := []api.RawEvent{event(1, old), event(2, old)}
	new1 := event(3, recent)
	if err := l.AppendEvents(ctx, olds); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendEvents(ctx, []api.RawEvent{new1}); err != nil {
		t.Fatal(err)
	}
	_, committed := readAll(t, l, "")
	new2 := event(4, recent) // above the committed cursor: kept whatever its age
	if err := l.AppendEvents(ctx, []api.RawEvent{new2}); err != nil {
		t.Fatal(err)
	}
	kept, err := l.DeleteProcessed(ctx, committed, recent.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("DeleteProcessed: %v", err)
	}
	got, _ := readAll(t, l, "")
	sameEvents(t, "events left after DeleteProcessed", got, []api.RawEvent{new1, new2})
	got, next := readAll(t, l, kept)
	sameEvents(t, "read from the returned cursor", got, []api.RawEvent{new2})
	// Numbers are never reused: a new event is read from the saved cursor.
	new3 := event(5, recent)
	if err := l.AppendEvents(ctx, []api.RawEvent{new3}); err != nil {
		t.Fatal(err)
	}
	got, _ = readAll(t, l, next)
	sameEvents(t, "read after a new append", got, []api.RawEvent{new3})
	got, _ = readAll(t, l, kept)
	sameEvents(t, "read again from the returned cursor", got, []api.RawEvent{new2, new3})
}

func eventsShrank(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	_, l := open(t)
	ctx := t.Context()
	at := now()
	if err := l.AppendEvents(ctx, []api.RawEvent{event(1, at), event(2, at), event(3, at)}); err != nil {
		t.Fatal(err)
	}
	_, saved := readAll(t, l, "")
	_, fresh := open(t)
	if _, _, err := fresh.ReadEvents(ctx, saved); !errors.Is(err, api.ErrEventsShrank) {
		t.Errorf("a saved cursor against a fresh store = %v, want ErrEventsShrank", err)
	}
	if err := fresh.AppendEvents(ctx, []api.RawEvent{event(1, at)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fresh.ReadEvents(ctx, saved); !errors.Is(err, api.ErrEventsShrank) {
		t.Errorf("a saved cursor against a store holding fewer events = %v, want ErrEventsShrank", err)
	}
}

// sameRows compares rows, a missing column counting as empty. With key columns
// the order does not matter; without, rows must come back in written order.
func sameRows(t *testing.T, what string, got, want []api.Row, key []string) {
	t.Helper()
	norm := func(rows []api.Row) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			var cols []string
			for c, v := range r {
				if v != "" {
					cols = append(cols, fmt.Sprintf("%q=%q", c, v))
				}
			}
			sort.Strings(cols)
			k := ""
			for _, c := range key {
				k += r[c] + "\x1f"
			}
			out[i] = k + "\x00" + strings.Join(cols, " ")
		}
		if len(key) > 0 {
			sort.Strings(out)
		}
		return out
	}
	g, w := norm(got), norm(want)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got %q\nwant %q", what, g, w)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
