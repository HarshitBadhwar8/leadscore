package sheets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// Raw receiver events live in one tab per UTC month of their received time,
// "Events 2026-10" (RFC 6.6, "Monthly tabs"). Rows are only ever appended, so
// an event's sequence is its (tab, row): a cursor holds, for every tab read,
// how many data rows were read from it, written "2026-09:120,2026-10:7". A tab
// the cursor does not name is read from its first row, so an append during
// the grace hour to last month's tab is still read. The `seq` column is left
// empty on Sheets: the position is the sequence.

var monthForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)

// EventsTab is the tab holding events received in t's UTC month.
func EventsTab(t time.Time) string { return model.EventsPrefix + t.UTC().Format("2006-01") }

// eventsMonth returns the month of an events tab name.
func eventsMonth(name string) (string, bool) {
	m, ok := strings.CutPrefix(name, model.EventsPrefix)
	if !ok || !monthForm.MatchString(m) {
		return "", false
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		return "", false
	}
	return m, true
}

type positions map[string]int // month -> data rows read

func parseCursor(c api.Cursor) (positions, error) {
	pos := positions{}
	if c == "" {
		return pos, nil
	}
	for _, part := range strings.Split(string(c), ",") {
		m, n, ok := strings.Cut(part, ":")
		count, err := strconv.Atoi(n)
		if _, isTab := eventsMonth(model.EventsPrefix + m); !ok || !isTab || err != nil || count < 0 {
			return nil, fmt.Errorf("event cursor %q is not a Sheets events cursor", c)
		}
		pos[m] = count
	}
	return pos, nil
}

func (p positions) encode() api.Cursor {
	months := make([]string, 0, len(p))
	for m, n := range p {
		if n > 0 {
			months = append(months, m)
		}
	}
	sort.Strings(months)
	parts := make([]string, len(months))
	for i, m := range months {
		parts[i] = m + ":" + strconv.Itoa(p[m])
	}
	return api.Cursor(strings.Join(parts, ","))
}

// eventMonths lists the spreadsheet's events tabs by month, oldest first.
func eventMonths(book *sheetsapi.Spreadsheet) []string {
	var out []string
	for _, sh := range book.Sheets {
		if sh.Properties == nil {
			continue
		}
		if m, ok := eventsMonth(sh.Properties.Title); ok {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// eventColumns are the Events columns in order (contracts section 4).
var eventColumns = func() []string {
	d, _ := model.Def(model.EventsPrefix + "x")
	return d.Columns
}()

// AppendEvents stores a batch in one batchUpdate, so all-or-nothing, each
// event in the tab of its received time's month, in order. It creates a
// missing month's tab in the same request, protected like the existing
// Events tabs. It retries Google's "slow down" until ctx is done; when another
// writer created the same tab first, it reads the spreadsheet again and
// appends.
func (s *Store) AppendEvents(ctx context.Context, events []api.RawEvent) error {
	if len(events) == 0 {
		return nil
	}
	byMonth := map[string][]*sheetsapi.RowData{}
	for i, e := range events {
		if e.ReceivedAt.IsZero() {
			return fmt.Errorf("appending events: event %d has no received time", i)
		}
		tab := EventsTab(e.ReceivedAt)
		byMonth[tab] = append(byMonth[tab], rowData(eventColumns, api.Row{
			"received_at": model.FormatTime(e.ReceivedAt), "kind": e.Kind, "body": string(e.Body),
		}))
	}
	tabs := make([]string, 0, len(byMonth))
	for t := range byMonth {
		tabs = append(tabs, t)
	}
	sort.Strings(tabs)
	for {
		book, err := s.metaTries(ctx, 0)
		if err != nil {
			return fmt.Errorf("appending events: %w", err)
		}
		ids := sheetIDs(book)
		var reqs []*sheetsapi.Request
		for _, name := range tabs {
			id := int64(0)
			if sh := tabOf(book, name); sh != nil {
				id = sh.Properties.SheetId
			} else {
				id = newSheetID(ids)
				reqs = append(reqs, addTab(id, name, int64(len(eventColumns)), 0), headerCells(id, 0, eventColumns))
				if pr := protectionFor(book, name, id); pr != nil {
					reqs = append(reqs, pr)
				}
			}
			reqs = append(reqs, &sheetsapi.Request{AppendCells: &sheetsapi.AppendCellsRequest{
				SheetId: id, Rows: byMonth[name], Fields: "userEnteredValue"}})
		}
		err = s.batchUpdate(ctx, reqs, 0)
		if alreadyExists(err) {
			continue // a second writer created the tab first: append to it
		}
		if err != nil {
			return fmt.Errorf("appending events: %w", err)
		}
		return nil
	}
}

func alreadyExists(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest && strings.Contains(e.Message, "already exists")
}

// metaTries is meta with a chosen retry bound (0: until ctx is done).
func (s *Store) metaTries(ctx context.Context, tries int) (*sheetsapi.Spreadsheet, error) {
	var book *sheetsapi.Spreadsheet
	err := retry(ctx, tries, func() error {
		var err error
		book, err = s.svc.Sheets.Spreadsheets.Get(s.id).
			Fields("spreadsheetId,properties(title,autoRecalc,timeZone),sheets(properties(sheetId,title,index,gridProperties),protectedRanges)").
			Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("opening spreadsheet %s: %w", s.id, err)
	}
	return book, nil
}

// ReadEvents returns the events after cursor, tab by tab (oldest month first)
// and row by row, and the cursor after the last one. Each event's Seq is the
// cursor just after it. It returns ErrEventsShrank when a tab the cursor names
// is missing, or no longer holds the last row the cursor says was read.
func (s *Store) ReadEvents(ctx context.Context, cursor api.Cursor) ([]api.RawEvent, api.Cursor, error) {
	pos, err := parseCursor(cursor)
	if err != nil {
		return nil, cursor, err
	}
	book, err := s.meta(ctx)
	if err != nil {
		return nil, cursor, err
	}
	months := eventMonths(book)
	present := map[string]bool{}
	for _, m := range months {
		present[m] = true
	}
	for m, n := range pos {
		if n > 0 && !present[m] {
			return nil, cursor, fmt.Errorf("%w: tab %q is gone", api.ErrEventsShrank, model.EventsPrefix+m)
		}
	}
	// Read each tab from the last row already read (to check it is still
	// there) or, for a tab not read yet, from its first data row.
	ranges := make([]string, len(months))
	for i, m := range months {
		start := pos[m] + 1 // sheet row of the last data row read; the header is row 1
		if pos[m] == 0 {
			start = 2
		}
		ranges[i] = fmt.Sprintf("%s!A%d:%s", QuoteTab(model.EventsPrefix+m), start, colName(len(eventColumns)-1))
	}
	vrs, err := s.batchGet(ctx, ranges, func(string) string { return "UNFORMATTED_VALUE" })
	if err != nil {
		return nil, cursor, err
	}
	cur := positions{}
	for m, n := range pos {
		cur[m] = n
	}
	var out []api.RawEvent
	for i, m := range months {
		var values [][]any
		if vrs != nil {
			values = vrs[i].Values
		}
		read := pos[m]
		if read > 0 {
			if len(values) == 0 || blankCells(values[0]) {
				return nil, cursor, fmt.Errorf("%w: tab %q holds fewer than the %d rows already read",
					api.ErrEventsShrank, model.EventsPrefix+m, read)
			}
			values = values[1:]
		}
		for j, cells := range values {
			cur[m] = read + j + 1
			if blankCells(cells) {
				continue
			}
			get := func(c int) string {
				if c < len(cells) {
					return cellText(cells[c])
				}
				return ""
			}
			at, err := model.ParseTime(get(1))
			if err != nil {
				return nil, cursor, fmt.Errorf("%s row %d received_at: %w", model.EventsPrefix+m, cur[m]+1, err)
			}
			out = append(out, api.RawEvent{Seq: cur.encode(), Kind: get(2), ReceivedAt: at, Body: []byte(get(3))})
		}
	}
	return out, cur.encode(), nil
}

func blankCells(cells []any) bool {
	for _, c := range cells {
		if cellText(c) != "" {
			return false
		}
	}
	return true
}

// graceAfterMonth is how long after its month ends a tab may still be
// appended to (RFC 6.6): a receiver's clock may lag.
const graceAfterMonth = time.Hour

// DeleteProcessed deletes whole monthly tabs: a tab goes when every row in it
// is at or below committed, its month ended before olderThan (so every event
// in it is older), and its month plus the one-hour grace has passed, so no
// receiver can still append to it. It returns committed without the deleted
// tabs; the engine saves that cursor. Rows are never deleted one by one: that
// would move the positions cursors hold.
func (s *Store) DeleteProcessed(ctx context.Context, committed api.Cursor, olderThan time.Time) (api.Cursor, error) {
	pos, err := parseCursor(committed)
	if err != nil {
		return committed, err
	}
	book, err := s.meta(ctx)
	if err != nil {
		return committed, err
	}
	now := s.now()
	var candidates []string
	for _, m := range eventMonths(book) {
		if pos[m] == 0 {
			continue
		}
		start, _ := time.Parse("2006-01", m)
		end := start.AddDate(0, 1, 0)
		if now.Before(end.Add(graceAfterMonth)) || end.After(olderThan) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return committed, nil
	}
	ranges := make([]string, len(candidates))
	for i, m := range candidates {
		ranges[i] = QuoteTab(model.EventsPrefix+m) + "!B2:B"
	}
	vrs, err := s.batchGet(ctx, ranges, func(string) string { return "UNFORMATTED_VALUE" })
	if err != nil {
		return committed, err
	}
	var reqs []*sheetsapi.Request
	var gone []string
	for i, m := range candidates {
		if len(vrs[i].Values) != pos[m] {
			continue // rows past the committed cursor (or a shrunk tab): keep it
		}
		sh := tabOf(book, model.EventsPrefix+m)
		reqs = append(reqs, &sheetsapi.Request{DeleteSheet: &sheetsapi.DeleteSheetRequest{SheetId: sh.Properties.SheetId}})
		gone = append(gone, m)
	}
	if len(reqs) == 0 {
		return committed, nil
	}
	if err := s.batchUpdate(ctx, reqs, readTries); err != nil {
		return committed, fmt.Errorf("deleting processed events: %w", err)
	}
	for _, m := range gone {
		delete(pos, m)
	}
	return pos.encode(), nil
}
