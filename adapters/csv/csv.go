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

// Fetch reads the whole file. A plain source returns rows; an events source
// returns events. Any line the CSV reader cannot parse, or text that is not
// UTF-8, fails the whole fetch with the line number, so a bad file is fixed
// rather than half-read.
func (s *Source) Fetch(ctx context.Context, _ api.Cursor) ([]api.InputRow, []api.Event, api.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("csv source %q: %w", s.id, err)
	}
	headers, records, err := parse(data)
	if err != nil {
		return nil, nil, "", fmt.Errorf("csv source %q: %s: %w", s.id, s.path, err)
	}
	if s.events {
		return nil, s.toEvents(headers, records), "", nil
	}
	return s.toRows(headers, records), nil, "", nil
}

// record is one data line with its line number in the file, for reject reasons.
type record struct {
	line  int
	cells []string
}

var bom = []byte("\xEF\xBB\xBF")

// parse splits the file into headers and data records. Lines whose cells are
// all empty are dropped: a spreadsheet export often ends with them.
func parse(data []byte) ([]string, []record, error) {
	data = bytes.TrimPrefix(data, bom)
	r := stdcsv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // ragged rows: merge decides whether a row is usable
	r.TrimLeadingSpace = true

	headers, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("the file is empty; it needs at least a header row")
	}
	if err != nil {
		return nil, nil, err
	}
	if err := checkUTF8(headers, 1); err != nil {
		return nil, nil, err
	}
	var out []record
	for {
		cells, err := r.Read()
		if errors.Is(err, io.EOF) {
			return headers, out, nil
		}
		if err != nil {
			return nil, nil, err
		}
		line, _ := r.FieldPos(0)
		if err := checkUTF8(cells, line); err != nil {
			return nil, nil, err
		}
		if blank(cells) {
			continue
		}
		out = append(out, record{line: line, cells: cells})
	}
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
		rows = append(rows, api.InputRow{
			SourceID: s.id,
			Headers:  append([]string(nil), headers...),
			Columns:  cols,
		})
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
// squashed form ("event" and "at" squash to themselves).
func resolve(aliases map[string]string, header string) string {
	sq := api.SquashHeader(header)
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
// the log carries no email.
func (s *Source) toEvents(headers []string, records []record) []api.Event {
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
		kind := "visit_" + s.id
		if hasEventCol {
			kind = get(colEvent)
		}
		at, atErr := parseAt(get(colAt))
		switch {
		case kind == "":
			e.Attrs["reject"] = fmt.Sprintf("line %d: no event kind", rec.line)
		case forbidden(kind):
			e.Attrs["reject"] = fmt.Sprintf("line %d: event kind %q may not come from a file", rec.line, kind)
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
	return events
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
