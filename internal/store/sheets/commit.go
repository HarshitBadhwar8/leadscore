package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/api/googleapi"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// MaxCommitBytes is the largest encoded batchUpdate a commit sends; a bigger
// one is ErrTooLarge, under Sheets' 10 MB request cap.
var MaxCommitBytes = 9 << 20 // a variable so tests can reach Sheets' own limit

// MaxCellChars is the most characters a Sheets cell holds.
const MaxCellChars = 50_000

// Commit applies every write in one batchUpdate, all-or-nothing. It reads the
// tabs the writes touch, applies the writes to that copy in memory (so a
// duplicate append key or a bad write fails before anything is sent), and
// sends the difference as row-level requests: deleted rows, changed cells,
// and appended rows. It creates a missing tab and appends a missing column
// the first time a write names it, and returns ErrTooLarge rather than
// splitting.
//
// People type into Overrides (and the other people-owned tabs) at any time,
// and a row delete is by position. So when a commit deletes or changes rows
// of such a tab, it re-reads the tab just before sending and refuses if it
// changed, and re-reads it after: a row that is gone but was not meant to go
// (the person sorted rows at that moment) is put back and the commit returns
// ErrCommittedWithProblems (everything was saved; a person should look).
func (s *Store) Commit(ctx context.Context, writes []api.TableWrite) error {
	for _, w := range writes {
		if err := validate(w); err != nil {
			return err
		}
	}
	if len(writes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	book, err := s.meta(ctx)
	if err != nil {
		return err
	}
	works, order, err := s.load(ctx, book, writes)
	if err != nil {
		return err
	}
	for _, w := range writes {
		if err := works[w.Table].apply(w); err != nil {
			return fmt.Errorf("%s: %w", w.Table, err)
		}
	}
	ids := sheetIDs(book)
	protect := func(name string, id int64) *sheetsapi.Request {
		pr, _ := protectionFor(book, name, id, nil) // a commit never creates an Events tab, the one case that can fail
		return pr
	}
	var reqs []*sheetsapi.Request
	var guarded []*work
	for _, name := range order {
		wk := works[name]
		r, err := wk.requests(ids, protect)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		reqs = append(reqs, r...)
		if wk.snapshot != nil && wk.movesRows() {
			guarded = append(guarded, wk)
		}
	}
	if len(reqs) == 0 {
		return nil
	}
	if err := s.unchanged(ctx, guarded); err != nil {
		return err
	}
	if err := s.batchUpdate(ctx, reqs, readTries); err != nil {
		return err
	}
	return s.verify(ctx, guarded)
}

// batchUpdate sends requests as one batchUpdate, refusing a body over
// MaxCommitBytes and mapping Sheets' "too large" answers to ErrTooLarge.
func (s *Store) batchUpdate(ctx context.Context, reqs []*sheetsapi.Request, tries int) error {
	body := &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: reqs}
	enc, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(enc) > MaxCommitBytes {
		return fmt.Errorf("%w: the batchUpdate body is %d bytes, over %d", api.ErrTooLarge, len(enc), MaxCommitBytes)
	}
	err = retry(ctx, tries, func() error {
		_, err := s.svc.Sheets.Spreadsheets.BatchUpdate(s.id, body).Context(ctx).Do()
		return err
	})
	if tooLarge(err) {
		return fmt.Errorf("%w: %w", api.ErrTooLarge, err)
	}
	if err != nil {
		return fmt.Errorf("writing the spreadsheet: %w", err)
	}
	return nil
}

// tooLarge reports Sheets refusing a request for its size: the request body,
// or the 10-million-cell cap of the workbook.
func tooLarge(err error) bool {
	var e *googleapi.Error
	if !errors.As(err, &e) {
		return false
	}
	m := strings.ToLower(e.Message)
	return e.Code == http.StatusRequestEntityTooLarge ||
		(e.Code == http.StatusBadRequest && (strings.Contains(m, "payload size exceeds") || strings.Contains(m, "too large") ||
			(strings.Contains(m, "above the limit of") && strings.Contains(m, "cells"))))
}

func validate(w api.TableWrite) error {
	if strings.TrimSpace(w.Table) == "" {
		return errors.New("a table write names no table")
	}
	if len(w.Table) > 100 {
		return fmt.Errorf("%s: a tab name may be at most 100 characters", w.Table)
	}
	if isEventsTab(w.Table) {
		return fmt.Errorf("%s: Events tabs are written only through AppendEvents", w.Table)
	}
	for _, r := range w.Rows {
		if _, ok := r[""]; ok {
			return fmt.Errorf("%s: a column name is empty", w.Table)
		}
	}
	if slices.Contains(w.Key, "") {
		return fmt.Errorf("%s: a column name is empty", w.Table)
	}
	switch w.Op {
	case api.OpReplace, api.OpAppend:
	case api.OpUpsert, api.OpDelete:
		if len(w.Key) == 0 {
			return fmt.Errorf("%s: an upsert or delete needs Key columns", w.Table)
		}
	case api.OpTrim:
		if w.Column == "" || w.Before.IsZero() {
			return fmt.Errorf("%s: a trim needs Column and Before", w.Table)
		}
	default:
		return fmt.Errorf("%s: unknown write op %d", w.Table, w.Op)
	}
	return nil
}

// meta reads the spreadsheet's tabs and their protection.
func (s *Store) meta(ctx context.Context) (*sheetsapi.Spreadsheet, error) {
	return s.metaTries(ctx, readTries)
}

// tabOf finds a tab by name; Sheets tab names ignore case.
func tabOf(book *sheetsapi.Spreadsheet, title string) *sheetsapi.Sheet {
	for _, sh := range book.Sheets {
		if sh.Properties != nil && strings.EqualFold(sh.Properties.Title, title) {
			return sh
		}
	}
	return nil
}

func sheetIDs(book *sheetsapi.Spreadsheet) map[int64]bool {
	ids := map[int64]bool{}
	for _, sh := range book.Sheets {
		if sh.Properties != nil {
			ids[sh.Properties.SheetId] = true
		}
	}
	return ids
}

// readMode says how much of a tab a commit must read.
type readMode int

const (
	readHeader readMode = iota // only appends or a replace: the header row
	readKeys                   // appends to a keyed table: the header and key columns
	readAll                    // upserts, deletes or trims, Health, people tabs: every row
)

// wrow is one row of a tab while a commit is applied in memory.
type wrow struct {
	orig  int // position in the tab as read (0 is the first data row); -1 for a new row
	vals  api.Row
	dirty map[string]bool // a row as read: the columns this commit changed
	blank bool            // a blank row as read: kept in place, never matched
	gone  bool            // deleted by this commit
}

// work is one tab while a commit is applied in memory.
type work struct {
	name     string // as the writes name it
	title    string // as the spreadsheet spells it
	def      model.TableDef
	known    bool
	mode     readMode
	exists   bool
	sheetID  int64
	gridRows int64
	gridCols int64
	header   []string // the tab's columns by position ("" for an unnamed one), including ones this commit adds
	oldWidth int      // header length as read
	used     int      // columns holding anything as read, header or data
	rows     []*wrow  // readAll: every row; otherwise only new rows
	deleted  []int    // positions as read of deleted rows
	wipe     bool     // replaced without readAll: every data row goes
	keys     map[string]bool
	idx      map[string]map[string][]*wrow // by key columns, then key value
	snapshot [][]any                       // people-owned tabs: the values as read
}

// load reads, in a few values calls, what each touched tab needs.
func (s *Store) load(ctx context.Context, book *sheetsapi.Spreadsheet, writes []api.TableWrite) (map[string]*work, []string, error) {
	works := map[string]*work{}
	var order []string
	named := map[string]map[string]bool{} // columns each tab's writes name
	for _, w := range writes {
		wk := works[w.Table]
		if wk == nil {
			def, known := model.Def(w.Table)
			wk = &work{name: w.Table, title: w.Table, def: def, known: known, keys: map[string]bool{}}
			if sh := tabOf(book, w.Table); sh != nil {
				wk.exists = true
				wk.title = sh.Properties.Title
				wk.sheetID = sh.Properties.SheetId
				if gp := sh.Properties.GridProperties; gp != nil {
					wk.gridRows, wk.gridCols = gp.RowCount, gp.ColumnCount
				}
			}
			works[w.Table] = wk
			named[w.Table] = map[string]bool{}
			order = append(order, w.Table)
		}
		if w.Op != api.OpTrim {
			for _, r := range w.Rows {
				for c := range r {
					named[w.Table][c] = true
				}
			}
			for _, c := range w.Key {
				named[w.Table][c] = true
			}
		}
		switch {
		case w.Op == api.OpUpsert || w.Op == api.OpDelete || w.Op == api.OpTrim ||
			w.Table == model.TableHealth || peopleOwned(w.Table):
			// Health is read whole so the formula can name the cell holding
			// last_success_at; people tabs so the commit can check them.
			wk.mode = readAll
		case w.Op == api.OpAppend && wk.keyed() && wk.mode < readKeys:
			wk.mode = readKeys
		}
	}

	var ranges []string
	var targets []*work
	for _, name := range order {
		if wk := works[name]; wk.exists {
			r := QuoteTab(wk.title)
			if wk.mode != readAll {
				r += "!1:1"
			}
			ranges = append(ranges, r)
			targets = append(targets, wk)
		}
	}
	vrs, err := s.batchGet(ctx, ranges, renderOption)
	if err != nil {
		return nil, nil, err
	}
	type keyCol struct {
		wk   *work
		name string
	}
	var keyRanges []string
	var keyCols []keyCol
	var widthRanges []string
	var widthTargets []*work
	for i, wk := range targets {
		values := vrs[i].Values
		wk.header = headerOf(wk.name, values)
		wk.oldWidth = len(wk.header)
		wk.used = usedWidth(wk.name, values)
		missing := false
		for c := range named[wk.name] {
			missing = missing || !slices.Contains(wk.header, c)
		}
		switch wk.mode {
		case readAll:
			for p, r := range dataRows(wk.header, values) {
				wk.rows = append(wk.rows, &wrow{orig: p, vals: r, blank: r == nil})
			}
			if peopleOwned(wk.name) {
				wk.snapshot = values
			}
		default:
			if missing {
				// A new column goes after the last used column, which only the
				// whole tab shows.
				widthRanges = append(widthRanges, QuoteTab(wk.title))
				widthTargets = append(widthTargets, wk)
			}
			if wk.mode == readKeys {
				for _, k := range wk.def.Key {
					if c := slices.Index(wk.header, k); c >= 0 {
						keyRanges = append(keyRanges, fmt.Sprintf("%s!%s2:%s", QuoteTab(wk.title), colName(c), colName(c)))
						keyCols = append(keyCols, keyCol{wk, k})
					}
					// A missing key column means every stored key is empty there.
				}
			}
		}
	}
	vrs, err = s.batchGet(ctx, widthRanges, renderOption)
	if err != nil {
		return nil, nil, err
	}
	for i, wk := range widthTargets {
		wk.used = max(wk.used, usedWidth(wk.name, vrs[i].Values))
	}
	vrs, err = s.batchGet(ctx, keyRanges, renderOption)
	if err != nil {
		return nil, nil, err
	}
	// Gather each tab's key columns by name, then join them per row.
	cols := map[*work]map[string][]string{}
	for i, kc := range keyCols {
		if cols[kc.wk] == nil {
			cols[kc.wk] = map[string][]string{}
		}
		vals := make([]string, len(vrs[i].Values))
		for j, row := range vrs[i].Values {
			if len(row) > 0 {
				vals[j] = cellText(row[0])
			}
		}
		cols[kc.wk][kc.name] = vals
	}
	for wk, byCol := range cols {
		n := 0
		for _, v := range byCol {
			n = max(n, len(v))
		}
		for i := range n {
			parts := make([]string, len(wk.def.Key))
			blank := true
			for j, k := range wk.def.Key {
				if v := byCol[k]; i < len(v) {
					parts[j] = v[i]
				}
				blank = blank && parts[j] == ""
			}
			if !blank {
				wk.keys[strings.Join(parts, "\x1f")] = true
			}
		}
	}
	return works, order, nil
}

// usedWidth is how many columns hold anything in values (for Health, before
// its formula column).
func usedWidth(name string, values [][]any) int {
	n := 0
	for _, row := range values {
		for j := len(row) - 1; j >= n; j-- {
			if cellText(row[j]) != "" {
				n = j + 1
				break
			}
		}
	}
	if name == model.TableHealth {
		n = min(n, healthFormulaColumn)
	}
	return n
}

// batchGet reads ranges in one values call per value form; render picks the
// form per tab name (the part before "!"). Results come back in order.
func (s *Store) batchGet(ctx context.Context, ranges []string, render func(string) string) ([]*sheetsapi.ValueRange, error) {
	if len(ranges) == 0 {
		return nil, nil
	}
	byForm := map[string][]int{}
	for i, r := range ranges {
		f := render(unquoteTab(r))
		byForm[f] = append(byForm[f], i)
	}
	out := make([]*sheetsapi.ValueRange, len(ranges))
	for _, f := range slices.Sorted(maps.Keys(byForm)) {
		idx := byForm[f]
		rs := make([]string, len(idx))
		for j, i := range idx {
			rs[j] = ranges[i]
		}
		var resp *sheetsapi.BatchGetValuesResponse
		err := retry(ctx, readTries, func() error {
			var err error
			resp, err = s.svc.Sheets.Spreadsheets.Values.BatchGet(s.id).Ranges(rs...).
				ValueRenderOption(f).Context(ctx).Do()
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("reading the spreadsheet: %w", err)
		}
		if len(resp.ValueRanges) != len(rs) {
			return nil, fmt.Errorf("reading the spreadsheet: asked for %d ranges, got %d", len(rs), len(resp.ValueRanges))
		}
		for j, i := range idx {
			out[i] = resp.ValueRanges[j]
		}
	}
	return out, nil
}

// unquoteTab returns the tab name of an A1 range written by QuoteTab.
func unquoteTab(r string) string {
	if !strings.HasPrefix(r, "'") {
		name, _, _ := strings.Cut(r, "!")
		return name
	}
	var b strings.Builder
	for i := 1; i < len(r); i++ {
		if r[i] == '\'' {
			if i+1 < len(r) && r[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			break
		}
		b.WriteByte(r[i])
	}
	return b.String()
}

func (wk *work) keyed() bool { return wk.known && len(wk.def.Key) > 0 }

func keyOf(cols []string, r api.Row) string {
	parts := make([]string, len(cols))
	for i, k := range cols {
		parts[i] = r[k]
	}
	return strings.Join(parts, "\x1f")
}

// lookup returns the live rows whose cols equal r's, through an index built
// the first time those columns are asked for and kept current after.
func (wk *work) lookup(cols []string, r api.Row) []*wrow {
	sig := strings.Join(cols, "\x1e")
	ix := wk.idx[sig]
	if ix == nil {
		ix = map[string][]*wrow{}
		for _, cur := range wk.rows {
			if !cur.blank && !cur.gone {
				k := keyOf(cols, cur.vals)
				ix[k] = append(ix[k], cur)
			}
		}
		if wk.idx == nil {
			wk.idx = map[string]map[string][]*wrow{}
		}
		wk.idx[sig] = ix
	}
	var out []*wrow
	for _, cur := range ix[keyOf(cols, r)] {
		if !cur.gone {
			out = append(out, cur)
		}
	}
	return out
}

// addColumns adds to the header every column a write names that the tab
// lacks, sorted, after the last used column (never into an unnamed column
// that holds stray data). A new tab gets its fixed columns first
// (Ranked's derived columns after company_domain).
func (wk *work) addColumns(names map[string]bool) {
	if !wk.exists && len(wk.header) == 0 && wk.known {
		wk.header = append(wk.header, wk.def.Columns...)
	}
	var extra []string
	for c := range names {
		if !slices.Contains(wk.header, c) {
			extra = append(extra, c)
		}
	}
	if len(extra) == 0 {
		return
	}
	slices.Sort(extra)
	if !wk.exists && wk.known && wk.def.DynamicAfter != "" && len(wk.header) == len(wk.def.Columns) {
		at := slices.Index(wk.header, wk.def.DynamicAfter) + 1
		wk.header = slices.Insert(wk.header, at, extra...)
		return
	}
	for len(wk.header) < wk.used {
		wk.header = append(wk.header, "")
	}
	wk.header = append(wk.header, extra...)
}

// apply runs one write against the tab in memory.
func (wk *work) apply(w api.TableWrite) error {
	if !wk.exists && len(wk.header) == 0 && (w.Op == api.OpDelete || w.Op == api.OpTrim) {
		return nil // nothing to delete from a tab that does not exist
	}
	if w.Op != api.OpTrim {
		named := map[string]bool{}
		for _, r := range w.Rows {
			for c := range r {
				named[c] = true
			}
		}
		for _, c := range w.Key {
			named[c] = true
		}
		wk.addColumns(named)
	}
	switch w.Op {
	case api.OpReplace:
		if wk.mode == readAll {
			for _, r := range wk.rows {
				wk.drop(r)
			}
		} else {
			wk.wipe = true
		}
		wk.rows, wk.idx = nil, nil
		wk.keys = map[string]bool{}
		return wk.appendRows(w.Rows, false)
	case api.OpAppend:
		return wk.appendRows(w.Rows, wk.keyed())
	case api.OpUpsert:
		for _, r := range w.Rows {
			found := wk.lookup(w.Key, r)
			for _, cur := range found {
				wk.set(cur, r)
			}
			if len(found) == 0 {
				if err := wk.appendRows([]api.Row{r}, false); err != nil {
					return err
				}
			}
		}
	case api.OpDelete:
		for _, r := range w.Rows {
			for _, cur := range wk.lookup(w.Key, r) {
				wk.drop(cur)
			}
		}
	case api.OpTrim:
		if !slices.Contains(wk.header, w.Column) {
			return nil // a column the tab lacks: nothing is older
		}
		cut := model.FormatTime(w.Before)
		for _, cur := range wk.rows {
			if v := cur.vals[w.Column]; !cur.blank && !cur.gone && v != "" && v < cut {
				wk.drop(cur)
			}
		}
	}
	return nil
}

// set writes r's values into a row, marking only the cells that change, and
// forgets any index over a column it changed.
func (wk *work) set(cur *wrow, r api.Row) {
	for c, v := range r {
		if cur.vals[c] == v {
			continue
		}
		cur.vals[c] = v
		if cur.orig >= 0 {
			if cur.dirty == nil {
				cur.dirty = map[string]bool{}
			}
			cur.dirty[c] = true
		}
		for sig := range wk.idx {
			if slices.Contains(strings.Split(sig, "\x1e"), c) {
				delete(wk.idx, sig)
			}
		}
	}
}

func (wk *work) drop(cur *wrow) {
	if cur.gone || cur.blank {
		return
	}
	cur.gone = true
	if cur.orig >= 0 {
		wk.deleted = append(wk.deleted, cur.orig)
	}
}

// appendRows adds new rows. With checkKeys, a key the tab already holds (or
// that this commit already wrote) fails the commit, as the Backend contract
// says.
func (wk *work) appendRows(rows []api.Row, checkKeys bool) error {
	for _, r := range rows {
		if wk.keyed() {
			k := keyOf(wk.def.Key, r)
			if checkKeys && (wk.keys[k] || len(wk.lookup(wk.def.Key, r)) > 0) {
				return fmt.Errorf("appending a key the table already holds (%s)", strings.Join(wk.def.Key, ", "))
			}
			wk.keys[k] = true
		}
		nr := &wrow{orig: -1, vals: maps.Clone(r)}
		if nr.vals == nil {
			nr.vals = api.Row{}
		}
		wk.rows = append(wk.rows, nr)
		for sig, ix := range wk.idx {
			cols := strings.Split(sig, "\x1e")
			ix[keyOf(cols, nr.vals)] = append(ix[keyOf(cols, nr.vals)], nr)
		}
	}
	return nil
}

// movesRows reports whether the commit deletes or changes existing rows.
func (wk *work) movesRows() bool {
	if len(wk.deleted) > 0 || wk.wipe {
		return true
	}
	for _, r := range wk.rows {
		if len(r.dirty) > 0 {
			return true
		}
	}
	return false
}

// width is how many columns a tab's grid needs: its header, or for Health at
// least through H, where the formula lives.
func (wk *work) width() int64 {
	n := int64(len(wk.header))
	if wk.name == model.TableHealth {
		n = max(n, healthFormulaColumn+1)
	}
	return n
}

// machineTabs hold the tool's own records: hidden when created, so a person
// sees what to read and what to type. Ranked, Health, Outcomes, Pushes, Log
// and export tabs stay visible.
var machineTabs = map[string]bool{
	model.TablePeople: true, model.TableIdentities: true, model.TableCompanyFacts: true,
	model.TableWindowEvents: true, model.TableAppliedRows: true, model.TableSeenEvents: true,
	model.TableAppliedOverrides: true, model.TableState: true,
}

func hiddenTab(name string) bool { return machineTabs[name] || isEventsTab(name) }

// requests turns the tab's in-memory changes into batchUpdate requests:
// create or widen the tab, delete rows (bottom first), rewrite changed cells,
// append new rows, and for Health rewrite the H1 formula.
func (wk *work) requests(ids map[int64]bool, protect func(string, int64) *sheetsapi.Request) ([]*sheetsapi.Request, error) {
	if !wk.exists && len(wk.header) == 0 {
		return nil, nil // no write named a column: nothing to create yet
	}
	if wk.name == model.TableHealth && len(wk.header) > healthFormulaColumn {
		return nil, fmt.Errorf("Health has room for %d columns before its H1 formula; a write needs %d",
			healthFormulaColumn, len(wk.header))
	}
	var reqs []*sheetsapi.Request
	if !wk.exists {
		wk.sheetID = newSheetID(ids)
		wk.gridRows, wk.gridCols = 2, wk.width()
		reqs = append(reqs, addTab(wk.sheetID, wk.name, wk.gridCols, hiddenTab(wk.name)))
		reqs = append(reqs, headerCells(wk.sheetID, 0, wk.header))
		if pr := protect(wk.name, wk.sheetID); pr != nil {
			reqs = append(reqs, pr)
		}
	} else {
		if need := wk.width(); need > wk.gridCols {
			reqs = append(reqs, &sheetsapi.Request{AppendDimension: &sheetsapi.AppendDimensionRequest{
				SheetId: wk.sheetID, Dimension: "COLUMNS", Length: need - wk.gridCols}})
			wk.gridCols = need
		}
		for start := wk.oldWidth; start < len(wk.header); start++ {
			if wk.header[start] != "" {
				reqs = append(reqs, headerCells(wk.sheetID, start, wk.header[start:]))
				break
			}
		}
		reqs = append(reqs, wk.deletes()...)
	}

	// Rows kept from the tab, in order, now sit directly under the header;
	// only their changed cells are written, so a team's formula in another
	// column of the row survives.
	pos := 0
	var added []*sheetsapi.RowData
	for _, r := range wk.rows {
		if r.gone {
			continue
		}
		if r.orig < 0 {
			if err := wk.checkCells(r.vals, nil, -1); err != nil {
				return nil, err
			}
			added = append(added, rowData(wk.header, r.vals))
			continue
		}
		if len(r.dirty) > 0 {
			if err := wk.checkCells(r.vals, r.dirty, pos+2); err != nil {
				return nil, err
			}
			reqs = append(reqs, wk.cellRuns(pos, r)...)
		}
		pos++
	}
	if len(added) > 0 {
		reqs = append(reqs, &sheetsapi.Request{AppendCells: &sheetsapi.AppendCellsRequest{
			SheetId: wk.sheetID, Rows: added, Fields: "userEnteredValue"}})
	}
	if wk.name == model.TableHealth {
		var final []api.Row
		for _, r := range wk.rows {
			switch {
			case r.gone:
			case r.blank:
				final = append(final, nil)
			default:
				final = append(final, r.vals)
			}
		}
		reqs = append(reqs, FormulaRequest(wk.sheetID, Formula(wk.header, final)))
	}
	return reqs, nil
}

// checkCells refuses a value longer than a Sheets cell holds, naming the
// tab, row (0 for a new row) and column, before anything is sent.
func (wk *work) checkCells(r api.Row, only map[string]bool, row int) error {
	for c, v := range r {
		if (only == nil || only[c]) && len(v) > MaxCellChars && utf8.RuneCountInString(v) > MaxCellChars {
			where := "a new row"
			if row > 0 {
				where = fmt.Sprintf("row %d", row)
			}
			return fmt.Errorf("%s, column %s: a value of %d characters is longer than the %d a Sheets cell holds",
				where, c, utf8.RuneCountInString(v), MaxCellChars)
		}
	}
	return nil
}

// cellRuns writes a kept row's changed cells, one request per run of
// adjacent changed columns.
func (wk *work) cellRuns(pos int, r *wrow) []*sheetsapi.Request {
	var colsChanged []int
	for c := range r.dirty {
		if i := slices.Index(wk.header, c); i >= 0 {
			colsChanged = append(colsChanged, i)
		}
	}
	slices.Sort(colsChanged)
	var reqs []*sheetsapi.Request
	for i := 0; i < len(colsChanged); {
		j := i
		for j+1 < len(colsChanged) && colsChanged[j+1] == colsChanged[j]+1 {
			j++
		}
		cells := make([]*sheetsapi.CellData, 0, j-i+1)
		for _, c := range colsChanged[i : j+1] {
			cells = append(cells, textCell(r.vals[wk.header[c]]))
		}
		reqs = append(reqs, &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
			Start:  &sheetsapi.GridCoordinate{SheetId: wk.sheetID, RowIndex: int64(1 + pos), ColumnIndex: int64(colsChanged[i])},
			Rows:   []*sheetsapi.RowData{{Values: cells}},
			Fields: "userEnteredValue",
		}})
		i = j + 1
	}
	return reqs
}

// deletes removes deleted rows bottom first, in contiguous ranges. Sheets
// refuses to delete every non-frozen row, so when every data row of the grid
// goes, the top one is cleared instead and later appends reuse it.
func (wk *work) deletes() []*sheetsapi.Request {
	var pos []int
	if wk.wipe {
		for p := 0; p < int(wk.gridRows)-1; p++ {
			pos = append(pos, p)
		}
	} else {
		pos = slices.Clone(wk.deleted)
	}
	if len(pos) == 0 {
		return nil
	}
	slices.Sort(pos)
	slices.Reverse(pos)
	cleared := false
	if len(pos) >= int(wk.gridRows)-1 {
		pos = pos[:len(pos)-1] // keep the top data row (position 0)
		cleared = true
	}
	var reqs []*sheetsapi.Request
	for i := 0; i < len(pos); {
		hi := pos[i]
		j := i
		for j+1 < len(pos) && pos[j+1] == pos[j]-1 {
			j++
		}
		lo := pos[j]
		reqs = append(reqs, &sheetsapi.Request{DeleteDimension: &sheetsapi.DeleteDimensionRequest{
			Range: &sheetsapi.DimensionRange{SheetId: wk.sheetID, Dimension: "ROWS",
				StartIndex: int64(1 + lo), EndIndex: int64(2 + hi)}}})
		i = j + 1
	}
	if cleared {
		blank := make([]*sheetsapi.CellData, wk.gridCols)
		for i := range blank {
			blank[i] = &sheetsapi.CellData{}
		}
		reqs = append(reqs, &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
			Start:  &sheetsapi.GridCoordinate{SheetId: wk.sheetID, RowIndex: 1},
			Rows:   []*sheetsapi.RowData{{Values: blank}},
			Fields: "userEnteredValue",
		}})
	}
	return reqs
}

// unchanged re-reads people-owned tabs the commit deletes or changes rows of,
// just before it is sent, and refuses when one changed since it was read.
func (s *Store) unchanged(ctx context.Context, guarded []*work) error {
	vrs, err := s.batchGet(ctx, guardRanges(guarded), renderOption)
	if err != nil {
		return err
	}
	for i, wk := range guarded {
		if !slices.Equal(rowSigs(wk.snapshot), rowSigs(vrs[i].Values)) {
			return fmt.Errorf("%s changed while it was being saved, so nothing was written; the next try reads it again", wk.title)
		}
	}
	return nil
}

// verify re-reads the guarded tabs after the write. Every row that should be
// there must be, and every row meant to go must be gone. A row that went
// missing (someone sorted rows between the check and the write, so a delete
// by position hit it) is put back. The write itself landed, so it returns
// ErrCommittedWithProblems, never a plain error: a caller that resent the
// writes would fail on keys already appended. The next run reads the tab
// again and deletes the intended row. A row typed in the
// same instant and deleted in place of the intended one cannot be known, so
// the error says to check the tab.
func (s *Store) verify(ctx context.Context, guarded []*work) error {
	if len(guarded) == 0 {
		return nil
	}
	vrs, err := s.batchGet(ctx, guardRanges(guarded), renderOption)
	if err != nil {
		return fmt.Errorf("checking the saved rows: %w", err)
	}
	var restore []*sheetsapi.Request
	var problems []string
	for i, wk := range guarded {
		cols := namedCols(wk.header)
		have := map[string]int{}
		for _, r := range dataRows(wk.header, vrs[i].Values) {
			if r != nil {
				have[keyOf(cols, r)]++
			}
		}
		var back []*sheetsapi.RowData
		stayed := 0
		for _, r := range wk.rows {
			if r.blank || r.gone {
				continue
			}
			if k := keyOf(cols, r.vals); have[k] > 0 {
				have[k]--
			} else {
				back = append(back, rowData(wk.header, r.vals))
			}
		}
		for _, r := range wk.rows {
			if r.gone && r.orig >= 0 {
				if k := keyOf(cols, r.vals); have[k] > 0 {
					have[k]--
					stayed++
				}
			}
		}
		if len(back) > 0 {
			restore = append(restore, &sheetsapi.Request{AppendCells: &sheetsapi.AppendCellsRequest{
				SheetId: wk.sheetID, Rows: back, Fields: "userEnteredValue"}})
			problems = append(problems, fmt.Sprintf("%d row(s) of %s deleted by mistake were put back", len(back), wk.title))
		}
		if stayed > 0 && len(back) == 0 {
			problems = append(problems, fmt.Sprintf("%d row(s) of %s meant to go are still there, so a row typed at that moment may "+
				"have been deleted instead; check the tab", stayed, wk.title))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	msg := "rows moved while saving: " + strings.Join(problems, "; ") + "; the next run deletes the intended rows"
	if len(restore) > 0 {
		if err := s.batchUpdate(ctx, restore, readTries); err != nil {
			return fmt.Errorf("%w: %s, but putting rows back failed: %w", api.ErrCommittedWithProblems, msg, err)
		}
	}
	return fmt.Errorf("%w: %s", api.ErrCommittedWithProblems, msg)
}

func guardRanges(guarded []*work) []string {
	out := make([]string, len(guarded))
	for i, wk := range guarded {
		out[i] = QuoteTab(wk.title)
	}
	return out
}

func namedCols(header []string) []string {
	var out []string
	for _, h := range header {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// rowSigs is one text per sheet row, for comparing two reads of a tab.
func rowSigs(values [][]any) []string {
	out := make([]string, len(values))
	for i, row := range values {
		parts := make([]string, len(row))
		for j, c := range row {
			parts[j] = cellText(c)
		}
		out[i] = strings.Join(parts, "\x1f")
	}
	return out
}

// rowData writes a row's values in header order, as raw text. An empty value,
// and an unnamed column, is an empty cell.
func rowData(header []string, r api.Row) *sheetsapi.RowData {
	cells := make([]*sheetsapi.CellData, len(header))
	for i, h := range header {
		cells[i] = textCell(r[h])
	}
	return &sheetsapi.RowData{Values: cells}
}

func textCell(v string) *sheetsapi.CellData {
	if v == "" {
		return &sheetsapi.CellData{}
	}
	return &sheetsapi.CellData{UserEnteredValue: &sheetsapi.ExtendedValue{StringValue: &v}}
}

// headerCells writes header names, in bold, from column col of row 1.
func headerCells(sheetID int64, col int, names []string) *sheetsapi.Request {
	return &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
		Start:  &sheetsapi.GridCoordinate{SheetId: sheetID, ColumnIndex: int64(col)},
		Rows:   []*sheetsapi.RowData{headerRow(names, nil)},
		Fields: "userEnteredValue,userEnteredFormat.textFormat.bold",
	}}
}

// headerRow is a bold header row, with a note on the columns notes names.
func headerRow(names []string, notes map[string]string) *sheetsapi.RowData {
	cells := make([]*sheetsapi.CellData, len(names))
	for i, n := range names {
		c := textCell(n)
		c.UserEnteredFormat = &sheetsapi.CellFormat{TextFormat: &sheetsapi.TextFormat{Bold: true}}
		c.Note = notes[n]
		cells[i] = c
	}
	return &sheetsapi.RowData{Values: cells}
}

// addTab creates a tab two rows tall (a header and one row: Sheets cannot
// freeze every row) and cols wide, with the header row frozen.
func addTab(sheetID int64, title string, cols int64, hidden bool) *sheetsapi.Request {
	return &sheetsapi.Request{AddSheet: &sheetsapi.AddSheetRequest{Properties: &sheetsapi.SheetProperties{
		SheetId: sheetID, Title: title, Hidden: hidden,
		GridProperties: &sheetsapi.GridProperties{RowCount: 2, ColumnCount: cols, FrozenRowCount: 1}}}}
}

// newSheetID picks an unused positive sheet id (0 would be dropped from the
// request as an empty field) and records it.
func newSheetID(ids map[int64]bool) int64 {
	for {
		id := rand.Int64N(1<<31-2) + 1
		if !ids[id] {
			ids[id] = true
			return id
		}
	}
}
