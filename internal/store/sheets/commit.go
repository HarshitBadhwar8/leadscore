package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/api/googleapi"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// MaxCommitBytes is the largest encoded batchUpdate a commit sends; a bigger
// one is ErrTooLarge (contracts section 4), under Sheets' 10 MB request cap.
var MaxCommitBytes = 9 << 20 // a variable so tests can reach Sheets' own limit

// Commit applies every write in one batchUpdate, all-or-nothing. It reads the
// tabs the writes touch, applies the writes to that copy in memory (so a
// duplicate append key or a bad write fails before anything is sent), and
// sends the difference as row-level requests: deleted rows, rewritten rows,
// and appended rows. It creates a missing tab and appends a missing column
// the first time a write names it, and returns ErrTooLarge rather than
// splitting.
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
	var reqs []*sheetsapi.Request
	for _, name := range order {
		r, err := works[name].requests(book, ids)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		reqs = append(reqs, r...)
	}
	if len(reqs) == 0 {
		return nil
	}
	return s.batchUpdate(ctx, reqs, readTries)
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

func tooLarge(err error) bool {
	var e *googleapi.Error
	if !errors.As(err, &e) {
		return false
	}
	m := strings.ToLower(e.Message)
	return e.Code == http.StatusRequestEntityTooLarge ||
		(e.Code == http.StatusBadRequest && (strings.Contains(m, "payload size exceeds") || strings.Contains(m, "too large")))
}

func validate(w api.TableWrite) error {
	if strings.TrimSpace(w.Table) == "" {
		return errors.New("a table write names no table")
	}
	if len(w.Table) > 100 {
		return fmt.Errorf("%s: a tab name may be at most 100 characters", w.Table)
	}
	if d, ok := model.Def(w.Table); ok && d.Name == model.EventsPrefix {
		return fmt.Errorf("%s: Events tabs are written only through AppendEvents", w.Table)
	}
	check := func(c string) error {
		if c == "" {
			return fmt.Errorf("%s: a column name is empty", w.Table)
		}
		return nil
	}
	for _, r := range w.Rows {
		for c := range r {
			if err := check(c); err != nil {
				return err
			}
		}
	}
	for _, c := range w.Key {
		if err := check(c); err != nil {
			return err
		}
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

func tabOf(book *sheetsapi.Spreadsheet, title string) *sheetsapi.Sheet {
	for _, sh := range book.Sheets {
		if sh.Properties != nil && sh.Properties.Title == title {
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
	readAll                    // upserts, deletes or trims: every row
)

// wrow is one row of a tab while a commit is applied in memory.
type wrow struct {
	orig    int // position in the tab as read (0 is the first data row); -1 for a new row
	vals    api.Row
	changed bool
	blank   bool // a blank row as read: kept in place, never matched
}

// work is one tab while a commit is applied in memory.
type work struct {
	name     string
	def      model.TableDef
	known    bool
	mode     readMode
	exists   bool
	sheetID  int64
	gridRows int64
	gridCols int64
	header   []string // the tab's columns, including ones this commit adds
	oldWidth int      // header width as read
	rows     []*wrow  // readAll: every row; otherwise only new rows
	origN    int      // rows as read (readAll)
	deleted  []int    // positions as read of deleted rows
	wipe     bool     // replaced without readAll: every data row goes
	keys     map[string]bool
	cleared  bool // set by requests: row 1 is cleared rather than deleted
}

// load reads, in at most two values calls, what each touched tab needs.
func (s *Store) load(ctx context.Context, book *sheetsapi.Spreadsheet, writes []api.TableWrite) (map[string]*work, []string, error) {
	works := map[string]*work{}
	var order []string
	for _, w := range writes {
		wk := works[w.Table]
		if wk == nil {
			def, known := model.Def(w.Table)
			wk = &work{name: w.Table, def: def, known: known, keys: map[string]bool{}}
			if sh := tabOf(book, w.Table); sh != nil {
				wk.exists = true
				wk.sheetID = sh.Properties.SheetId
				if gp := sh.Properties.GridProperties; gp != nil {
					wk.gridRows, wk.gridCols = gp.RowCount, gp.ColumnCount
				}
			}
			works[w.Table] = wk
			order = append(order, w.Table)
		}
		switch {
		case w.Op == api.OpUpsert || w.Op == api.OpDelete || w.Op == api.OpTrim || w.Table == model.TableHealth:
			// Health is read whole so the formula can name the cell holding
			// last_success_at.
			wk.mode = readAll
		case w.Op == api.OpAppend && wk.keyed() && wk.mode < readKeys:
			wk.mode = readKeys
		}
	}

	var ranges []string
	var targets []*work
	for _, name := range order {
		wk := works[name]
		if !wk.exists {
			continue
		}
		r := QuoteTab(name)
		if wk.mode != readAll {
			r += "!1:1"
		}
		ranges = append(ranges, r)
		targets = append(targets, wk)
	}
	vrs, err := s.batchGet(ctx, ranges, renderOption)
	if err != nil {
		return nil, nil, err
	}
	var keyRanges []string
	var keyTargets []*work
	for i, wk := range targets {
		values := vrs[i].Values
		wk.header = headerOf(wk.name, values)
		wk.oldWidth = len(wk.header)
		wk.keys = map[string]bool{}
		switch wk.mode {
		case readAll:
			for p, r := range dataRows(wk.header, values) {
				wk.rows = append(wk.rows, &wrow{orig: p, vals: r, blank: r == nil})
			}
			wk.origN = len(wk.rows)
		case readKeys:
			for _, k := range wk.def.Key {
				c := indexOf(wk.header, k)
				if c < 0 {
					continue // the column is missing, so every stored key is empty there
				}
				keyRanges = append(keyRanges, fmt.Sprintf("%s!%s2:%s", QuoteTab(wk.name), colName(c), colName(c)))
				keyTargets = append(keyTargets, wk)
			}
		}
	}
	if len(keyRanges) > 0 {
		vrs, err := s.batchGet(ctx, keyRanges, renderOption)
		if err != nil {
			return nil, nil, err
		}
		// Gather each tab's key columns, then join them per row.
		cols := map[*work]map[string][]string{}
		for i, wk := range keyTargets {
			if cols[wk] == nil {
				cols[wk] = map[string][]string{}
			}
			var vals []string
			for _, row := range vrs[i].Values {
				v := ""
				if len(row) > 0 {
					v = cellText(row[0])
				}
				vals = append(vals, v)
			}
			cols[wk][wk.def.Key[len(cols[wk])]] = vals
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
					if parts[j] != "" {
						blank = false
					}
				}
				if !blank {
					wk.keys[strings.Join(parts, "\x1f")] = true
				}
			}
		}
	}
	return works, order, nil
}

// batchGet reads ranges in one values call; render picks the value form per
// tab name (the part before "!").
func (s *Store) batchGet(ctx context.Context, ranges []string, render func(string) string) ([]*sheetsapi.ValueRange, error) {
	if len(ranges) == 0 {
		return nil, nil
	}
	// One call per value form, results put back in order.
	byForm := map[string][]int{}
	for i, r := range ranges {
		f := render(unquoteTab(r))
		byForm[f] = append(byForm[f], i)
	}
	out := make([]*sheetsapi.ValueRange, len(ranges))
	forms := make([]string, 0, len(byForm))
	for f := range byForm {
		forms = append(forms, f)
	}
	sort.Strings(forms)
	for _, f := range forms {
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

func (wk *work) keyOf(r api.Row) string {
	parts := make([]string, len(wk.def.Key))
	for i, k := range wk.def.Key {
		parts[i] = r[k]
	}
	return strings.Join(parts, "\x1f")
}

// addColumns appends to the header every column a write names that the tab
// lacks, sorted, so the result does not depend on map order. A new tab gets
// its section 4 columns first (Ranked's derived columns after company_domain).
func (wk *work) addColumns(names map[string]bool) {
	if !wk.exists && len(wk.header) == 0 && wk.known {
		wk.header = append(wk.header, wk.def.Columns...)
	}
	var extra []string
	for c := range names {
		if indexOf(wk.header, c) < 0 {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	if len(extra) == 0 {
		return
	}
	if !wk.exists && wk.known && wk.def.DynamicAfter != "" && len(wk.header) == len(wk.def.Columns) {
		at := indexOf(wk.header, wk.def.DynamicAfter) + 1
		wk.header = append(wk.header[:at:at], append(extra, wk.header[at:]...)...)
		return
	}
	wk.header = append(wk.header, extra...)
}

// apply runs one write against the tab in memory.
func (wk *work) apply(w api.TableWrite) error {
	if !wk.exists && len(wk.header) == 0 && (w.Op == api.OpDelete || w.Op == api.OpTrim) {
		return nil // nothing to delete from a tab that does not exist
	}
	named := map[string]bool{}
	for _, r := range w.Rows {
		for c := range r {
			named[c] = true
		}
	}
	for _, c := range w.Key {
		named[c] = true
	}
	if w.Op != api.OpTrim {
		wk.addColumns(named)
	}
	switch w.Op {
	case api.OpReplace:
		if wk.mode == readAll {
			for _, r := range wk.rows {
				if r.orig >= 0 {
					wk.deleted = append(wk.deleted, r.orig)
				}
			}
		} else {
			wk.wipe = true
		}
		wk.rows = nil
		wk.keys = map[string]bool{}
		return wk.appendRows(w.Rows, false)
	case api.OpAppend:
		return wk.appendRows(w.Rows, wk.keyed())
	case api.OpUpsert:
		for _, r := range w.Rows {
			found := false
			for _, cur := range wk.rows {
				if cur.blank || !matchesKey(cur.vals, r, w.Key) {
					continue
				}
				found = true
				for c, v := range r {
					if cur.vals[c] != v {
						cur.vals[c] = v
						cur.changed = true
					}
				}
			}
			if !found {
				if err := wk.appendRows([]api.Row{r}, false); err != nil {
					return err
				}
			}
		}
	case api.OpDelete:
		for _, r := range w.Rows {
			kept := wk.rows[:0]
			for _, cur := range wk.rows {
				if !cur.blank && matchesKey(cur.vals, r, w.Key) {
					if cur.orig >= 0 {
						wk.deleted = append(wk.deleted, cur.orig)
					}
					continue
				}
				kept = append(kept, cur)
			}
			wk.rows = kept
		}
	case api.OpTrim:
		if indexOf(wk.header, w.Column) < 0 {
			return nil // a column the tab lacks: nothing is older
		}
		cut := model.FormatTime(w.Before)
		kept := wk.rows[:0]
		for _, cur := range wk.rows {
			if v := cur.vals[w.Column]; !cur.blank && v != "" && v < cut {
				if cur.orig >= 0 {
					wk.deleted = append(wk.deleted, cur.orig)
				}
				continue
			}
			kept = append(kept, cur)
		}
		wk.rows = kept
	}
	return nil
}

// appendRows adds new rows. With checkKeys, a key the tab already holds (or
// that this commit already wrote) fails the commit (contracts section 1).
func (wk *work) appendRows(rows []api.Row, checkKeys bool) error {
	for _, r := range rows {
		if wk.keyed() {
			k := wk.keyOf(r)
			if checkKeys {
				dup := wk.keys[k]
				for _, cur := range wk.rows {
					if !cur.blank && wk.keyOf(cur.vals) == k {
						dup = true
					}
				}
				if dup {
					return fmt.Errorf("appending a key the table already holds (%s)", strings.Join(wk.def.Key, ", "))
				}
			}
			wk.keys[k] = true
		}
		vals := make(api.Row, len(r))
		for c, v := range r {
			vals[c] = v
		}
		wk.rows = append(wk.rows, &wrow{orig: -1, vals: vals, changed: true})
	}
	return nil
}

func matchesKey(r, want api.Row, key []string) bool {
	for _, k := range key {
		if r[k] != want[k] {
			return false
		}
	}
	return true
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

// requests turns the tab's in-memory changes into batchUpdate requests:
// create or widen the tab, delete rows (bottom first), rewrite changed rows,
// append new rows, and for Health rewrite the H1 formula.
func (wk *work) requests(book *sheetsapi.Spreadsheet, ids map[int64]bool) ([]*sheetsapi.Request, error) {
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
		reqs = append(reqs, addTab(wk.sheetID, wk.name, wk.gridCols, 0))
		reqs = append(reqs, headerCells(wk.sheetID, 0, wk.header))
		if pr := protectionFor(book, wk.name, wk.sheetID); pr != nil {
			reqs = append(reqs, pr)
		}
	} else {
		if need := wk.width(); need > wk.gridCols {
			reqs = append(reqs, &sheetsapi.Request{AppendDimension: &sheetsapi.AppendDimensionRequest{
				SheetId: wk.sheetID, Dimension: "COLUMNS", Length: need - wk.gridCols}})
			wk.gridCols = need
		}
		if len(wk.header) > wk.oldWidth {
			reqs = append(reqs, headerCells(wk.sheetID, wk.oldWidth, wk.header[wk.oldWidth:]))
		}
		reqs = append(reqs, wk.deletes()...)
	}

	// Rows kept from the tab, in order, now sit directly under the header.
	pos := 0
	var run []*sheetsapi.RowData
	runStart := 0
	flush := func() {
		if len(run) > 0 {
			reqs = append(reqs, &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
				Start:  &sheetsapi.GridCoordinate{SheetId: wk.sheetID, RowIndex: int64(1 + runStart)},
				Rows:   run,
				Fields: "userEnteredValue",
			}})
			run = nil
		}
	}
	var added []*sheetsapi.RowData
	for _, r := range wk.rows {
		if r.orig < 0 {
			added = append(added, rowData(wk.header, r.vals))
			continue
		}
		if r.changed {
			if len(run) == 0 {
				runStart = pos
			}
			run = append(run, rowData(wk.header, r.vals))
		} else {
			flush()
		}
		pos++
	}
	flush()
	if len(added) > 0 {
		reqs = append(reqs, &sheetsapi.Request{AppendCells: &sheetsapi.AppendCellsRequest{
			SheetId: wk.sheetID, Rows: added, Fields: "userEnteredValue"}})
	}
	if wk.name == model.TableHealth {
		final := make([]api.Row, len(wk.rows))
		for i, r := range wk.rows {
			if !r.blank {
				final[i] = r.vals
			}
		}
		reqs = append(reqs, FormulaRequest(wk.sheetID, Formula(wk.header, final)))
	}
	return reqs, nil
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
		pos = append(pos, wk.deleted...)
	}
	if len(pos) == 0 {
		return nil
	}
	sort.Sort(sort.Reverse(sort.IntSlice(pos)))
	var reqs []*sheetsapi.Request
	if len(pos) >= int(wk.gridRows)-1 {
		pos = pos[:len(pos)-1] // keep the top data row (position 0)
		wk.cleared = true
	}
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
	if wk.cleared {
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

// rowData writes a row's values in header order, as raw text. An empty value
// is an empty cell.
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
	cells := make([]*sheetsapi.CellData, len(names))
	for i, n := range names {
		c := textCell(n)
		c.UserEnteredFormat = &sheetsapi.CellFormat{TextFormat: &sheetsapi.TextFormat{Bold: true}}
		cells[i] = c
	}
	return &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
		Start:  &sheetsapi.GridCoordinate{SheetId: sheetID, ColumnIndex: int64(col)},
		Rows:   []*sheetsapi.RowData{{Values: cells}},
		Fields: "userEnteredValue,userEnteredFormat.textFormat.bold",
	}}
}

// addTab creates a tab two rows tall (a header and one row: Sheets cannot
// freeze every row) and cols wide, with the header row frozen.
func addTab(sheetID int64, title string, cols int64, index int64) *sheetsapi.Request {
	p := &sheetsapi.SheetProperties{SheetId: sheetID, Title: title,
		GridProperties: &sheetsapi.GridProperties{RowCount: 2, ColumnCount: cols, FrozenRowCount: 1}}
	if index > 0 {
		p.Index = index
	}
	return &sheetsapi.Request{AddSheet: &sheetsapi.AddSheetRequest{Properties: p}}
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

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}
