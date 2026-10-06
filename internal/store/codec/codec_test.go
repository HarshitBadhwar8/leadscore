package codec_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

var t0 = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

// memStore is a Backend holding tables in memory, enough to load from.
type memStore map[string][]api.Row

func (s memStore) ReadTable(_ context.Context, name string) ([]api.Row, error) { return s[name], nil }
func (memStore) Lease(context.Context, string, time.Duration) (api.RunLease, error) {
	return nil, nil
}
func (memStore) Commit(context.Context, []api.TableWrite) error { return nil }

// loadFrom loads s; a store with no State gets the current schema version, so
// the load records no version change.
func loadFrom(t *testing.T, s memStore, lanes ...string) *model.Model {
	t.Helper()
	if s["State"] == nil {
		s["State"] = []api.Row{{"key": "schema_version", "value": model.SchemaVersion}}
	}
	m, err := codec.Load(context.Background(), s, lanes...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// summary is "Table:Op:rows" per write, for compact assertions.
func summary(ws []api.TableWrite) []string {
	var out []string
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s:%d:%d", w.Table, w.Op, len(w.Rows)))
	}
	return out
}

func TestEncodeChoosesTheOp(t *testing.T) {
	s := memStore{
		"State":       {{"key": "schema_version", "value": "1.0"}},
		"Identities":  {{"key": "old@x.example", "kind": "email", "lead_id": "L1"}},
		"Seen events": {{"event_key": "e0"}},
		"People":      {{"lead_id": "L1"}, {"lead_id": "L2"}},
		"Overrides":   {{"person": "a", "action": "status", "value": "do_not_contact", "note": "n"}},
		"Ranked":      {{"lead_id": "L1", "score": "1"}},
	}
	m := loadFrom(t, s)
	m.Put(model.TableIdentities, model.Identity{Key: "new@x.example", Kind: "email", LeadID: "L1"})            // new: append
	m.Put(model.TableIdentities, model.Identity{Key: "old@x.example", Kind: "email", LeadID: "L2"})            // changed: upsert
	m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: "e1", FirstReceivedAt: t0})                         // new: append
	m.Put(model.TableWindowEvents, model.WindowEvent{EventKey: "w1", At: t0})                                  // new: append
	m.Put(model.TableAppliedRows, model.AppliedRow{SourceID: "csv", RowID: "r1", FirstAppliedAt: t0})          // new keyed: upsert
	m.Put(model.TablePeople, model.Person{LeadID: "L3", CreatedAt: t0})                                        // new: upsert
	m.Put(model.TablePeople, m.People["L1"])                                                                   // unchanged: nothing
	m.Delete(model.TablePeople, []string{"L2"})                                                                // delete by key
	m.Put(model.TableOverrides, model.Override{Person: "b", Action: "retry"})                                  // keyless: append
	m.Delete(model.TableOverrides, []string{"a", "status", "do_not_contact", "n"})                             // by all four columns
	m.Put(model.TableLog, model.LogEntry{At: t0, Kind: "tier_change"})                                         // append
	m.Trim(model.TableLog, "at", t0.Add(-time.Hour))                                                           // trim
	m.Put(model.TableRanked, model.RankedRow{LeadID: "L3", Score: 2, Derived: map[string]string{"tier": "A"}}) // replace all
	m.Put(model.ExportTable("warm"), model.ExportRow{LeadID: "L3"})

	got := summary(codec.Encode(m))
	want := []string{
		"Overrides:3:1", "Overrides:1:1",
		"People:3:1", "People:2:1",
		"Identities:1:1", "Identities:2:1",
		"Window events:1:1",
		"Applied rows:2:1",
		"Seen events:1:1",
		"Ranked:0:2",
		"Log:4:0", "Log:1:1",
		"Export warm:2:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes\n got %v\nwant %v", got, want)
	}
	for _, w := range codec.Encode(m) {
		switch w.Op {
		case api.OpUpsert, api.OpDelete:
			if len(w.Key) == 0 {
				t.Errorf("%s op %d has no Key", w.Table, w.Op)
			}
		}
		if w.Table == model.TableOverrides && w.Op == api.OpDelete &&
			!reflect.DeepEqual(w.Key, []string{"person", "action", "value", "note"}) {
			t.Errorf("Overrides delete key = %v", w.Key)
		}
		if w.Table == model.TablePeople && w.Op == api.OpDelete && !reflect.DeepEqual(w.Rows, []api.Row{{"lead_id": "L2"}}) {
			t.Errorf("People delete rows = %v", w.Rows)
		}
	}
	if got := summary(codec.Encode(m, model.TableIdentities)); !reflect.DeepEqual(got, []string{"Identities:1:1", "Identities:2:1"}) {
		t.Errorf("Encode(Identities) = %v", got)
	}
}

func TestStatePrefix(t *testing.T) {
	m := loadFrom(t, memStore{"State": {{"key": "schema_version", "value": "1.0"}}})
	m.SetState("cursor:csv", "5")
	m.SetState("cursor:events", "9")
	m.SetState("last_poll_at", "x")
	w := codec.Encode(m, "State:cursor:")
	if len(w) != 1 || len(w[0].Rows) != 2 || w[0].Table != model.TableState {
		t.Fatalf("Encode(State:cursor:) = %+v", w)
	}
	m.Committed(w)
	w = codec.Encode(m, model.TableState)
	if len(w) != 1 || len(w[0].Rows) != 1 || w[0].Rows[0]["key"] != "last_poll_at" {
		t.Errorf("after committing cursors, State encodes %+v; want only last_poll_at", w)
	}
}

func TestCommittedKeepsLaterChanges(t *testing.T) {
	m := loadFrom(t, memStore{})
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: t0})
	w := codec.Encode(m, model.TablePeople)
	// A change after encoding, before Commit returns.
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: t0.Add(time.Hour)})
	m.Put(model.TablePeople, model.Person{LeadID: "L2", CreatedAt: t0})
	m.Committed(w)
	got := codec.Encode(m, model.TablePeople)
	if len(got) != 1 || len(got[0].Rows) != 2 {
		t.Fatalf("after Committed, People = %+v; want the two later changes", got)
	}
	m.Committed(got)
	if m.Pending() {
		t.Error("everything was committed")
	}
	// Putting the committed value back is no change.
	m.Put(model.TablePeople, model.Person{LeadID: "L2", CreatedAt: t0})
	if m.Pending() {
		t.Error("an unchanged row must not be recorded")
	}
}

func TestDiscard(t *testing.T) {
	m := loadFrom(t, memStore{
		"People":    {{"lead_id": "L1", "created_at": "2026-01-01T00:00:00.000Z"}},
		"Overrides": {{"person": "a", "action": "status", "value": "x"}},
		"State":     {{"key": "schema_version", "value": "1.0"}},
	})
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: t0})
	m.Put(model.TablePeople, model.Person{LeadID: "L2"})
	m.Put(model.TableOverrides, model.Override{Person: "b"})
	m.Delete(model.TableOverrides, []string{"a", "status", "x", ""})
	m.Put(model.TableLog, model.LogEntry{At: t0})
	m.Trim(model.TablePeople, "created_at", t0.Add(24*time.Hour))
	m.Discard()
	if m.Pending() || len(codec.Encode(m)) != 0 {
		t.Errorf("Discard left changes: %v", summary(codec.Encode(m)))
	}
	if len(m.People) != 1 || !m.People["L1"].CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("People after Discard = %+v", m.People)
	}
	if len(m.Overrides) != 1 || m.Overrides[0].Person != "a" || len(m.Log) != 0 {
		t.Errorf("Overrides %v, Log %v after Discard", m.Overrides, m.Log)
	}
}

func TestTrim(t *testing.T) {
	m := loadFrom(t, memStore{"Window events": {
		{"event_key": "old", "at": "2026-01-01T00:00:00.000Z"},
		{"event_key": "new", "at": "2026-03-01T00:00:00.000Z"},
		{"event_key": "undated"},
	}})
	m.Put(model.TableWindowEvents, model.WindowEvent{EventKey: "old2", At: t0.AddDate(-1, 0, 0)}) // new but already old
	cut := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	m.Trim(model.TableWindowEvents, "at", cut)
	if len(m.WindowEvents) != 2 {
		t.Errorf("after Trim: %v; want new and undated", m.WindowEvents)
	}
	w := codec.Encode(m)
	if len(w) != 1 || w[0].Op != api.OpTrim || w[0].Column != "at" || !w[0].Before.Equal(cut) {
		t.Fatalf("Encode = %+v; want one OpTrim and no append of a trimmed row", w)
	}
	m.Committed(w)
	if m.Pending() {
		t.Error("the trim was committed")
	}
}

func TestRankedRewritesAndChunks(t *testing.T) {
	m := loadFrom(t, memStore{"Ranked": {{"lead_id": "L0"}}})
	m.Delete(model.TableRanked, []string{"L0"})
	for i := range 5 {
		m.Put(model.TableRanked, model.RankedRow{LeadID: api.LeadID(fmt.Sprintf("L%d", i+1)), Score: float64(i)})
	}
	w := codec.Encode(m, model.TableRanked)
	if len(w) != 1 || w[0].Op != api.OpReplace || len(w[0].Rows) != 5 {
		t.Fatalf("Ranked = %v", summary(w))
	}
	chunks := codec.Chunk(w[0], 2)
	if got := summary(chunks); !reflect.DeepEqual(got, []string{"Ranked:0:2", "Ranked:1:2", "Ranked:1:1"}) {
		t.Errorf("chunks = %v", got)
	}
	for _, c := range chunks {
		m.Committed([]api.TableWrite{c})
	}
	if m.Pending() {
		t.Errorf("after every chunk committed, still pending: %v", summary(codec.Encode(m)))
	}
	if got := codec.Chunk(api.TableWrite{Op: api.OpUpsert, Rows: make([]api.Row, 3)}, 0); len(got) != 1 {
		t.Error("size 0 leaves the write whole")
	}
}

func TestLoadSchemaVersion(t *testing.T) {
	for _, tt := range []struct {
		stored, want string
		err          error
	}{
		{"", model.SchemaVersion, nil},
		{"0.9", model.SchemaVersion, nil},
		{"1.0", "1.0", nil},
		{"1.4", "1.4", nil},
		{"2.0", "", codec.ErrNewerSchema},
	} {
		s := memStore{}
		if tt.stored != "" {
			s["State"] = []api.Row{{"key": "schema_version", "value": tt.stored}}
		}
		m, err := codec.Load(context.Background(), s)
		if !errors.Is(err, tt.err) {
			t.Errorf("stored %q: err = %v, want %v", tt.stored, err, tt.err)
			continue
		}
		if err == nil && m.StateValue("schema_version") != tt.want {
			t.Errorf("stored %q: schema_version = %q, want %q", tt.stored, m.StateValue("schema_version"), tt.want)
		}
	}
	if _, err := codec.Load(context.Background(), memStore{"State": {{"key": "schema_version", "value": "one"}}}); err == nil {
		t.Error("a malformed version must fail the load")
	}
}

func TestLoadReadsCompaniesAndExports(t *testing.T) {
	m := loadFrom(t, memStore{
		"Companies":   {{"domain": "x.example", "Employees": "12"}},
		"Export warm": {{"lead_id": "L1", "do_not_contact": "yes"}},
		"Log":         {{"at": "2026-01-01T00:00:00.000Z"}},
	}, "warm")
	if len(m.Companies) != 1 || m.Companies[0]["Employees"] != "12" {
		t.Errorf("Companies = %v", m.Companies)
	}
	if !m.Exports["warm"]["L1"].DoNotContact {
		t.Errorf("Exports = %v", m.Exports)
	}
	if len(m.Log) != 0 {
		t.Error("runs do not load Log")
	}
}
