// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package model holds the in-memory model of the store's tables:
// one Go struct per store table, keyed tables in maps by
// primary key, keyless tables (Overrides, Log) as ordered slices.
//
// Every change goes through Put, Delete or Trim, which record what changed;
// Writes (codec.Encode) turns the record into table writes, Committed marks
// what a successful Commit wrote, and Discard drops everything uncommitted. The maps
// are for reading: no slice writes a table any other way.
package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Key is a primary key: the key column values in key column order, joined.
// A one-column key is the value itself, so m.People[model.Key(id)] works.
type Key string

const keySep = "\x1f"

// K builds a Key from the key column values in key column order.
func K(parts ...string) Key { return Key(strings.Join(parts, keySep)) }

// Parts splits a Key back into its column values.
func (k Key) Parts() []string { return strings.Split(string(k), keySep) }

// Model is the in-memory model of every store table. Read the fields; change
// them only through Put, Delete and Trim. A row's maps and slices belong to the
// model once put: to change a row, build a new value and Put it.
type Model struct {
	Overrides        []Override
	AppliedOverrides map[Key]AppliedOverride
	People           map[Key]Person
	Identities       map[Key]Identity
	CompanyFacts     map[Key]CompanyFact
	WindowEvents     map[Key]WindowEvent
	AppliedRows      map[Key]AppliedRow
	SeenEvents       map[Key]SeenEvent
	Outcomes         map[Key]Outcome
	Ranked           map[Key]RankedRow
	Pushes           map[Key]Push
	Log              []LogEntry // Log is never loaded: only the rows put since load
	Health           map[Key]HealthRow
	State            map[Key]StateRow
	Exports          map[string]map[Key]ExportRow // by lane id
	Companies        []api.Row                    // the people-owned Companies tab, read only

	tables           map[string]*tracked // by table name
	identitiesByLead map[api.LeadID]map[Key]bool
	peopleByDomain   map[string]map[api.LeadID]bool
}

// tracked is one table's committed rows (as last loaded or committed) and its
// uncommitted changes.
type tracked struct {
	def  TableDef
	name string
	// Keyed tables.
	base    map[Key]api.Row
	pending map[Key]change
	// Keyless tables.
	baseList []api.Row
	added    []api.Row
	removed  []api.Row // Overrides rows to delete, by all four columns
	trims    []trim
}

type change struct {
	row     api.Row
	deleted bool
}

type trim struct {
	column string
	before time.Time
}

// New returns an empty model.
func New() *Model {
	m := &Model{
		AppliedOverrides: map[Key]AppliedOverride{},
		People:           map[Key]Person{},
		Identities:       map[Key]Identity{},
		CompanyFacts:     map[Key]CompanyFact{},
		WindowEvents:     map[Key]WindowEvent{},
		AppliedRows:      map[Key]AppliedRow{},
		SeenEvents:       map[Key]SeenEvent{},
		Outcomes:         map[Key]Outcome{},
		Ranked:           map[Key]RankedRow{},
		Pushes:           map[Key]Push{},
		Health:           map[Key]HealthRow{},
		State:            map[Key]StateRow{},
		Exports:          map[string]map[Key]ExportRow{},
		tables:           map[string]*tracked{},
		identitiesByLead: map[api.LeadID]map[Key]bool{},
		peopleByDomain:   map[string]map[api.LeadID]bool{},
	}
	return m
}

// track returns the record for a table, creating it on first use. It panics on
// a table the model does not hold: that is a programming error.
func (m *Model) track(table string) *tracked {
	if tr, ok := m.tables[table]; ok {
		return tr
	}
	def, ok := Def(table)
	if !ok || def.Name == EventsPrefix {
		panic(fmt.Sprintf("model: table %q is not in the model", table))
	}
	tr := &tracked{def: def, name: table, base: map[Key]api.Row{}, pending: map[Key]change{}}
	m.tables[table] = tr
	return tr
}

// Load replaces a table's contents with rows read from the store; codec.Load
// calls it once per table. Rows are decoded with the store's cell formats.
func (m *Model) Load(table string, rows []api.Row) error {
	if table == TableCompanies {
		m.Companies = make([]api.Row, 0, len(rows))
		for _, r := range rows {
			m.Companies = append(m.Companies, copyRow(r))
		}
		return nil
	}
	tr := m.track(table)
	m.clearTyped(tr)
	tr.base, tr.pending = map[Key]api.Row{}, map[Key]change{}
	tr.baseList, tr.added, tr.removed, tr.trims = nil, nil, nil, nil
	for i, r := range rows {
		row, err := decode(tr.def, r)
		if err != nil {
			return fmt.Errorf("%s row %d: %w", table, i+1, err)
		}
		// The base is the re-encoded row, so a value stored in another accepted
		// form (a trimmed time) does not count as a change when put back unchanged.
		enc := row.encode()
		if tr.def.Key == nil {
			tr.baseList = append(tr.baseList, enc)
			m.appendTyped(tr, row)
			continue
		}
		k := keyOf(tr.def, enc)
		tr.base[k] = enc
		m.setTyped(tr, k, row)
	}
	return nil
}

// Put records a new or changed row. The row's type must match the table
// (Person for People, ExportRow for "Export <lane id>", ...). On a keyless
// table it appends. The model keeps the row as the store will return it: times
// in UTC cut to milliseconds, JSON numbers as float64, empty unknown columns
// dropped. The first row put in an export table records its lane in State
// (export_lane:<lane id>), so codec.Load finds the table after the lane is gone.
//
// Put returns an error, and records nothing, for an export table whose lane id
// breaks the lane-id rule (letters, digits, "-" and "_", starting with a
// letter or digit) or matches an already recorded lane only ignoring case:
// such tables would share one SQLite table. A row of the wrong type for the
// table is a programming error and panics.
func (m *Model) Put(table string, row Row) error {
	if err := m.checkExportLane(table); err != nil {
		return err
	}
	tr := m.track(table)
	if row == nil || row.table() != tr.def.Name {
		panic(fmt.Sprintf("model: Put(%q) with a %T", table, row))
	}
	norm, err := decode(tr.def, row.encode())
	if err != nil {
		panic(fmt.Sprintf("model: Put(%q): %v", table, err)) // encode always writes what decode reads
	}
	enc := norm.encode()
	if tr.def.Key == nil {
		m.appendTyped(tr, norm)
		tr.added = append(tr.added, enc)
		return nil
	}
	k := keyOf(tr.def, enc)
	m.setTyped(tr, k, norm)
	tr.rederive(k, enc, true)
	if lane, ok := strings.CutPrefix(table, ExportPrefix); ok && m.StateValue(ExportLaneKey+lane) == "" {
		m.SetState(ExportLaneKey+lane, "yes")
	}
	return nil
}

var laneIDForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ValidLaneID reports a lane id that follows the lane-id rule (letters,
// digits, "-" and "_", starting with a letter or digit), so it is safe in a
// table name and a file name.
func ValidLaneID(id string) bool { return laneIDForm.MatchString(id) }

// checkExportLane refuses an export table whose lane id breaks the lane-id
// rule, or that matches a recorded lane only ignoring case. Other tables pass.
func (m *Model) checkExportLane(table string) error {
	lane, ok := strings.CutPrefix(table, ExportPrefix)
	if !ok {
		return nil
	}
	if !laneIDForm.MatchString(lane) {
		return fmt.Errorf("export table %q: a lane id holds only letters, digits, \"-\" and \"_\", starting with a letter or digit", table)
	}
	for k := range m.State {
		rec, ok := strings.CutPrefix(string(k), ExportLaneKey)
		if ok && rec != lane && strings.EqualFold(rec, lane) {
			return fmt.Errorf("export table %q: lane %q differs from the recorded lane %q only by case", table, lane, rec)
		}
	}
	return nil
}

// ExportLaneKey prefixes the State key that records an export table's lane:
// export_lane:<lane id> = yes.
const ExportLaneKey = "export_lane:"

// Delete records a deleted row, by its key in key column order. Overrides
// rows are deleted by all four columns (person, action, value, note), which
// removes every row that matches. Log rows cannot be deleted, only trimmed.
//
// On an export table that Put refuses (see Put) it does nothing: no row can
// exist there.
func (m *Model) Delete(table string, key []string) {
	if m.checkExportLane(table) != nil {
		return
	}
	tr := m.track(table)
	if tr.name == TableLog {
		panic("model: Log is only appended and trimmed")
	}
	if tr.def.Key == nil {
		if len(key) != len(tr.def.Columns) {
			panic(fmt.Sprintf("model: Delete(%q) needs all %d columns", table, len(tr.def.Columns)))
		}
		want := api.Row{}
		for i, c := range tr.def.Columns {
			want[c] = key[i]
		}
		var kept []Override
		for _, o := range m.Overrides {
			if !matchesKey(o.encode(), want, tr.def.Columns) {
				kept = append(kept, o)
			}
		}
		m.Overrides = kept
		m.rediffList(tr)
		return
	}
	if len(key) != len(tr.def.Key) {
		panic(fmt.Sprintf("model: Delete(%q) needs a key of %d columns", table, len(tr.def.Key)))
	}
	k := K(key...)
	m.delTyped(tr, k)
	tr.rederive(k, nil, false)
}

// rederive sets a keyed row's pending change from its current value (present
// false: deleted) against the committed one.
func (tr *tracked) rederive(k Key, cur api.Row, present bool) {
	b, inBase := tr.base[k]
	switch {
	case !present && !inBase, present && inBase && rowsEqual(b, cur):
		delete(tr.pending, k)
	case !present:
		tr.pending[k] = change{deleted: true}
	default:
		tr.pending[k] = change{row: cur}
	}
}

// rediffList sets a keyless table's pending appends (and, for Overrides,
// deletes) from its current rows against the committed ones.
func (m *Model) rediffList(tr *tracked) {
	base := map[string]int{}
	for _, r := range tr.baseList {
		base[rowSig(r)]++
	}
	tr.added = nil
	cur := map[string]int{}
	for _, r := range m.currentList(tr) {
		s := rowSig(r)
		cur[s]++
		if base[s] > 0 {
			base[s]--
			continue
		}
		tr.added = append(tr.added, r)
	}
	if tr.name != TableOverrides {
		return // Log rows are never deleted, only trimmed
	}
	tr.removed = nil
	for _, r := range tr.baseList {
		s := rowSig(r)
		if cur[s] > 0 {
			cur[s]--
			continue
		}
		want := api.Row{}
		for _, c := range tr.def.Columns {
			want[c] = r[c]
		}
		dup := false
		for _, x := range tr.removed {
			dup = dup || matchesKey(x, want, tr.def.Columns)
		}
		if !dup {
			tr.removed = append(tr.removed, want)
		}
	}
}

// Trim records a retention trim: rows whose column holds a time before before
// are removed now, and codec.Encode writes an OpTrim. An empty column is kept.
// On an export table that Put refuses it does nothing.
func (m *Model) Trim(table, column string, before time.Time) {
	if m.checkExportLane(table) != nil {
		return
	}
	tr := m.track(table)
	cut := FormatTime(before)
	old := func(r api.Row) bool { return r[column] != "" && r[column] < cut }
	if tr.def.Key == nil {
		rows := m.currentList(tr)
		m.clearTyped(tr)
		for _, r := range rows {
			if !old(r) {
				row, _ := decode(tr.def, r)
				m.appendTyped(tr, row)
			}
		}
		var added []api.Row
		for _, r := range tr.added {
			if !old(r) {
				added = append(added, r)
			}
		}
		tr.added = added
	} else {
		for k, r := range m.current(tr) {
			if old(r) {
				m.delTyped(tr, k)
				delete(tr.pending, k)
			}
		}
	}
	tr.trims = append(tr.trims, trim{column: column, before: before})
}

// Writes returns the recorded changes of the named tables as table writes, in
// model order (all tables when none is named); codec.Encode is the public name.
// Each change becomes:
//
//   - OpAppend for new rows of Seen events, Window events, Log and Identities,
//     and for new Overrides rows;
//   - OpUpsert for new or changed keyed rows (Applied rows included);
//   - OpDelete for deleted rows, by key (Overrides: by all four columns);
//   - OpReplace for Ranked, the whole table (codec.Chunk splits it);
//   - OpTrim for retention trims, first among the table's writes.
//
// A State name may carry a key prefix ("State:cursor:") to encode only those
// keys. A row put back unchanged produces no write.
func (m *Model) Writes(tables ...string) []api.TableWrite {
	if len(tables) == 0 {
		tables = m.trackedNames()
	}
	var out []api.TableWrite
	for _, name := range tables {
		prefix, hasPrefix := "", false
		if p, ok := strings.CutPrefix(name, TableState+":"); ok {
			name, prefix, hasPrefix = TableState, p, true
		}
		tr, ok := m.tables[name]
		if !ok {
			if _, known := Def(name); !known || name == TableEvents || strings.HasPrefix(name, EventsPrefix) {
				panic(fmt.Sprintf("model: no table %q to encode", name))
			}
			continue
		}
		w := tr.writes(m, prefix, hasPrefix)
		out = append(out, w...)
		// An export table's first rows travel with their export_lane record,
		// so no commit can create the table without recording its lane.
		if lane, ok := strings.CutPrefix(name, ExportPrefix); ok && len(w) > 0 && !covers(tables, ExportLaneKey+lane) {
			for _, sw := range m.Writes(TableState + ":" + ExportLaneKey + lane) {
				var rows []api.Row
				for _, r := range sw.Rows {
					if r["key"] == ExportLaneKey+lane {
						rows = append(rows, r)
					}
				}
				if len(rows) > 0 {
					sw.Rows = rows
					out = append(out, sw)
				}
			}
		}
	}
	return out
}

// covers reports whether the named tables already encode State key k.
func covers(tables []string, k string) bool {
	for _, t := range tables {
		if t == TableState {
			return true
		}
		if p, ok := strings.CutPrefix(t, TableState+":"); ok && strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func (tr *tracked) writes(m *Model, prefix string, hasPrefix bool) []api.TableWrite {
	var out []api.TableWrite
	if !hasPrefix {
		for _, t := range tr.trims {
			out = append(out, api.TableWrite{Table: tr.name, Op: api.OpTrim, Column: t.column, Before: t.before})
		}
	}
	if tr.def.Key == nil {
		if len(tr.removed) > 0 {
			out = append(out, api.TableWrite{Table: tr.name, Op: api.OpDelete,
				Key: append([]string(nil), tr.def.Columns...), Rows: copyRows(tr.removed)})
		}
		if len(tr.added) > 0 {
			out = append(out, api.TableWrite{Table: tr.name, Op: api.OpAppend, Rows: copyRows(tr.added)})
		}
		return out
	}
	if tr.name == TableRanked {
		// Ranked is recomputed every run: any change rewrites the whole table.
		if len(tr.pending) > 0 {
			cur := m.current(tr)
			rows := make([]api.Row, 0, len(cur))
			for _, k := range sortedKeys(cur) {
				rows = append(rows, cur[k])
			}
			out = append(out, api.TableWrite{Table: tr.name, Op: api.OpReplace, Rows: rows})
		}
		return out
	}
	var dels, adds, ups []api.Row
	for _, k := range sortedKeys(tr.pending) {
		if hasPrefix && !strings.HasPrefix(string(k), prefix) {
			continue
		}
		p := tr.pending[k]
		switch _, inBase := tr.base[k]; {
		case p.deleted:
			r := api.Row{}
			for i, part := range k.Parts() {
				r[tr.def.Key[i]] = part
			}
			dels = append(dels, r)
		case appendNew[tr.name] && !inBase:
			adds = append(adds, copyRow(p.row))
		default:
			ups = append(ups, copyRow(p.row))
		}
	}
	key := append([]string(nil), tr.def.Key...)
	if len(dels) > 0 {
		out = append(out, api.TableWrite{Table: tr.name, Op: api.OpDelete, Key: key, Rows: dels})
	}
	if len(adds) > 0 {
		out = append(out, api.TableWrite{Table: tr.name, Op: api.OpAppend, Rows: adds})
	}
	if len(ups) > 0 {
		out = append(out, api.TableWrite{Table: tr.name, Op: api.OpUpsert, Key: key, Rows: ups})
	}
	return out
}

// Committed records that Commit succeeded with these writes: the store now
// holds what they wrote. Every row they touched is compared again with the
// model, so a change made after the writes were encoded (even one back to the
// old value, or a delete of a row the write added) stays recorded.
func (m *Model) Committed(writes []api.TableWrite) {
	for _, w := range writes {
		tr, ok := m.tables[w.Table]
		if !ok {
			continue
		}
		touched := tr.applyToBase(w)
		if tr.def.Key == nil {
			m.rediffList(tr)
			continue
		}
		for k := range touched {
			cur, ok := m.rowAt(tr, k)
			if ok {
				tr.rederive(k, cur.encode(), true)
			} else {
				tr.rederive(k, nil, false)
			}
		}
	}
}

// applyToBase mirrors a committed write onto the committed rows and returns
// the keys it touched (keyed tables).
func (tr *tracked) applyToBase(w api.TableWrite) map[Key]bool {
	keyed := tr.def.Key != nil
	touched := map[Key]bool{}
	switch w.Op {
	case api.OpTrim:
		cut := FormatTime(w.Before)
		old := func(r api.Row) bool { return r[w.Column] != "" && r[w.Column] < cut }
		if keyed {
			for k, r := range tr.base {
				if old(r) {
					delete(tr.base, k)
					touched[k] = true
				}
			}
		} else {
			var kept []api.Row
			for _, r := range tr.baseList {
				if !old(r) {
					kept = append(kept, r)
				}
			}
			tr.baseList = kept
		}
		for i, t := range tr.trims {
			if t.column == w.Column && t.before.Equal(w.Before) {
				tr.trims = append(tr.trims[:i:i], tr.trims[i+1:]...)
				break
			}
		}
	case api.OpDelete:
		for _, r := range w.Rows {
			if keyed {
				k := keyOf(tr.def, r)
				delete(tr.base, k)
				touched[k] = true
				continue
			}
			tr.baseList = removeMatching(tr.baseList, r, w.Key)
		}
	case api.OpAppend, api.OpUpsert:
		for _, r := range w.Rows {
			if keyed {
				k := keyOf(tr.def, r)
				tr.base[k] = copyRow(r)
				touched[k] = true
				continue
			}
			tr.baseList = append(tr.baseList, copyRow(r))
		}
	case api.OpReplace:
		if keyed {
			for k := range tr.base {
				touched[k] = true
			}
			tr.base = map[Key]api.Row{}
			for _, r := range w.Rows {
				k := keyOf(tr.def, r)
				tr.base[k] = copyRow(r)
				touched[k] = true
			}
		} else {
			tr.baseList = copyRows(w.Rows)
		}
	}
	return touched
}

// Discard drops every uncommitted change: the model returns to what was last
// loaded or committed (dry-run, and the reload after ErrTooLarge).
func (m *Model) Discard() {
	for _, tr := range m.tables {
		m.clearTyped(tr)
		tr.pending = map[Key]change{}
		tr.added, tr.removed, tr.trims = nil, nil, nil
		if tr.def.Key == nil {
			for _, r := range tr.baseList {
				row, _ := decode(tr.def, r) // base rows were encoded by this package
				m.appendTyped(tr, row)
			}
			continue
		}
		for k, r := range tr.base {
			row, _ := decode(tr.def, r)
			m.setTyped(tr, k, row)
		}
	}
}

// StateValue returns a State value, or empty.
func (m *Model) StateValue(key string) string { return m.State[Key(key)].Value }

// SetState puts a State value, keeping the row's unknown columns.
func (m *Model) SetState(key, value string) {
	row := m.State[Key(key)]
	row.Key, row.Value = key, value
	m.Put(TableState, row)
}

// IdentitiesOf returns a lead's own identities (not those of leads merged into
// it), oldest first.
func (m *Model) IdentitiesOf(lead api.LeadID) []Identity {
	out := make([]Identity, 0, len(m.identitiesByLead[lead]))
	for k := range m.identitiesByLead[lead] {
		out = append(out, m.Identities[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeenAt.Equal(out[j].FirstSeenAt) {
			return out[i].FirstSeenAt.Before(out[j].FirstSeenAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// PeopleAt returns the leads whose People.fields["company.domain"] is domain,
// sorted. Merged leads are included; callers follow merged_into.
func (m *Model) PeopleAt(domain string) []api.LeadID {
	out := make([]api.LeadID, 0, len(m.peopleByDomain[domain]))
	for id := range m.peopleByDomain[domain] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CompanyDomainField is the People.fields name of a lead's company domain.
const CompanyDomainField = "company.domain"

// trackedNames lists every tracked table in the store's table order, export
// tables last and sorted.
func (m *Model) trackedNames() []string {
	var out, exports []string
	for _, d := range Tables {
		if _, ok := m.tables[d.Name]; ok && !d.Pattern {
			out = append(out, d.Name)
		}
	}
	for name := range m.tables {
		if strings.HasPrefix(name, ExportPrefix) {
			exports = append(exports, name)
		}
	}
	sort.Strings(exports)
	return append(out, exports...)
}

// Typed access, one case per table.

func (m *Model) setTyped(tr *tracked, k Key, row Row) {
	switch r := row.(type) {
	case AppliedOverride:
		m.AppliedOverrides[k] = r
	case Person:
		if old, ok := m.People[k]; ok {
			m.unindexPerson(old)
		}
		m.People[k] = r
		if d := r.Fields[CompanyDomainField].Value; d != "" {
			if m.peopleByDomain[d] == nil {
				m.peopleByDomain[d] = map[api.LeadID]bool{}
			}
			m.peopleByDomain[d][r.LeadID] = true
		}
	case Identity:
		if old, ok := m.Identities[k]; ok {
			delete(m.identitiesByLead[old.LeadID], k)
		}
		m.Identities[k] = r
		if m.identitiesByLead[r.LeadID] == nil {
			m.identitiesByLead[r.LeadID] = map[Key]bool{}
		}
		m.identitiesByLead[r.LeadID][k] = true
	case CompanyFact:
		m.CompanyFacts[k] = r
	case WindowEvent:
		m.WindowEvents[k] = r
	case AppliedRow:
		m.AppliedRows[k] = r
	case SeenEvent:
		m.SeenEvents[k] = r
	case Outcome:
		m.Outcomes[k] = r
	case RankedRow:
		m.Ranked[k] = r
	case Push:
		m.Pushes[k] = r
	case HealthRow:
		m.Health[k] = r
	case StateRow:
		m.State[k] = r
	case ExportRow:
		lane := strings.TrimPrefix(tr.name, ExportPrefix)
		if m.Exports[lane] == nil {
			m.Exports[lane] = map[Key]ExportRow{}
		}
		m.Exports[lane][k] = r
	}
}

func (m *Model) unindexPerson(p Person) {
	if d := p.Fields[CompanyDomainField].Value; d != "" {
		delete(m.peopleByDomain[d], p.LeadID)
		if len(m.peopleByDomain[d]) == 0 {
			delete(m.peopleByDomain, d)
		}
	}
}

func (m *Model) delTyped(tr *tracked, k Key) {
	switch tr.name {
	case TableAppliedOverrides:
		delete(m.AppliedOverrides, k)
	case TablePeople:
		if old, ok := m.People[k]; ok {
			m.unindexPerson(old)
		}
		delete(m.People, k)
	case TableIdentities:
		if old, ok := m.Identities[k]; ok {
			delete(m.identitiesByLead[old.LeadID], k)
			if len(m.identitiesByLead[old.LeadID]) == 0 {
				delete(m.identitiesByLead, old.LeadID)
			}
		}
		delete(m.Identities, k)
	case TableCompanyFacts:
		delete(m.CompanyFacts, k)
	case TableWindowEvents:
		delete(m.WindowEvents, k)
	case TableAppliedRows:
		delete(m.AppliedRows, k)
	case TableSeenEvents:
		delete(m.SeenEvents, k)
	case TableOutcomes:
		delete(m.Outcomes, k)
	case TableRanked:
		delete(m.Ranked, k)
	case TablePushes:
		delete(m.Pushes, k)
	case TableHealth:
		delete(m.Health, k)
	case TableState:
		delete(m.State, k)
	default:
		delete(m.Exports[strings.TrimPrefix(tr.name, ExportPrefix)], k)
	}
}

func (m *Model) appendTyped(_ *tracked, row Row) {
	switch r := row.(type) {
	case Override:
		m.Overrides = append(m.Overrides, r)
	case LogEntry:
		m.Log = append(m.Log, r)
	}
}

// clearTyped empties a table's typed rows (and the indexes over them).
func (m *Model) clearTyped(tr *tracked) {
	switch tr.name {
	case TableOverrides:
		m.Overrides = nil
		return
	case TableLog:
		m.Log = nil
		return
	}
	for k := range m.current(tr) {
		m.delTyped(tr, k)
	}
}

// current returns a keyed table's typed rows, encoded.
func (m *Model) current(tr *tracked) map[Key]api.Row {
	out := map[Key]api.Row{}
	add := func(k Key, r Row) { out[k] = r.encode() }
	switch tr.name {
	case TableAppliedOverrides:
		for k, r := range m.AppliedOverrides {
			add(k, r)
		}
	case TablePeople:
		for k, r := range m.People {
			add(k, r)
		}
	case TableIdentities:
		for k, r := range m.Identities {
			add(k, r)
		}
	case TableCompanyFacts:
		for k, r := range m.CompanyFacts {
			add(k, r)
		}
	case TableWindowEvents:
		for k, r := range m.WindowEvents {
			add(k, r)
		}
	case TableAppliedRows:
		for k, r := range m.AppliedRows {
			add(k, r)
		}
	case TableSeenEvents:
		for k, r := range m.SeenEvents {
			add(k, r)
		}
	case TableOutcomes:
		for k, r := range m.Outcomes {
			add(k, r)
		}
	case TableRanked:
		for k, r := range m.Ranked {
			add(k, r)
		}
	case TablePushes:
		for k, r := range m.Pushes {
			add(k, r)
		}
	case TableHealth:
		for k, r := range m.Health {
			add(k, r)
		}
	case TableState:
		for k, r := range m.State {
			add(k, r)
		}
	default:
		for k, r := range m.Exports[strings.TrimPrefix(tr.name, ExportPrefix)] {
			add(k, r)
		}
	}
	return out
}

// rowAt returns a keyed table's typed row at k.
func (m *Model) rowAt(tr *tracked, k Key) (Row, bool) {
	var r Row
	var ok bool
	switch tr.name {
	case TableAppliedOverrides:
		r, ok = m.AppliedOverrides[k]
	case TablePeople:
		r, ok = m.People[k]
	case TableIdentities:
		r, ok = m.Identities[k]
	case TableCompanyFacts:
		r, ok = m.CompanyFacts[k]
	case TableWindowEvents:
		r, ok = m.WindowEvents[k]
	case TableAppliedRows:
		r, ok = m.AppliedRows[k]
	case TableSeenEvents:
		r, ok = m.SeenEvents[k]
	case TableOutcomes:
		r, ok = m.Outcomes[k]
	case TableRanked:
		r, ok = m.Ranked[k]
	case TablePushes:
		r, ok = m.Pushes[k]
	case TableHealth:
		r, ok = m.Health[k]
	case TableState:
		r, ok = m.State[k]
	default:
		r, ok = m.Exports[strings.TrimPrefix(tr.name, ExportPrefix)][k]
	}
	return r, ok
}

// currentList returns a keyless table's typed rows, encoded, in order.
func (m *Model) currentList(tr *tracked) []api.Row {
	var out []api.Row
	switch tr.name {
	case TableOverrides:
		for _, r := range m.Overrides {
			out = append(out, r.encode())
		}
	case TableLog:
		for _, r := range m.Log {
			out = append(out, r.encode())
		}
	}
	return out
}

// Helpers.

func keyOf(def TableDef, r api.Row) Key {
	parts := make([]string, len(def.Key))
	for i, c := range def.Key {
		parts[i] = r[c]
	}
	return K(parts...)
}

// rowsEqual compares two rows, a missing column counting as empty.
func rowsEqual(a, b api.Row) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// rowSig is a row's identity for multiset comparison: its non-empty columns.
func rowSig(r api.Row) string {
	cols := make([]string, 0, len(r))
	for c, v := range r {
		if v != "" {
			cols = append(cols, c+"\x00"+v)
		}
	}
	sort.Strings(cols)
	return strings.Join(cols, "\x1e")
}

func matchesKey(r, want api.Row, cols []string) bool {
	for _, c := range cols {
		if r[c] != want[c] {
			return false
		}
	}
	return true
}

func removeMatching(rows []api.Row, want api.Row, cols []string) []api.Row {
	var out []api.Row
	for _, r := range rows {
		if !matchesKey(r, want, cols) {
			out = append(out, r)
		}
	}
	return out
}

func copyRow(r api.Row) api.Row {
	out := make(api.Row, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

func copyRows(rows []api.Row) []api.Row {
	out := make([]api.Row, len(rows))
	for i, r := range rows {
		out[i] = copyRow(r)
	}
	return out
}

func sortedKeys[V any](m map[Key]V) []Key {
	out := make([]Key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
