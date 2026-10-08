// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

func TestKey(t *testing.T) {
	if K("L1") != Key("L1") {
		t.Error("a one-column key must be the value itself")
	}
	k := K("L1", "warm", "contact")
	if got := k.Parts(); !reflect.DeepEqual(got, []string{"L1", "warm", "contact"}) {
		t.Errorf("Parts = %v", got)
	}
}

func TestTablesFollowSection4(t *testing.T) {
	var names []string
	for _, d := range Tables {
		names = append(names, d.Name)
		for _, k := range d.Key {
			if !hasColumn(d, k) {
				t.Errorf("%s: key column %s is not a column", d.Name, k)
			}
		}
	}
	want := "Overrides|Applied overrides|People|Identities|Company facts|Events |Window events|Applied rows|" +
		"Seen events|Outcomes|Ranked|Pushes|Log|Health|State|Export "
	if got := strings.Join(names, "|"); got != want {
		t.Errorf("table order\n got %s\nwant %s", got, want)
	}
	for _, name := range []string{"Export warm", "Events 2026-10", "Events", "People"} {
		if _, ok := Def(name); !ok {
			t.Errorf("Def(%q) not found", name)
		}
	}
	for _, name := range []string{"Export ", "Leads", "Companies"} {
		if _, ok := Def(name); ok {
			t.Errorf("Def(%q) must not match", name)
		}
	}
}

func TestFormats(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("IST", 19800))
	if got := FormatTime(at); got != "2026-01-01T21:34:05.000Z" {
		t.Errorf("FormatTime = %q: must be UTC with milliseconds, never trimmed", got)
	}
	if FormatTime(time.Time{}) != "" {
		t.Error("the zero time is empty")
	}
	if got, err := ParseTime("2026-01-01T21:34:05Z"); err != nil || !got.Equal(at) {
		t.Errorf("ParseTime of RFC 3339 = %v, %v", got, err)
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Error("ParseTime must reject a non-time")
	}
	if FormatFloat(0.1) != "0.1" || FormatFloat(1e21) != "1000000000000000000000" || FormatFloat(-2) != "-2" {
		t.Error("numbers are plain decimal")
	}
	r := ExportRow{LeadID: "L1"}.encode()
	if r["do_not_contact"] != "no" {
		t.Errorf("do_not_contact = %q, want no", r["do_not_contact"])
	}
	if r["first_listed_at"] != "" {
		t.Error("an unset time is an empty cell")
	}
	if got := (Person{LeadID: "L1"}).encode()["fields"]; got != "{}" {
		t.Errorf("an empty JSON column = %q, want {}", got)
	}
	f := (Person{Fields: map[string]Field{"title": {Value: "CTO", SourceID: "csv", At: at}}}).encode()["fields"]
	if f != `{"title":{"value":"CTO","source_id":"csv","at":"2026-01-01T21:34:05.000Z"}}` {
		t.Errorf("fields JSON = %s", f)
	}
}

func TestDecodeRejectsBadCells(t *testing.T) {
	m := New()
	for _, tt := range []struct {
		table string
		row   api.Row
	}{
		{TablePeople, api.Row{"lead_id": "L1", "created_at": "soon"}},
		{TablePeople, api.Row{"lead_id": "L1", "fields": "{not json"}},
		{TablePushes, api.Row{"lead_id": "L1", "attempts": "two"}},
		{"Export warm", api.Row{"lead_id": "L1", "do_not_contact": "maybe"}},
	} {
		if err := m.Load(tt.table, []api.Row{tt.row}); err == nil {
			t.Errorf("Load(%s, %v) must fail", tt.table, tt.row)
		}
	}
}

func TestPutChecksTheRowType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("putting an Identity into People must panic")
		}
	}()
	New().Put(TablePeople, Identity{Key: "a"})
}

func TestUnknownTablePanics(t *testing.T) {
	for _, name := range []string{"Events", "Leads"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Put(%q) must panic", name)
				}
			}()
			New().Put(name, StateRow{Key: "x"})
		}()
	}
}

func TestIndexes(t *testing.T) {
	m := New()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Put(TableIdentities, Identity{Key: "b@x.example", Kind: "email", LeadID: "L1", FirstSeenAt: at.Add(time.Hour)})
	m.Put(TableIdentities, Identity{Key: "a@x.example", Kind: "email", LeadID: "L1", FirstSeenAt: at})
	m.Put(TableIdentities, Identity{Key: "c@y.example", Kind: "email", LeadID: "L2", FirstSeenAt: at})
	ids := m.IdentitiesOf("L1")
	if len(ids) != 2 || ids[0].Key != "a@x.example" {
		t.Errorf("IdentitiesOf(L1) = %v, want oldest first", ids)
	}
	// Moving an identity to another lead moves it in the index.
	m.Put(TableIdentities, Identity{Key: "b@x.example", Kind: "email", LeadID: "L2", FirstSeenAt: at})
	if len(m.IdentitiesOf("L1")) != 1 || len(m.IdentitiesOf("L2")) != 2 {
		t.Errorf("after a move: L1 %v, L2 %v", m.IdentitiesOf("L1"), m.IdentitiesOf("L2"))
	}

	dom := func(d string) map[string]Field { return map[string]Field{CompanyDomainField: {Value: d}} }
	m.Put(TablePeople, Person{LeadID: "L2", Fields: dom("x.example")})
	m.Put(TablePeople, Person{LeadID: "L1", Fields: dom("x.example")})
	m.Put(TablePeople, Person{LeadID: "L3"})
	if got := m.PeopleAt("x.example"); !reflect.DeepEqual(got, []api.LeadID{"L1", "L2"}) {
		t.Errorf("PeopleAt = %v", got)
	}
	m.Put(TablePeople, Person{LeadID: "L1", Fields: dom("y.example")})
	m.Delete(TablePeople, []string{"L2"})
	if len(m.PeopleAt("x.example")) != 0 || len(m.PeopleAt("y.example")) != 1 {
		t.Errorf("after a change and a delete: x %v, y %v", m.PeopleAt("x.example"), m.PeopleAt("y.example"))
	}
	m.Discard()
	if len(m.People) != 0 || len(m.IdentitiesOf("L1")) != 0 || len(m.PeopleAt("y.example")) != 0 {
		t.Error("Discard must empty the indexes with the tables")
	}
}

// Lane ids follow the rubric's rule (letters, digits, "-", "_", starting with a
// letter or digit) and are unique ignoring case: anything else would share a
// SQLite table with another lane. Put refuses it with an error and records
// nothing.
func TestExportLaneRule(t *testing.T) {
	m := New()
	for _, lane := range []string{"", "my lane", "-x", "_x", "a.b", "a/b", "ünï"} {
		if err := m.Put(ExportTable(lane), ExportRow{LeadID: "L1"}); err == nil {
			t.Errorf("Put(Export %q) must be refused", lane)
		}
	}
	if len(m.Exports) != 0 || len(m.Writes()) != 0 {
		t.Fatalf("refused puts recorded something: %v %v", m.Exports, m.Writes())
	}
	if err := m.Put(ExportTable("warm-1_A"), ExportRow{LeadID: "L1"}); err != nil {
		t.Fatalf("a valid lane id: %v", err)
	}
	// A lane recorded in State (loaded or put) blocks its case variants.
	m.SetState(ExportLaneKey+"Cold", "yes")
	for _, lane := range []string{"cold", "COLD", "WARM-1_a"} {
		if err := m.Put(ExportTable(lane), ExportRow{LeadID: "L2"}); err == nil {
			t.Errorf("Put(Export %q) must be refused: differs from a recorded lane only by case", lane)
		}
	}
	if err := m.Put(ExportTable("Cold"), ExportRow{LeadID: "L2"}); err != nil {
		t.Errorf("the recorded lane itself: %v", err)
	}
	// Delete and Trim on a refused lane write nothing.
	before := len(m.Writes())
	m.Delete(ExportTable("cold"), []string{"L2"})
	m.Trim(ExportTable("my lane"), "updated_at", time.Now())
	if len(m.Writes()) != before || len(m.Exports["Cold"]) != 1 {
		t.Errorf("Delete/Trim on a refused lane changed something: %v", m.Writes())
	}
}

// Encoding an export table alone carries its export_lane record, so the table
// is never committed without it; encoding State too does not repeat it.
func TestExportLaneTravelsWithItsTable(t *testing.T) {
	m := New()
	if err := m.Put(ExportTable("warm"), ExportRow{LeadID: "L1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ExportTable("warm2"), ExportRow{LeadID: "L1"}); err != nil {
		t.Fatal(err)
	}
	w := m.Writes(ExportTable("warm"))
	if len(w) != 2 || w[1].Table != TableState || len(w[1].Rows) != 1 || w[1].Rows[0]["key"] != "export_lane:warm" {
		t.Fatalf("Writes(Export warm) = %+v; want the table and only its export_lane row", w)
	}
	m.Committed(w)
	if w := m.Writes(ExportTable("warm")); len(w) != 0 {
		t.Errorf("after commit: %+v", w)
	}
	n := 0
	for _, x := range m.Writes(ExportTable("warm2"), TableState) {
		for _, r := range x.Rows {
			if r["key"] == "export_lane:warm2" {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("export_lane:warm2 encoded %d times, want once", n)
	}
}
