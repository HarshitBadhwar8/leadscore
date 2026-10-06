// Package csv is the CSV file source (contracts section 12.5). It only parses:
// UTF-8 with an optional byte-order mark, comma-delimited, ragged rows allowed,
// headers returned as written. Merge, not this package, applies aliases to
// input rows, checks email shapes and computes row ids.
//
// A source marked `events: true` returns one event per row instead (contracts
// section 5.2). Finding the event, time and person columns there uses the
// built-in alias table from internal/api.
//
// It registers as source type `csv`:
//
//	sources:
//	  - { id: conference, type: csv, path: in/saastr.csv }
//	  - { id: site, type: csv, path: in/visitors.csv, events: true }
package csv

import (
	"bytes"
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

func init() { api.RegisterSource("csv", New) }

// Source reads one CSV file. It is a snapshot source: every Fetch returns the
// whole file and ignores the cursor.
type Source struct {
	id     string
	path   string
	events bool
	now    func() time.Time // stamps Event.ReceivedAt
}

// New builds a source from its sources[] entry: `id` and `path` are required,
// `events` is optional. The file is read at Fetch, not here, so a file that
// appears later is picked up.
func New(cfg api.Config) (api.Source, error) {
	id, _ := cfg["id"].(string)
	if id == "" {
		return nil, errors.New("csv source: `id` is required")
	}
	path, _ := cfg["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("csv source %q: `path` is required", id)
	}
	events := false
	if v, has := cfg["events"]; has && v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("csv source %q: `events` must be true or false", id)
		}
		events = b
	}
	return &Source{id: id, path: path, events: events, now: time.Now}, nil
}

func (s *Source) ID() string { return s.id }

// Limits that keep one bad file from exhausting memory (contracts section
// 12.5). Variables, not constants, so tests can lower them.
var (
	maxFileBytes int64 = 100 << 20  // 100 MB
	maxColumns         = 1000       // header columns
	maxCells           = 10_000_000 // cells, each row counted at least as wide as the header
)

// Fetch reads the whole file. A plain source returns rows; an events source
// returns events. A file the source cannot read safely fails the whole fetch
// with a message saying how to fix it, so a bad file is fixed rather than
// half-read.
func (s *Source) Fetch(ctx context.Context, _ api.Cursor) ([]api.InputRow, []api.Event, api.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
	}
	data, err := readFile(s.path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("csv source %q: %w", s.id, err)
	}
	headers, records, err := parse(data)
	if err != nil {
		return nil, nil, "", fmt.Errorf("csv source %q: %s: %w", s.id, s.path, err)
	}
	if s.events {
		events, err := s.toEvents(headers, records)
		if err != nil {
			return nil, nil, "", fmt.Errorf("csv source %q: %s: %w", s.id, s.path, err)
		}
		return nil, events, "", nil
	}
	return s.toRows(headers, records), nil, "", nil
}

// readFile reads at most maxFileBytes, and fails rather than truncating.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFileBytes {
		return nil, fmt.Errorf("%s: the file is larger than %d MB, the most a csv source reads; split it", path, maxFileBytes>>20)
	}
	return data, nil
}

// record is one data line with its line number in the file, for reject reasons.
type record struct {
	line  int
	cells []string
}

var bom = []byte("\xEF\xBB\xBF")

var errCROnly = errors.New("line endings are CR only; save the file as CSV UTF-8")

// parse splits the file into headers and data records. Lines whose cells are
// all empty are dropped: a spreadsheet export often ends with them.
func parse(data []byte) ([]string, []record, error) {
	data = bytes.TrimPrefix(data, bom)
	// Go's reader splits lines on \n only, so a file saved with old Mac line
	// endings reads as one long row. A \r not followed by \n means CR endings.
	for i := bytes.IndexByte(data, '\r'); i >= 0; i = nextCR(data, i) {
		if i+1 == len(data) || data[i+1] != '\n' {
			return nil, nil, errCROnly
		}
	}
	// Bound the cells before parsing: the reader allocates a whole line's
	// cells at once, so a line of millions of commas must be refused unread.
	// Commas inside quotes count too; overcounting only errs safe.
	if n := bytes.Count(data, []byte{','}) + bytes.Count(data, []byte{'\n'}); n > maxCells {
		return nil, nil, cellsError()
	}
	r := stdcsv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // ragged rows: merge decides whether a row is usable
	// Leading spaces are trimmed, as core does, so `a, "b"` reads; this is the
	// one change to the text as written (contracts section 12.5).
	r.TrimLeadingSpace = true

	headers, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("the file is empty; it needs at least a header row")
	}
	if err != nil {
		return nil, nil, explain(err)
	}
	if err := checkUTF8(headers, 1); err != nil {
		return nil, nil, err
	}
	if blank(headers) {
		return nil, nil, errors.New("the header row (line 1) is blank; the first line must name the columns")
	}
	if len(headers) > maxColumns {
		return nil, nil, fmt.Errorf("the header row has %d columns, more than the %d a csv source reads", len(headers), maxColumns)
	}
	var out []record
	cells := 0 // what the rows will hold: every row is padded to the headers
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return headers, out, nil
		}
		if err != nil {
			return nil, nil, explain(err)
		}
		line, _ := r.FieldPos(0)
		if err := checkUTF8(rec, line); err != nil {
			return nil, nil, err
		}
		if blank(rec) {
			continue
		}
		if cells += max(len(rec), len(headers)); cells > maxCells {
			return nil, nil, cellsError()
		}
		if len(rec) > len(headers) {
			// Cells past the last header have no name. The reader's strings
			// share one line-long buffer, so copy the kept cells to free it.
			kept := make([]string, len(headers))
			for i := range kept {
				kept[i] = strings.Clone(rec[i])
			}
			rec = kept
		}
		out = append(out, record{line: line, cells: rec})
	}
}

func nextCR(data []byte, i int) int {
	j := bytes.IndexByte(data[i+1:], '\r')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

func cellsError() error {
	return fmt.Errorf("the file has more than %d cells, the most a csv source reads; split it", maxCells)
}

// explain adds the fix to a quoting error, the one parse error a person
// typing in a spreadsheet commonly makes.
func explain(err error) error {
	if errors.Is(err, stdcsv.ErrBareQuote) || errors.Is(err, stdcsv.ErrQuote) {
		return fmt.Errorf("%w; put a field holding a quote in double quotes and double the quote inside (\"Acme \"\"Inc\"\"\")", err)
	}
	return err
}

func checkUTF8(cells []string, line int) error {
	for _, c := range cells {
		if !utf8.ValidString(c) {
			return fmt.Errorf("line %d is not UTF-8; save the file as CSV UTF-8", line)
		}
	}
	return nil
}

func blank(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// toRows returns one InputRow per record. Every header gets a column, empty
// when the row is short, so a ragged row and the same row padded with empty
// cells hash alike in merge. Cells past the last header have no name and are
// dropped. When a header is written twice, the first column's value is kept.
// All rows share one Headers slice, which callers must not change.
func (s *Source) toRows(headers []string, records []record) []api.InputRow {
	rows := make([]api.InputRow, 0, len(records))
	for _, rec := range records {
		cols := make(map[string]string, len(headers))
		for i, h := range headers {
			if _, seen := cols[h]; seen {
				continue
			}
			cols[h] = cell(rec.cells, i)
		}
		rows = append(rows, api.InputRow{SourceID: s.id, Headers: headers, Columns: cols})
	}
	return rows
}

func cell(cells []string, i int) string {
	if i < len(cells) {
		return cells[i]
	}
	return ""
}

// Event-row column names after resolving a header (contracts section 2): the
// built-in aliases, plus the two event-only names.
const (
	colEvent  = "event"
	colAt     = "at"
	colEmail  = "email"
	colLinked = "linkedin_url"
	colDomain = "company.domain"
)

// resolve names an event-row header: its built-in alias field, else its
// squashed form ("event" and "at" squash to themselves). A header with no
// a-z0-9 at all (`#`, `日本`) keeps its trimmed text as written.
func resolve(aliases map[string]string, header string) string {
	sq := api.SquashHeader(header)
	if sq == "" {
		return strings.TrimSpace(header)
	}
	if f, ok := aliases[sq]; ok {
		return f
	}
	return sq
}

// attrKey is the Attrs key for an extra column: the resolved name, except that
// the company name uses the Event.Attrs name `company` (contracts section 1).
func attrKey(field string) string {
	if field == "company.name" {
		return "company"
	}
	return field
}

// toEvents returns one event per record (contracts section 5.2). A row that
// cannot be an event comes back with Kind empty and Attrs["reject"] saying why;
// the engine logs it. Reasons carry the line number, never a cell's value, so
// the log carries no email. A file with no `at` column, or no person-key
// column, fails as a whole, as core refused such files.
func (s *Source) toEvents(headers []string, records []record) ([]api.Event, error) {
	aliases := api.BuiltinAliases()
	// The first header in file order that resolves to a name owns it.
	index := map[string]int{}
	for i, h := range headers {
		name := resolve(aliases, h)
		if name == "" {
			continue
		}
		if _, seen := index[name]; !seen {
			index[name] = i
		}
	}
	if _, ok := index[colAt]; !ok {
		return nil, errors.New("an events file needs an `at` column (or visited at, visit date, last visited)")
	}
	_, hasEmail := index[colEmail]
	_, hasLinked := index[colLinked]
	_, hasDomain := index[colDomain]
	if !hasEmail && !hasLinked && !hasDomain {
		return nil, errors.New("an events file needs an email, linkedin_url or domain column")
	}
	_, hasEventCol := index[colEvent]
	received := s.now().UTC()

	events := make([]api.Event, 0, len(records))
	for _, rec := range records {
		get := func(name string) string {
			i, ok := index[name]
			if !ok {
				return ""
			}
			return strings.TrimSpace(cell(rec.cells, i))
		}
		e := api.Event{
			Email:       get(colEmail),
			LinkedInURL: get(colLinked),
			Domain:      get(colDomain),
			ReceivedAt:  received,
			Origin:      s.id,
			Attrs:       map[string]string{},
		}
		for name, i := range index {
			switch name {
			case colEvent, colAt, colEmail, colLinked, colDomain, "reject":
				// "reject" is the engine's marker; a column by that name must
				// not turn a good row into a rejected one.
				continue
			}
			if v := strings.TrimSpace(cell(rec.cells, i)); v != "" {
				e.Attrs[attrKey(name)] = v
			}
		}

		// An Apollo visitor export has no event column: each row is a visit.
		kind := visitKind(s.id)
		if hasEventCol {
			kind = strings.ToLower(get(colEvent))
		}
		at, atErr := parseAt(get(colAt))
		switch {
		case kind == "":
			e.Attrs["reject"] = fmt.Sprintf("line %d: no event kind", rec.line)
		case hasEventCol && !plainKind(kind):
			e.Attrs["reject"] = fmt.Sprintf("line %d: an event kind may use only a-z, 0-9 and _", rec.line)
		case forbidden(kind):
			e.Attrs["reject"] = fmt.Sprintf("line %d: a sent, reply, opt-out or deal kind may not come from a file", rec.line)
		case atErr != nil:
			e.Attrs["reject"] = fmt.Sprintf("line %d: %v", rec.line, atErr)
		case e.Email == "" && e.LinkedInURL == "" && e.Domain == "":
			e.Attrs["reject"] = fmt.Sprintf("line %d: no email, linkedin_url or domain", rec.line)
		default:
			e.Kind = kind
			e.At = at
		}
		events = append(events, e)
	}
	return events, nil
}

// visitKind is the kind of an Apollo visitor export's rows: visit_<source id>,
// lowercased, with any character outside a-z, 0-9 and _ made _, so it obeys
// the kind rule like a kind read from a cell.
func visitKind(sourceID string) string {
	b := []byte("visit_" + strings.ToLower(sourceID))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}

// plainKind reports a kind made only of a-z, 0-9 and _, so look-alike letters
// (`ſent`) and invisible characters cannot slip a forbidden kind past forbidden.
func plainKind(kind string) bool {
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// forbidden reports a kind only a vendor may report (contracts section 5.2):
// a file claiming a send, reply, opt-out or deal could suppress or unblock a
// person on no evidence. Case is ignored, so `Sent` is refused too.
func forbidden(kind string) bool {
	k := strings.ToLower(kind)
	switch k {
	case "sent", "unsubscribed", "reply", "optout":
		return true
	}
	return strings.HasPrefix(k, "replied") || strings.HasPrefix(k, "deal_")
}

// parseAt reads an event time: RFC 3339 (any offset, converted to UTC), or a
// bare YYYY-MM-DD read as UTC midnight.
func parseAt(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errors.New("no `at` time")
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("`at` is not RFC 3339 or YYYY-MM-DD")
}
