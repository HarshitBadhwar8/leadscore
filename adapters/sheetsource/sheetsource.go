// Package sheetsource is the Google Sheet tab source (contracts section 3,
// `sources[]`): it reads people-owned tabs of the team's spreadsheet as they
// appear on screen (FORMATTED_VALUE), headers in order as written, one
// InputRow per non-blank row. Like the CSV source it only parses; merge
// applies aliases, checks emails and computes row ids.
//
// A source marked `events: true` returns one event per row instead, by the
// same rules as a CSV events file (contracts section 5.2).
//
// The engine copies the spreadsheet id (store.spreadsheet, or
// store.view_spreadsheet on SQLite) and store.credentials into the entry as
// `spreadsheet` and `credentials`, so a team writes only the tabs:
//
//	sources:
//	  - { id: leads, type: sheetsource, tabs: [Leads] }
//	  - { id: offline-events, type: sheetsource, tabs: [Events import], events: true }
package sheetsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/adapters/csv"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

func init() { api.RegisterSource("sheetsource", New) }

// Source reads tabs of one spreadsheet. It is a snapshot source: every Fetch
// returns every row and ignores the cursor.
type Source struct {
	id     string
	sheet  string
	tabs   []string
	events bool
	svc    *sheets.Services
	now    func() time.Time // stamps Event.ReceivedAt
}

// New builds a source from its sources[] entry: `id`, `tabs` and
// `spreadsheet` are required; `events`, `credentials`, and the test keys
// `base_url` and `_http_client` are optional.
func New(cfg api.Config) (api.Source, error) {
	id, _ := cfg["id"].(string)
	if id == "" {
		return nil, errors.New("sheetsource: `id` is required")
	}
	var tabs []string
	switch v := cfg["tabs"].(type) {
	case []string:
		tabs = append(tabs, v...)
	case []any:
		for _, t := range v {
			s, ok := t.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("sheetsource %q: every entry in `tabs` must be a tab name", id)
			}
			tabs = append(tabs, s)
		}
	case nil:
	default:
		return nil, fmt.Errorf("sheetsource %q: `tabs` must be a list of tab names", id)
	}
	if len(tabs) == 0 {
		return nil, fmt.Errorf("sheetsource %q: `tabs` is required, for example tabs: [Leads]", id)
	}
	sheet, _ := cfg["spreadsheet"].(string)
	if sheet == "" {
		return nil, fmt.Errorf("sheetsource %q: no spreadsheet; it comes from store.spreadsheet "+
			"(or store.view_spreadsheet on a SQLite store), so set that", id)
	}
	events := false
	if v, has := cfg["events"]; has && v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("sheetsource %q: `events` must be true or false", id)
		}
		events = b
	}
	svc, err := sheets.Connect(context.Background(), cfg, sheets.ReadOnlyScopes...)
	if err != nil {
		return nil, fmt.Errorf("sheetsource %q: %w", id, err)
	}
	return &Source{id: id, sheet: sheet, tabs: tabs, events: events, svc: svc, now: time.Now}, nil
}

func (s *Source) ID() string { return s.id }

// Fetch reads every tab in one call. A missing tab, or a tab whose header row
// is blank, fails the whole fetch rather than reading part of the source.
func (s *Source) Fetch(ctx context.Context, _ api.Cursor) ([]api.InputRow, []api.Event, api.Cursor, error) {
	ranges := make([]string, len(s.tabs))
	for i, t := range s.tabs {
		ranges[i] = sheets.QuoteTab(t)
	}
	resp, err := s.svc.Sheets.Spreadsheets.Values.BatchGet(s.sheet).Ranges(ranges...).
		ValueRenderOption("FORMATTED_VALUE").Context(ctx).Do()
	if err != nil {
		if strings.Contains(err.Error(), "Unable to parse range") {
			return nil, nil, "", fmt.Errorf("sheetsource %q: a tab in %q is missing from the spreadsheet: %w", s.id, s.tabs, err)
		}
		return nil, nil, "", fmt.Errorf("sheetsource %q: reading the spreadsheet: %w", s.id, err)
	}
	if len(resp.ValueRanges) != len(s.tabs) {
		return nil, nil, "", fmt.Errorf("sheetsource %q: asked for %d tabs, got %d", s.id, len(s.tabs), len(resp.ValueRanges))
	}
	var rows []api.InputRow
	var events []api.Event
	received := s.now()
	for i, vr := range resp.ValueRanges {
		tab := s.tabs[i]
		headers, records, err := parse(vr)
		if err != nil {
			return nil, nil, "", fmt.Errorf("sheetsource %q: tab %q: %w", s.id, tab, err)
		}
		if s.events {
			evs, err := csv.ParseEvents(s.id, headers, records, received)
			if err != nil {
				return nil, nil, "", fmt.Errorf("sheetsource %q: tab %q: %w", s.id, tab, err)
			}
			events = append(events, evs...)
			continue
		}
		rows = append(rows, csv.Rows(s.id, headers, records)...)
	}
	return rows, events, "", nil
}

// parse splits a tab into its header (as written) and its non-blank rows,
// each with its sheet row number. Cells past the last header are dropped.
func parse(vr *sheetsapi.ValueRange) ([]string, []csv.EventRecord, error) {
	if len(vr.Values) == 0 {
		return nil, nil, errors.New("the tab is empty; its first row must name the columns")
	}
	headers := make([]string, len(vr.Values[0]))
	blankHeader := true
	for i, v := range vr.Values[0] {
		headers[i] = text(v)
		if strings.TrimSpace(headers[i]) != "" {
			blankHeader = false
		}
	}
	if blankHeader {
		return nil, nil, errors.New("the header row (row 1) is blank; the first row must name the columns")
	}
	var out []csv.EventRecord
	for i, row := range vr.Values[1:] {
		cells := make([]string, 0, min(len(row), len(headers)))
		blank := true
		for j, v := range row {
			if j >= len(headers) {
				break
			}
			t := text(v)
			if strings.TrimSpace(t) != "" {
				blank = false
			}
			cells = append(cells, t)
		}
		if !blank {
			out = append(out, csv.EventRecord{Line: i + 2, Cells: cells})
		}
	}
	return headers, out, nil
}

// text is a formatted cell as shown; FORMATTED_VALUE sends text, but a
// number or boolean is written plainly just in case.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}
