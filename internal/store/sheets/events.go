package sheets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
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
// an event's sequence is its (tab, row). Each row's `seq` cell holds a random
// id, so a read can tell that rows moved. A cursor holds, for every tab read,
// how many data rows were read from it and the id of the last one, written
// "2026-09:120:3f9a0c1b2d4e,2026-10:7:9b8a7c6d5e4f". A tab the cursor does not
// name is read from its first row, so an append during the grace hour to last
// month's tab is still read.

// deletedMetadataKey marks, in the spreadsheet's developer metadata, a month
// whose tab DeleteProcessed deleted after every row in it was read. It is
// written in the same batchUpdate as the delete, so a run that crashed before
// saving the shorter cursor still reads on: a cursor naming a recorded month
// skips it instead of failing.
const deletedMetadataKey = "leadscore.events_deleted"

var monthForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)

// EventsTab is the tab holding events received in t's UTC month.
func EventsTab(t time.Time) string { return model.EventsPrefix + t.UTC().Format("2006-01") }

// eventsMonth returns the month of an events tab name ("Events 2026-10";
// case is ignored, as Sheets ignores it in tab names).
func eventsMonth(name string) (string, bool) {
	if len(name) <= len(model.EventsPrefix) || !strings.EqualFold(name[:len(model.EventsPrefix)], model.EventsPrefix) {
		return "", false
	}
	m := name[len(model.EventsPrefix):]
	if !monthForm.MatchString(m) {
		return "", false
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		return "", false
	}
	return m, true
}

// isEventsTab reports a monthly events tab name.
func isEventsTab(name string) bool {
	_, ok := eventsMonth(name)
	return ok
}

// mark is how far a cursor read one tab: rows read and the last row's id.
type mark struct {
	n  int
	id string
}

type positions map[string]mark // month -> mark

func parseCursor(c api.Cursor) (positions, error) {
	pos := positions{}
	if c == "" {
		return pos, nil
	}
	for _, part := range strings.Split(string(c), ",") {
		f := strings.Split(part, ":")
		bad := len(f) < 2 || len(f) > 3
		var n int
		if !bad {
			var err error
			n, err = strconv.Atoi(f[1])
			_, isTab := eventsMonth(model.EventsPrefix + f[0])
			bad = err != nil || n < 0 || !isTab
		}
		if bad {
			return nil, fmt.Errorf("event cursor %q is not a Sheets events cursor", c)
		}
		mk := mark{n: n}
		if len(f) == 3 {
			mk.id = f[2]
		}
		pos[f[0]] = mk
	}
	return pos, nil
}

func (p positions) encode() api.Cursor {
	var parts []string
	for _, m := range slices.Sorted(maps.Keys(p)) {
		if mk := p[m]; mk.n > 0 {
			parts = append(parts, m+":"+strconv.Itoa(mk.n)+":"+mk.id)
		}
	}
	return api.Cursor(strings.Join(parts, ","))
}

// eventTabs maps each events tab's month to the tab.
func eventTabs(book *sheetsapi.Spreadsheet) map[string]*sheetsapi.Sheet {
	out := map[string]*sheetsapi.Sheet{}
	for _, sh := range book.Sheets {
		if sh.Properties == nil {
			continue
		}
		if m, ok := eventsMonth(sh.Properties.Title); ok {
			out[m] = sh
		}
	}
	return out
}

func gridRows(sh *sheetsapi.Sheet) int64 {
	if gp := sh.Properties.GridProperties; gp != nil {
		return gp.RowCount
	}
	return 0
}

// deletedMonths are the months DeleteProcessed recorded as deleted.
func deletedMonths(book *sheetsapi.Spreadsheet) map[string]bool {
	out := map[string]bool{}
	for _, md := range book.DeveloperMetadata {
		if md.MetadataKey == deletedMetadataKey {
			out[md.MetadataValue] = true
		}
	}
	return out
}

// eventColumns are the Events columns in order (contracts section 4).
var eventColumns = func() []string {
	d, _ := model.Def(model.EventsPrefix + "x")
	return d.Columns
}()

// rowID is a fresh random id for an appended event row.
func rowID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AppendEvents stores a batch in one batchUpdate, so all-or-nothing, each
// event in the tab of its received time's month, in order, each row with a
// fresh id in `seq`. It creates a missing month's tab (hidden) in the same
// request, protected like the existing Events tabs. It retries Google's "slow
// down" until ctx is done; when another writer created the same tab first, it
// reads the spreadsheet again and appends.
func (s *Store) AppendEvents(ctx context.Context, events []api.RawEvent) error {
	if len(events) == 0 {
		return nil
	}
	byMonth := map[string][]*sheetsapi.RowData{}
	for i, e := range events {
		if e.ReceivedAt.IsZero() {
			return fmt.Errorf("appending events: event %d has no received time", i)
		}
		if n := len([]rune(string(e.Body))); n > MaxCellChars {
			return fmt.Errorf("appending events: event %d's body has %d characters, more than a Sheets cell holds (%d)", i, n, MaxCellChars)
		}
		tab := EventsTab(e.ReceivedAt)
		byMonth[tab] = append(byMonth[tab], rowData(eventColumns, api.Row{
			"seq": rowID(), "received_at": model.FormatTime(e.ReceivedAt), "kind": e.Kind, "body": string(e.Body),
		}))
	}
	for {
		book, err := s.metaTries(ctx, 0)
		if err != nil {
			return fmt.Errorf("appending events: %w", err)
		}
		ids := sheetIDs(book)
		var reqs []*sheetsapi.Request
		for _, name := range slices.Sorted(maps.Keys(byMonth)) {
			id := int64(0)
			if sh := tabOf(book, name); sh != nil {
				id = sh.Properties.SheetId
			} else {
				id = newSheetID(ids)
				reqs = append(reqs, addTab(id, name, int64(len(eventColumns)), true), headerCells(id, 0, eventColumns))
				pr, err := protectionFor(book, name, id, func() (string, error) { return s.callerEmail(ctx) })
				if err != nil {
					return fmt.Errorf("appending events: %w", err)
				}
				if pr != nil {
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

// callerEmail is the signed-in account, as Drive reports it. Only a new
// Events tab with no protected sibling needs it.
func (s *Store) callerEmail(ctx context.Context) (string, error) {
	about, err := s.svc.Drive.About.Get().Fields("user(emailAddress)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("asking Drive which account is signed in: %w", err)
	}
	if about.User == nil || about.User.EmailAddress == "" {
		return "", errors.New("Drive did not say which account is signed in")
	}
	return about.User.EmailAddress, nil
}

func alreadyExists(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest && strings.Contains(e.Message, "already exists")
}

// metaTries reads the spreadsheet's tabs, protection and developer metadata,
// with a chosen retry bound (0: until ctx is done).
func (s *Store) metaTries(ctx context.Context, tries int) (*sheetsapi.Spreadsheet, error) {
	var book *sheetsapi.Spreadsheet
	err := retry(ctx, tries, func() error {
		var err error
		book, err = s.svc.Sheets.Spreadsheets.Get(s.id).
			Fields("spreadsheetId,properties(title,autoRecalc,timeZone),developerMetadata(metadataKey,metadataValue)," +
				"sheets(properties(sheetId,title,index,hidden,gridProperties),protectedRanges)").
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
// is missing (unless DeleteProcessed recorded deleting it), holds fewer rows
// than the cursor read, or no longer has the last row read at its place
// (rows were sorted, inserted or deleted).
func (s *Store) ReadEvents(ctx context.Context, cursor api.Cursor) ([]api.RawEvent, api.Cursor, error) {
	pos, err := parseCursor(cursor)
	if err != nil {
		return nil, cursor, err
	}
	book, err := s.meta(ctx)
	if err != nil {
		return nil, cursor, err
	}
	tabs := eventTabs(book)
	deleted := deletedMonths(book)
	// recreated: months deleted and then made again by a lagging receiver.
	// The cursor's mark may be for the deleted tab or for the new one; the
	// new tab is read whole, and the mark is kept only if its row is there.
	recreated := map[string]bool{}
	for m, mk := range pos {
		sh := tabs[m]
		switch {
		case mk.n == 0:
		case sh == nil && deleted[m]:
			delete(pos, m) // read whole, then deleted: nothing more to read there
		case deleted[m]:
			recreated[m] = true
		case sh == nil:
			return nil, cursor, fmt.Errorf("%w: tab %q is gone", api.ErrEventsShrank, model.EventsPrefix+m)
		case gridRows(sh)-1 < int64(mk.n):
			return nil, cursor, fmt.Errorf("%w: tab %q holds fewer than the %d rows already read",
				api.ErrEventsShrank, sh.Properties.Title, mk.n)
		}
	}
	months := slices.Sorted(maps.Keys(tabs))
	// Read each tab from the last row already read (to check it is still
	// there) or, for a tab not read yet, from its first data row.
	ranges := make([]string, len(months))
	for i, m := range months {
		start := pos[m].n + 1 // sheet row of the last data row read; the header is row 1
		if pos[m].n == 0 || recreated[m] {
			start = 2
		}
		ranges[i] = fmt.Sprintf("%s!A%d:%s", QuoteTab(tabs[m].Properties.Title), start, colName(len(eventColumns)-1))
	}
	vrs, err := s.batchGet(ctx, ranges, func(string) string { return "UNFORMATTED_VALUE" })
	if err != nil {
		return nil, cursor, err
	}
	cur := maps.Clone(pos)
	var out []api.RawEvent
	for i, m := range months {
		values := vrs[i].Values
		title := tabs[m].Properties.Title
		read := pos[m]
		if recreated[m] {
			// Read whole: skip up to the mark only if the mark is this tab's.
			if read.n <= len(values) && cellAt(values[read.n-1], 0) == read.id {
				values = values[read.n-1:]
			} else {
				read = mark{}
			}
		}
		if read.n > 0 {
			if len(values) == 0 || blankCells(values[0]) || cellAt(values[0], 0) != read.id {
				return nil, cursor, fmt.Errorf("%w: rows of tab %q moved (sorted, inserted or deleted) under the cursor",
					api.ErrEventsShrank, title)
			}
			values = values[1:]
		}
		for j, cells := range values {
			cur[m] = mark{n: read.n + j + 1, id: cellAt(cells, 0)}
			if blankCells(cells) {
				continue
			}
			at, err := model.ParseTime(cellAt(cells, 1))
			if err != nil {
				return nil, cursor, fmt.Errorf("%s row %d received_at: %w", title, cur[m].n+1, err)
			}
			out = append(out, api.RawEvent{Seq: cur.encode(), Kind: cellAt(cells, 2), ReceivedAt: at, Body: []byte(cellAt(cells, 3))})
		}
	}
	return out, cur.encode(), nil
}

func cellAt(cells []any, c int) string {
	if c < len(cells) {
		return cellText(cells[c])
	}
	return ""
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
// is at or below committed (and its last row is the one committed read), its
// month ended before olderThan (so every event in it is older), and its month
// plus the one-hour grace has passed, so no receiver can still append to it.
// The newest Events tab is never deleted, so a new month's tab always has a
// protected sibling to copy. Each deleted month is recorded in the same
// batchUpdate (see deletedMetadataKey). It returns committed without the
// deleted tabs; the engine saves that cursor. Rows are never deleted one by
// one: that would move the positions cursors hold.
func (s *Store) DeleteProcessed(ctx context.Context, committed api.Cursor, olderThan time.Time) (api.Cursor, error) {
	pos, err := parseCursor(committed)
	if err != nil {
		return committed, err
	}
	book, err := s.meta(ctx)
	if err != nil {
		return committed, err
	}
	tabs := eventTabs(book)
	months := slices.Sorted(maps.Keys(tabs))
	now := s.now()
	var candidates []string
	for i, m := range months {
		mk := pos[m]
		if mk.n == 0 || i == len(months)-1 || gridRows(tabs[m])-1 < int64(mk.n) {
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
		ranges[i] = QuoteTab(tabs[m].Properties.Title) + "!A2:B"
	}
	vrs, err := s.batchGet(ctx, ranges, func(string) string { return "UNFORMATTED_VALUE" })
	if err != nil {
		return committed, err
	}
	var reqs []*sheetsapi.Request
	var gone []string
	for i, m := range candidates {
		rows, mk := vrs[i].Values, pos[m]
		if len(rows) != mk.n || cellAt(rows[mk.n-1], 0) != mk.id {
			continue // rows past the committed cursor, or moved rows: keep it
		}
		reqs = append(reqs,
			&sheetsapi.Request{DeleteSheet: &sheetsapi.DeleteSheetRequest{SheetId: tabs[m].Properties.SheetId}},
			&sheetsapi.Request{CreateDeveloperMetadata: &sheetsapi.CreateDeveloperMetadataRequest{
				DeveloperMetadata: &sheetsapi.DeveloperMetadata{MetadataKey: deletedMetadataKey, MetadataValue: m,
					Location: &sheetsapi.DeveloperMetadataLocation{Spreadsheet: true}, Visibility: "DOCUMENT"}}})
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
