// Package sheets is an in-memory fake of the parts of the Google Sheets v4 and
// Drive v3 APIs that leadscore uses (contracts section 12.1): spreadsheet
// create and get, batchUpdate with the requests the Sheets store sends,
// values get and batchGet, and Drive permissions. It is an http.Handler, so a
// test serves it with httptest and points a store's `base_url` at it.
//
// It keeps the real API's rules that the store depends on: a batchUpdate is
// all-or-nothing, tab names are unique, appendCells writes after the last row
// with data, a tab cannot lose all its non-frozen rows or freeze all its rows,
// a cell holds at most 50,000 characters, a workbook at most 10 million cells,
// and a request body at most 10 MB. Reads trim trailing empty rows and cells,
// and a range naming a missing tab is "Unable to parse range".
//
// Formulas are not evaluated: a formula cell reads back as its formula text.
package sheets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/api/drive/v3"
	sheetsapi "google.golang.org/api/sheets/v4"
)

// Limits the real API enforces.
const (
	MaxCellChars  = 50_000
	MaxCells      = 10_000_000
	MaxBodyBytes  = 10 << 20
	defaultRows   = 1000
	defaultColumn = 26
)

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	mu         sync.Mutex
	books      map[string]*book
	nextID     int
	slowDown   int            // the next slowDown calls answer 429
	calls      map[string]int // by kind: "create", "get", "batchUpdate", "values", "permissions"
	reads      []string       // "<render> <range>" for every range read
	shareBlock string         // sharing any spreadsheet answers 403 with this message
}

type book struct {
	props      sheetsapi.SpreadsheetProperties
	tabs       []*tab
	perms      []*drive.Permission
	denied     bool   // every call answers 403
	shareBlock string // permissions.create answers 403 with this message
	nextRange  int64
}

type tab struct {
	props     sheetsapi.SheetProperties // GridProperties always set
	protected []*sheetsapi.ProtectedRange
	cells     [][]cell // one slice per grid row; a row may be shorter than the grid
}

type cell struct {
	kind byte // 0 empty, 's' string, 'n' number, 'b' bool, 'f' formula
	s    string
	n    float64
	b    bool
}

// New returns an empty fake.
func New() *Server {
	return &Server{books: map[string]*book{}, calls: map[string]int{}}
}

// NewSpreadsheet creates an empty spreadsheet with one tab, "Sheet1", as the
// real API does, and returns its id.
func (s *Server) NewSpreadsheet(title string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, b := s.newBook(title)
	b.tabs = []*tab{newTab(0, "Sheet1", defaultRows, defaultColumn)}
	return id
}

func (s *Server) newBook(title string) (string, *book) {
	s.nextID++
	id := fmt.Sprintf("fake-sheet-%d", s.nextID)
	b := &book{props: sheetsapi.SpreadsheetProperties{Title: title, AutoRecalc: "ON_CHANGE", TimeZone: "America/Los_Angeles", Locale: "en_US"}}
	s.books[id] = b
	return id, b
}

func newTab(id int64, title string, rows, cols int64) *tab {
	return &tab{
		props: sheetsapi.SheetProperties{SheetId: id, Title: title, SheetType: "GRID",
			GridProperties: &sheetsapi.GridProperties{RowCount: rows, ColumnCount: cols}},
		cells: make([][]cell, rows),
	}
}

// SlowDown makes the next n calls answer 429 RESOURCE_EXHAUSTED, Google's
// "slow down".
func (s *Server) SlowDown(n int) {
	s.mu.Lock()
	s.slowDown = n
	s.mu.Unlock()
}

// Deny makes every call on the spreadsheet answer 403, as when it is not shared
// with the caller.
func (s *Server) Deny(id string, denied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.books[id]; b != nil {
		b.denied = denied
	}
}

// BlockSharing makes sharing the spreadsheet (every spreadsheet, for an empty
// id) fail with Drive's 403 and msg, as a Workspace sharing policy does. An
// empty msg allows sharing again.
func (s *Server) BlockSharing(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		s.shareBlock = msg
		return
	}
	if b := s.books[id]; b != nil {
		b.shareBlock = msg
	}
}

// ValueReads returns every range read through values get and batchGet, as
// "<valueRenderOption> <range>", in order.
func (s *Server) ValueReads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reads...)
}

// IsFormula reports whether a cell (A1 form) holds a formula rather than text.
func (s *Server) IsFormula(id, title, a1 string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[id]
	if b == nil {
		return false
	}
	t := b.tabNamed(title)
	r, err := parseRange(title + "!" + a1)
	if t == nil || err != nil || r.r0 >= int64(len(t.cells)) || r.c0 >= int64(len(t.cells[r.r0])) {
		return false
	}
	return t.cells[r.r0][r.c0].kind == 'f'
}

// Calls returns how many calls of a kind were served: "create", "get",
// "batchUpdate", "values" (get and batchGet) or "permissions".
func (s *Server) Calls(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[kind]
}

// IDs returns every spreadsheet id, in creation order.
func (s *Server) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.books {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return idNum(out[i]) < idNum(out[j]) })
	return out
}

func idNum(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "fake-sheet-"))
	return n
}

// Put writes rows into a tab as a person typing would, creating the tab when
// missing and growing it to fit. A value is a string, a float64, an int or a
// bool; a string starting with "=" is a formula.
func (s *Server) Put(id, title string, rows [][]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[id]
	if b == nil {
		return fmt.Errorf("no spreadsheet %s", id)
	}
	t := b.tabNamed(title)
	if t == nil {
		t = newTab(b.freeSheetID(), title, 1, 1)
		t.props.Index = int64(len(b.tabs))
		b.tabs = append(b.tabs, t)
	}
	width := int64(0)
	for _, r := range rows {
		width = max(width, int64(len(r)))
	}
	t.grow(int64(len(rows)), width)
	for i, r := range rows {
		row := make([]cell, len(r))
		for j, v := range r {
			switch x := v.(type) {
			case nil:
			case string:
				if strings.HasPrefix(x, "=") {
					row[j] = cell{kind: 'f', s: x}
				} else if x != "" {
					row[j] = cell{kind: 's', s: x}
				}
			case float64:
				row[j] = cell{kind: 'n', n: x}
			case int:
				row[j] = cell{kind: 'n', n: float64(x)}
			case bool:
				row[j] = cell{kind: 'b', b: x}
			default:
				return fmt.Errorf("unsupported value %T", v)
			}
		}
		t.cells[i] = row
	}
	return nil
}

// Spreadsheet returns a copy of a spreadsheet's properties and tabs (with
// their protected ranges), as spreadsheets.get returns them; nil when missing.
func (s *Server) Spreadsheet(id string) *sheetsapi.Spreadsheet {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[id]
	if b == nil {
		return nil
	}
	return b.view(id)
}

// Cell returns the text of one cell given in A1 form ("H1"), a formula as its
// formula text; "" when empty or missing.
func (s *Server) Cell(id, title, a1 string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[id]
	if b == nil {
		return ""
	}
	t := b.tabNamed(title)
	if t == nil {
		return ""
	}
	r, err := parseRange(title + "!" + a1)
	if err != nil || r.r0 < 0 || r.r0 >= int64(len(t.cells)) {
		return ""
	}
	row := t.cells[r.r0]
	if r.c0 >= int64(len(row)) {
		return ""
	}
	return row[r.c0].text(true)
}

// Permissions returns the Drive permissions made on a spreadsheet.
func (s *Server) Permissions(id string) []*drive.Permission {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[id]
	if b == nil {
		return nil
	}
	out := make([]*drive.Permission, len(b.perms))
	for i, p := range b.perms {
		c := *p
		out[i] = &c
	}
	return out
}

// ServeHTTP routes the Sheets v4 and Drive v3 paths the store uses.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if len(body) > MaxBodyBytes {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("Request payload size exceeds the limit: %d bytes.", MaxBodyBytes))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slowDown > 0 {
		s.slowDown--
		apiError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED",
			"Quota exceeded for quota metric 'Write requests' and limit 'Write requests per minute per user'")
		return
	}
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/v4/spreadsheets"):
		s.serveSheets(w, r, strings.TrimPrefix(path, "/v4/spreadsheets"), body)
	case strings.HasPrefix(path, "/drive/v3/files/"):
		s.serveDrive(w, r, strings.TrimPrefix(path, "/drive/v3/files/"), body)
	default:
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such method: "+r.Method+" "+path)
	}
}

func (s *Server) serveSheets(w http.ResponseWriter, r *http.Request, rest string, body []byte) {
	if rest == "" || rest == "/" {
		if r.Method != http.MethodPost {
			apiError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "method not allowed")
			return
		}
		s.calls["create"]++
		s.create(w, body)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	id, tail, _ := strings.Cut(rest, "/")
	id, action, _ := strings.Cut(id, ":")
	id, _ = url.PathUnescape(id)
	b := s.books[id]
	if b == nil {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	if b.denied {
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED", "The caller does not have permission")
		return
	}
	switch {
	case action == "batchUpdate" && tail == "" && r.Method == http.MethodPost:
		s.calls["batchUpdate"]++
		s.batchUpdate(w, id, b, body)
	case action == "" && tail == "" && r.Method == http.MethodGet:
		s.calls["get"]++
		writeJSON(w, b.view(id))
	case tail == "values:batchGet" && r.Method == http.MethodGet:
		s.calls["values"]++
		q := r.URL.Query()
		resp := &sheetsapi.BatchGetValuesResponse{SpreadsheetId: id}
		for _, rg := range q["ranges"] {
			s.reads = append(s.reads, q.Get("valueRenderOption")+" "+rg)
			vr, err := b.values(rg, q.Get("valueRenderOption"))
			if err != nil {
				apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
				return
			}
			resp.ValueRanges = append(resp.ValueRanges, vr)
		}
		writeJSON(w, resp)
	case strings.HasPrefix(tail, "values/") && r.Method == http.MethodGet:
		s.calls["values"]++
		rg, err := url.PathUnescape(strings.TrimPrefix(tail, "values/"))
		if err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		s.reads = append(s.reads, r.URL.Query().Get("valueRenderOption")+" "+rg)
		vr, err := b.values(rg, r.URL.Query().Get("valueRenderOption"))
		if err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		writeJSON(w, vr)
	default:
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such method: "+r.Method+" "+r.URL.Path)
	}
}

func (s *Server) serveDrive(w http.ResponseWriter, r *http.Request, rest string, body []byte) {
	idEsc, tail, _ := strings.Cut(rest, "/")
	id, _ := url.PathUnescape(idEsc)
	b := s.books[id]
	if b == nil {
		apiError(w, http.StatusNotFound, "notFound", "File not found: "+id+".")
		return
	}
	if b.denied {
		apiError(w, http.StatusNotFound, "notFound", "File not found: "+id+".")
		return
	}
	if tail == "" && r.Method == http.MethodDelete {
		delete(s.books, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if tail != "permissions" {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such method")
		return
	}
	s.calls["permissions"]++
	switch r.Method {
	case http.MethodPost:
		if msg := b.shareBlock + s.shareBlock; msg != "" {
			apiError(w, http.StatusForbidden, "forbidden", msg)
			return
		}
		var p drive.Permission
		if err := json.Unmarshal(body, &p); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		if p.Type == "" || p.Role == "" || (p.Type == "user" && p.EmailAddress == "") {
			apiError(w, http.StatusBadRequest, "required", "The permission type, role and emailAddress are required.")
			return
		}
		for _, old := range b.perms {
			if strings.EqualFold(old.EmailAddress, p.EmailAddress) && old.Type == p.Type {
				old.Role = p.Role
				writeJSON(w, old)
				return
			}
		}
		p.Id = fmt.Sprintf("perm-%d", len(b.perms)+1)
		p.Kind = "drive#permission"
		b.perms = append(b.perms, &p)
		writeJSON(w, &p)
	case http.MethodGet:
		writeJSON(w, &drive.PermissionList{Permissions: b.perms})
	default:
		apiError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "method not allowed")
	}
}

func (s *Server) create(w http.ResponseWriter, body []byte) {
	var req sheetsapi.Spreadsheet
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	nb := &book{}
	nb.props = sheetsapi.SpreadsheetProperties{Title: "Untitled spreadsheet", AutoRecalc: "ON_CHANGE",
		TimeZone: "America/Los_Angeles", Locale: "en_US"}
	if p := req.Properties; p != nil {
		if p.Title != "" {
			nb.props.Title = p.Title
		}
		if p.AutoRecalc != "" {
			nb.props.AutoRecalc = p.AutoRecalc
		}
		if p.TimeZone != "" {
			nb.props.TimeZone = p.TimeZone
		}
		if p.Locale != "" {
			nb.props.Locale = p.Locale
		}
	}
	if len(req.Sheets) == 0 {
		nb.tabs = []*tab{newTab(0, "Sheet1", defaultRows, defaultColumn)}
	}
	for i, sh := range req.Sheets {
		p := sh.Properties
		if p == nil {
			p = &sheetsapi.SheetProperties{}
		}
		if err := nb.addSheet(p); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Invalid sheets[%d]: %v", i, err))
			return
		}
		t := nb.tabs[len(nb.tabs)-1]
		for _, d := range sh.Data {
			if err := t.write(d.StartRow, d.StartColumn, d.RowData, true, false); err != nil {
				apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Invalid sheets[%d]: %v", i, err))
				return
			}
		}
		for _, pr := range sh.ProtectedRanges {
			nb.nextRange++
			c := *pr
			c.ProtectedRangeId = nb.nextRange
			t.protected = append(t.protected, &c)
		}
	}
	if err := nb.checkCells(); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	s.nextID++
	id := fmt.Sprintf("fake-sheet-%d", s.nextID)
	s.books[id] = nb
	writeJSON(w, nb.view(id))
}

func (s *Server) batchUpdate(w http.ResponseWriter, id string, b *book, body []byte) {
	var req sheetsapi.BatchUpdateSpreadsheetRequest
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	// All-or-nothing: work on a copy and keep it only when every request applied.
	nb := b.clone()
	resp := &sheetsapi.BatchUpdateSpreadsheetResponse{SpreadsheetId: id}
	for i, rq := range req.Requests {
		reply, err := nb.apply(rq)
		if err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Invalid requests[%d]: %v", i, err))
			return
		}
		resp.Replies = append(resp.Replies, reply)
	}
	if err := nb.checkCells(); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	*b = *nb
	writeJSON(w, resp)
}

// clone copies the book; tabs are copied on first write (see mut).
func (b *book) clone() *book {
	nb := *b
	nb.tabs = make([]*tab, len(b.tabs))
	for i, t := range b.tabs {
		nb.tabs[i] = t.copy()
	}
	nb.perms = append([]*drive.Permission(nil), b.perms...)
	return &nb
}

func (t *tab) copy() *tab {
	c := &tab{props: t.props}
	gp := *t.props.GridProperties
	c.props.GridProperties = &gp
	c.protected = append([]*sheetsapi.ProtectedRange(nil), t.protected...)
	c.cells = make([][]cell, len(t.cells))
	copy(c.cells, t.cells) // rows are replaced, never changed in place (see setRow)
	return c
}

func (b *book) apply(rq *sheetsapi.Request) (*sheetsapi.Response, error) {
	reply := &sheetsapi.Response{}
	switch {
	case rq.AddSheet != nil:
		p := rq.AddSheet.Properties
		if p == nil {
			p = &sheetsapi.SheetProperties{}
		}
		if err := b.addSheet(p); err != nil {
			return nil, fmt.Errorf("addSheet: %w", err)
		}
		props := b.tabs[len(b.tabs)-1].props
		reply.AddSheet = &sheetsapi.AddSheetResponse{Properties: &props}
	case rq.DeleteSheet != nil:
		i := b.tabIndex(rq.DeleteSheet.SheetId)
		if i < 0 {
			return nil, fmt.Errorf("deleteSheet: No sheet with id: %d", rq.DeleteSheet.SheetId)
		}
		if len(b.tabs) == 1 {
			return nil, fmt.Errorf("deleteSheet: You can't remove all the sheets in a document.")
		}
		b.tabs = append(b.tabs[:i:i], b.tabs[i+1:]...)
		for j, t := range b.tabs {
			t.props.Index = int64(j)
		}
	case rq.UpdateSheetProperties != nil:
		return reply, b.updateSheetProps(rq.UpdateSheetProperties)
	case rq.UpdateSpreadsheetProperties != nil:
		p := rq.UpdateSpreadsheetProperties.Properties
		if p == nil {
			return nil, fmt.Errorf("updateSpreadsheetProperties: no properties")
		}
		for _, f := range fieldList(rq.UpdateSpreadsheetProperties.Fields) {
			switch f {
			case "autoRecalc":
				b.props.AutoRecalc = p.AutoRecalc
			case "timeZone":
				b.props.TimeZone = p.TimeZone
			case "title":
				b.props.Title = p.Title
			case "locale":
				b.props.Locale = p.Locale
			default:
				return nil, fmt.Errorf("updateSpreadsheetProperties: unsupported field %q", f)
			}
		}
	case rq.UpdateCells != nil:
		u := rq.UpdateCells
		values, formats, err := cellFields(u.Fields)
		if err != nil {
			return nil, fmt.Errorf("updateCells: %w", err)
		}
		var sheetID, row, col int64
		switch {
		case u.Start != nil:
			sheetID, row, col = u.Start.SheetId, u.Start.RowIndex, u.Start.ColumnIndex
		case u.Range != nil:
			sheetID, row, col = u.Range.SheetId, u.Range.StartRowIndex, u.Range.StartColumnIndex
		default:
			return nil, fmt.Errorf("updateCells: no start or range")
		}
		t := b.tabByID(sheetID)
		if t == nil {
			return nil, fmt.Errorf("updateCells: No grid with id: %d", sheetID)
		}
		if err := t.write(row, col, u.Rows, values, formats); err != nil {
			return nil, fmt.Errorf("updateCells: %w", err)
		}
	case rq.AppendCells != nil:
		a := rq.AppendCells
		values, formats, err := cellFields(a.Fields)
		if err != nil {
			return nil, fmt.Errorf("appendCells: %w", err)
		}
		t := b.tabByID(a.SheetId)
		if t == nil {
			return nil, fmt.Errorf("appendCells: No grid with id: %d", a.SheetId)
		}
		start := t.lastDataRow() + 1
		if need := start + int64(len(a.Rows)); need > t.props.GridProperties.RowCount {
			t.grow(need, 0)
		}
		if err := t.write(start, 0, a.Rows, values, formats); err != nil {
			return nil, fmt.Errorf("appendCells: %w", err)
		}
	case rq.DeleteDimension != nil:
		return reply, b.deleteDimension(rq.DeleteDimension.Range)
	case rq.AppendDimension != nil:
		a := rq.AppendDimension
		t := b.tabByID(a.SheetId)
		if t == nil {
			return nil, fmt.Errorf("appendDimension: No grid with id: %d", a.SheetId)
		}
		if a.Length <= 0 {
			return nil, fmt.Errorf("appendDimension: length must be positive")
		}
		gp := t.props.GridProperties
		switch a.Dimension {
		case "ROWS":
			t.grow(gp.RowCount+a.Length, 0)
		case "COLUMNS":
			t.grow(0, gp.ColumnCount+a.Length)
		default:
			return nil, fmt.Errorf("appendDimension: bad dimension %q", a.Dimension)
		}
	case rq.AddProtectedRange != nil:
		pr := rq.AddProtectedRange.ProtectedRange
		if pr == nil || pr.Range == nil {
			return nil, fmt.Errorf("addProtectedRange: no range")
		}
		t := b.tabByID(pr.Range.SheetId)
		if t == nil {
			return nil, fmt.Errorf("addProtectedRange: No grid with id: %d", pr.Range.SheetId)
		}
		b.nextRange++
		c := *pr
		c.ProtectedRangeId = b.nextRange
		if c.Editors != nil {
			e := *c.Editors
			e.Users = append([]string(nil), e.Users...)
			c.Editors = &e
		}
		t.protected = append(t.protected, &c)
		reply.AddProtectedRange = &sheetsapi.AddProtectedRangeResponse{ProtectedRange: &c}
	default:
		raw, _ := json.Marshal(rq)
		return nil, fmt.Errorf("the fake does not support this request: %s", raw)
	}
	return reply, nil
}

func (b *book) addSheet(p *sheetsapi.SheetProperties) error {
	title := p.Title
	if title == "" {
		title = fmt.Sprintf("Sheet%d", len(b.tabs)+1)
	}
	if b.tabNamed(title) != nil {
		return fmt.Errorf("A sheet with the name \"%s\" already exists. Please enter another name.", title)
	}
	id := p.SheetId
	if id == 0 {
		id = b.freeSheetID()
	} else if b.tabByID(id) != nil {
		return fmt.Errorf("Sheet with id %d already exists.", id)
	}
	rows, cols := int64(defaultRows), int64(defaultColumn)
	var frozen int64
	if gp := p.GridProperties; gp != nil {
		if gp.RowCount > 0 {
			rows = gp.RowCount
		}
		if gp.ColumnCount > 0 {
			cols = gp.ColumnCount
		}
		frozen = gp.FrozenRowCount
	}
	if frozen >= rows {
		return fmt.Errorf("You can't freeze all visible rows on the sheet.")
	}
	t := newTab(id, title, rows, cols)
	t.props.GridProperties.FrozenRowCount = frozen
	t.props.TabColorStyle = p.TabColorStyle
	t.props.Index = int64(len(b.tabs))
	b.tabs = append(b.tabs, t)
	return nil
}

func (b *book) updateSheetProps(u *sheetsapi.UpdateSheetPropertiesRequest) error {
	p := u.Properties
	if p == nil {
		return fmt.Errorf("updateSheetProperties: no properties")
	}
	t := b.tabByID(p.SheetId)
	if t == nil {
		return fmt.Errorf("updateSheetProperties: No grid with id: %d", p.SheetId)
	}
	gp := p.GridProperties
	if gp == nil {
		gp = &sheetsapi.GridProperties{}
	}
	for _, f := range fieldList(u.Fields) {
		switch f {
		case "title":
			if o := b.tabNamed(p.Title); o != nil && o != t {
				return fmt.Errorf("A sheet with the name \"%s\" already exists. Please enter another name.", p.Title)
			}
			t.props.Title = p.Title
		case "gridProperties.frozenRowCount":
			t.props.GridProperties.FrozenRowCount = gp.FrozenRowCount
		case "gridProperties.rowCount":
			t.resize(gp.RowCount, t.props.GridProperties.ColumnCount)
		case "gridProperties.columnCount":
			t.resize(t.props.GridProperties.RowCount, gp.ColumnCount)
		case "tabColorStyle":
			t.props.TabColorStyle = p.TabColorStyle
		default:
			return fmt.Errorf("updateSheetProperties: unsupported field %q", f)
		}
	}
	if t.props.GridProperties.FrozenRowCount >= t.props.GridProperties.RowCount {
		return fmt.Errorf("updateSheetProperties: You can't freeze all visible rows on the sheet.")
	}
	return nil
}

func (b *book) deleteDimension(r *sheetsapi.DimensionRange) error {
	if r == nil {
		return fmt.Errorf("deleteDimension: no range")
	}
	t := b.tabByID(r.SheetId)
	if t == nil {
		return fmt.Errorf("deleteDimension: No grid with id: %d", r.SheetId)
	}
	gp := t.props.GridProperties
	switch r.Dimension {
	case "ROWS":
		if r.StartIndex < 0 || r.EndIndex <= r.StartIndex || r.EndIndex > gp.RowCount {
			return fmt.Errorf("deleteDimension: bad row range [%d, %d) of %d rows", r.StartIndex, r.EndIndex, gp.RowCount)
		}
		if gp.RowCount-(r.EndIndex-r.StartIndex) <= gp.FrozenRowCount {
			return fmt.Errorf("deleteDimension: Sorry, it is not possible to delete all non-frozen rows.")
		}
		cells := make([][]cell, 0, len(t.cells)-int(r.EndIndex-r.StartIndex))
		cells = append(cells, t.cells[:r.StartIndex]...)
		t.cells = append(cells, t.cells[r.EndIndex:]...)
		gp.RowCount -= r.EndIndex - r.StartIndex
	case "COLUMNS":
		if r.StartIndex < 0 || r.EndIndex <= r.StartIndex || r.EndIndex > gp.ColumnCount {
			return fmt.Errorf("deleteDimension: bad column range")
		}
		if gp.ColumnCount-(r.EndIndex-r.StartIndex) <= 0 {
			return fmt.Errorf("deleteDimension: Sorry, it is not possible to delete all non-frozen columns.")
		}
		for i, row := range t.cells {
			if int64(len(row)) <= r.StartIndex {
				continue
			}
			nr := append([]cell(nil), row[:r.StartIndex]...)
			if int64(len(row)) > r.EndIndex {
				nr = append(nr, row[r.EndIndex:]...)
			}
			t.cells[i] = nr
		}
		gp.ColumnCount -= r.EndIndex - r.StartIndex
	default:
		return fmt.Errorf("deleteDimension: bad dimension %q", r.Dimension)
	}
	return nil
}

func cellFields(fields string) (values, formats bool, err error) {
	for _, f := range fieldList(fields) {
		switch {
		case f == "*":
			values, formats = true, true
		case f == "userEnteredValue":
			values = true
		case f == "userEnteredFormat" || strings.HasPrefix(f, "userEnteredFormat."):
			formats = true
		default:
			return false, false, fmt.Errorf("unsupported field %q", f)
		}
	}
	if !values && !formats {
		return false, false, fmt.Errorf("fields is required")
	}
	return values, formats, nil
}

func fieldList(fields string) []string {
	var out []string
	for _, f := range strings.Split(fields, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// write sets cells from rows starting at (row, col). With values, a cell with
// no userEnteredValue is cleared. Formats are accepted and not kept.
func (t *tab) write(row, col int64, rows []*sheetsapi.RowData, values, _ bool) error {
	gp := t.props.GridProperties
	for i, rd := range rows {
		r := row + int64(i)
		if rd == nil {
			continue
		}
		if r >= gp.RowCount || col+int64(len(rd.Values)) > gp.ColumnCount {
			return fmt.Errorf("Range (%s!R%dC%d:R%dC%d) exceeds grid limits. Max rows: %d, max columns: %d",
				t.props.Title, r+1, col+1, r+1, col+int64(len(rd.Values)), gp.RowCount, gp.ColumnCount)
		}
		if !values {
			continue
		}
		cur := t.cells[r]
		nr := make([]cell, max(int64(len(cur)), col+int64(len(rd.Values))))
		copy(nr, cur)
		for j, cd := range rd.Values {
			var c cell
			if cd != nil && cd.UserEnteredValue != nil {
				v := cd.UserEnteredValue
				switch {
				case v.StringValue != nil:
					if len([]rune(*v.StringValue)) > MaxCellChars {
						return fmt.Errorf("Your input contains more than the maximum of %d characters in a single cell.", MaxCellChars)
					}
					if *v.StringValue != "" {
						c = cell{kind: 's', s: *v.StringValue}
					}
				case v.NumberValue != nil:
					c = cell{kind: 'n', n: *v.NumberValue}
				case v.BoolValue != nil:
					c = cell{kind: 'b', b: *v.BoolValue}
				case v.FormulaValue != nil:
					c = cell{kind: 'f', s: *v.FormulaValue}
				}
			}
			nr[col+int64(j)] = c
		}
		t.cells[r] = nr
	}
	return nil
}

// lastDataRow is the index of the last row holding any value, or -1.
func (t *tab) lastDataRow() int64 {
	for i := len(t.cells) - 1; i >= 0; i-- {
		for _, c := range t.cells[i] {
			if c.kind != 0 {
				return int64(i)
			}
		}
	}
	return -1
}

// grow raises the grid to at least rows and cols (0 leaves one alone).
func (t *tab) grow(rows, cols int64) {
	gp := t.props.GridProperties
	t.resize(max(rows, gp.RowCount), max(cols, gp.ColumnCount))
}

func (t *tab) resize(rows, cols int64) {
	gp := t.props.GridProperties
	for int64(len(t.cells)) < rows {
		t.cells = append(t.cells, nil)
	}
	t.cells = t.cells[:rows]
	if cols < gp.ColumnCount {
		for i, row := range t.cells {
			if int64(len(row)) > cols {
				t.cells[i] = append([]cell(nil), row[:cols]...)
			}
		}
	}
	gp.RowCount, gp.ColumnCount = rows, cols
}

func (b *book) checkCells() error {
	var n int64
	for _, t := range b.tabs {
		n += t.props.GridProperties.RowCount * t.props.GridProperties.ColumnCount
	}
	if n > MaxCells {
		return fmt.Errorf("This action would increase the number of cells in the workbook above the limit of %d cells.", MaxCells)
	}
	return nil
}

func (b *book) tabNamed(title string) *tab {
	for _, t := range b.tabs {
		if t.props.Title == title {
			return t
		}
	}
	return nil
}

func (b *book) tabByID(id int64) *tab {
	if i := b.tabIndex(id); i >= 0 {
		return b.tabs[i]
	}
	return nil
}

func (b *book) tabIndex(id int64) int {
	for i, t := range b.tabs {
		if t.props.SheetId == id {
			return i
		}
	}
	return -1
}

func (b *book) freeSheetID() int64 {
	for {
		id := rand.Int64N(1<<31-1) + 1
		if b.tabByID(id) == nil {
			return id
		}
	}
}

func (b *book) view(id string) *sheetsapi.Spreadsheet {
	props := b.props
	out := &sheetsapi.Spreadsheet{SpreadsheetId: id, Properties: &props,
		SpreadsheetUrl: "https://docs.google.com/spreadsheets/d/" + id + "/edit"}
	for _, t := range b.tabs {
		p := t.props
		gp := *t.props.GridProperties
		p.GridProperties = &gp
		sh := &sheetsapi.Sheet{Properties: &p}
		for _, pr := range t.protected {
			c := *pr
			sh.ProtectedRanges = append(sh.ProtectedRanges, &c)
		}
		out.Sheets = append(out.Sheets, sh)
	}
	return out
}

// values reads a range as values.get does: trailing empty rows and trailing
// empty cells in a row are left out.
func (b *book) values(rg, render string) (*sheetsapi.ValueRange, error) {
	r, err := parseRange(rg)
	if err != nil {
		return nil, err
	}
	t := b.tabNamed(r.title)
	if t == nil {
		return nil, fmt.Errorf("Unable to parse range: %s", rg)
	}
	gp := t.props.GridProperties
	r1, c1 := r.r1, r.c1
	if r1 < 0 || r1 >= gp.RowCount {
		r1 = gp.RowCount - 1
	}
	if c1 < 0 || c1 >= gp.ColumnCount {
		c1 = gp.ColumnCount - 1
	}
	vr := &sheetsapi.ValueRange{MajorDimension: "ROWS",
		Range: fmt.Sprintf("%s!%s%d:%s%d", quoteTitle(r.title), colName(r.c0), r.r0+1, colName(c1), r1+1)}
	var rows [][]any
	lastNonEmpty := -1
	for i := r.r0; i <= r1; i++ {
		var out []any
		last := -1
		row := t.cells[i]
		for j := r.c0; j <= c1 && j < int64(len(row)); j++ {
			if row[j].kind != 0 {
				last = int(j - r.c0)
			}
		}
		for j := 0; j <= last; j++ {
			out = append(out, row[r.c0+int64(j)].value(render))
		}
		if last >= 0 {
			lastNonEmpty = len(rows)
		}
		rows = append(rows, out)
	}
	vr.Values = rows[:lastNonEmpty+1]
	if len(vr.Values) == 0 {
		vr.Values = nil
	}
	return vr, nil
}

func (c cell) value(render string) any {
	switch render {
	case "UNFORMATTED_VALUE":
		switch c.kind {
		case 'n':
			return c.n
		case 'b':
			return c.b
		}
		return c.text(true)
	case "FORMULA":
		return c.text(true)
	default: // FORMATTED_VALUE
		return c.text(true)
	}
}

func (c cell) text(formula bool) string {
	switch c.kind {
	case 's':
		return c.s
	case 'n':
		return strconv.FormatFloat(c.n, 'f', -1, 64)
	case 'b':
		if c.b {
			return "TRUE"
		}
		return "FALSE"
	case 'f':
		if formula {
			return c.s
		}
	}
	return ""
}

// a1 is a parsed A1 range; r1 and c1 are -1 when unbounded.
type a1 struct {
	title          string
	r0, c0, r1, c1 int64
}

// parseRange reads "Title", "T!1:1", "T!B2:B", "T!H1" and a quoted title
// with any apostrophe doubled, as in "'Seen events'!A1:D".
func parseRange(s string) (a1, error) {
	r := a1{r1: -1, c1: -1}
	rest := s
	if strings.HasPrefix(s, "'") {
		var b strings.Builder
		i := 1
		for ; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				break
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return r, fmt.Errorf("Unable to parse range: %s", s)
		}
		r.title, rest = b.String(), s[i+1:]
	} else {
		var ok bool
		r.title, rest, ok = strings.Cut(s, "!")
		if ok {
			rest = "!" + rest
		}
	}
	if rest == "" {
		return r, nil
	}
	cells, ok := strings.CutPrefix(rest, "!")
	if !ok || cells == "" {
		return r, fmt.Errorf("Unable to parse range: %s", s)
	}
	from, to, hasTo := strings.Cut(cells, ":")
	fr, fc, err := parseCell(from)
	if err != nil {
		return r, fmt.Errorf("Unable to parse range: %s", s)
	}
	r.r0, r.c0 = max(fr, 0), max(fc, 0)
	if !hasTo {
		r.r1, r.c1 = fr, fc
		if fr < 0 {
			r.r1 = -1
		}
		if fc < 0 {
			r.c1 = -1
		}
		return r, nil
	}
	tr, tc, err := parseCell(to)
	if err != nil {
		return r, fmt.Errorf("Unable to parse range: %s", s)
	}
	r.r1, r.c1 = tr, tc
	return r, nil
}

// parseCell reads "B2" (1, 1), "B" (-1, 1) or "2" (1, -1), zero-based.
func parseCell(s string) (row, col int64, err error) {
	i := 0
	col = -1
	for i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		if col < 0 {
			col = 0
		}
		col = col*26 + int64(s[i]-'A'+1)
		i++
	}
	if col > 0 {
		col--
	}
	row = -1
	if i < len(s) {
		n, err := strconv.ParseInt(s[i:], 10, 64)
		if err != nil || n < 1 {
			return 0, 0, fmt.Errorf("bad cell %q", s)
		}
		row = n - 1
	}
	if row < 0 && col < 0 {
		return 0, 0, fmt.Errorf("bad cell %q", s)
	}
	return row, col, nil
}

func colName(c int64) string {
	name := ""
	for c++; c > 0; c = (c - 1) / 26 {
		name = string(rune('A'+(c-1)%26)) + name
	}
	return name
}

func quoteTitle(t string) string { return "'" + strings.ReplaceAll(t, "'", "''") + "'" }

func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Write(buf.Bytes())
}

// apiError writes Google's JSON error shape, which googleapi.CheckResponse reads.
func apiError(w http.ResponseWriter, code int, status, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": msg, "status": status,
		"errors": []map[string]any{{"message": msg, "reason": status}},
	}})
}
