// Package sheets is the built-in Google Sheets store: a Backend and EventLog
// over one spreadsheet, one tab per table, and a run lease held as a file in a
// Cloud Storage bucket.
//
//   - Every Commit is one spreadsheets.batchUpdate, which Sheets applies
//     all-or-nothing. Rows are changed in place (rewritten tabs keep their
//     sheet ids, so protection, filters and team formulas survive); growing
//     tables are appended to, never rewritten.
//   - A tab is created by the first write that names it, at exactly its
//     column count with a frozen header row, because Sheets counts every grid
//     cell toward its 10-million-cell cap. Health is the one exception: its
//     cell H1 holds the staleness formula, rewritten by every commit that
//     writes Health.
//   - Tool tabs are written as raw text and read unformatted, so a value that
//     starts with "=" stays text; people-owned tabs (Overrides, Companies,
//     Leads) are read formatted.
//   - Raw receiver events go to one tab per UTC month of their received time
//     ("Events 2026-10"). An event's sequence is its (tab, row); a cursor
//     holds a position for every tab.
//
// With `base_url` set in its block (tests), every Google client points there
// and skips authentication; `_http_client` supplies the HTTP client.
package sheets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sheetsapi "google.golang.org/api/sheets/v4"
	"google.golang.org/api/storage/v1"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/vendorhttp"
)

func init() {
	api.RegisterBackend("sheets", func(cfg api.Config) (api.Backend, api.EventLog, error) {
		s, err := Open(context.Background(), cfg)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	})
}

// Scopes are the Google sign-in scopes the store and its setup use: Sheets,
// Drive (sharing and the sheet-access check) and Cloud Storage (the lease).
var Scopes = []string{
	sheetsapi.SpreadsheetsScope,
	drive.DriveScope,
	storage.DevstorageReadWriteScope,
}

// Services are the Google API clients for one install.
type Services struct {
	Sheets  *sheetsapi.Service
	Drive   *drive.Service
	Storage *storage.Service
}

// ReadOnlyScopes are what a reader of the team's tabs needs (sheetsource).
var ReadOnlyScopes = []string{sheetsapi.SpreadsheetsReadonlyScope}

// Connect builds the clients from an adapter block (`store`, or a Sheet-tab
// source's entry), signing in with scopes (Scopes when none are given). With
// `base_url` set, every client points there and skips authentication; only
// tests set it, together with `_http_client` (RunWith adds both), so a
// `base_url` without a client, as a file could carry, is refused: it would
// send the team's data to whatever address the file names. Otherwise
// `_http_client`, when set, is an already-authenticated client (setup signs
// in as the person); else the store signs in with `credentials` (a
// service-account key file) or, when that is empty, Google's standard
// credential loading.
func Connect(ctx context.Context, cfg api.Config, scopes ...string) (*Services, error) {
	if len(scopes) == 0 {
		scopes = Scopes
	}
	base, client, err := vendorhttp.Overrides(cfg)
	if err != nil {
		return nil, err
	}
	var common []option.ClientOption
	endpoint := func(_ string) []option.ClientOption { return nil }
	switch {
	case base != "":
		common = append(common, option.WithHTTPClient(client))
		endpoint = func(path string) []option.ClientOption {
			return []option.ClientOption{option.WithEndpoint(base + path)}
		}
	case client != nil:
		common = append(common, option.WithHTTPClient(client))
	default:
		if cred, _ := cfg["credentials"].(string); cred != "" {
			common = append(common, option.WithAuthCredentialsFile(option.ServiceAccount, cred))
		}
		common = append(common, option.WithScopes(scopes...))
	}
	sh, err := sheetsapi.NewService(ctx, append(common, endpoint("/")...)...)
	if err != nil {
		return nil, fmt.Errorf("signing in to Google Sheets: %w", err)
	}
	dr, err := drive.NewService(ctx, append(common, endpoint("/drive/v3/")...)...)
	if err != nil {
		return nil, fmt.Errorf("signing in to Google Drive: %w", err)
	}
	st, err := storage.NewService(ctx, append(common, endpoint("/storage/v1/")...)...)
	if err != nil {
		return nil, fmt.Errorf("signing in to Cloud Storage: %w", err)
	}
	return &Services{Sheets: sh, Drive: dr, Storage: st}, nil
}

// Store is a Sheets Backend, EventLog and LeaseInspector.
type Store struct {
	svc    *Services
	id     string // the spreadsheet id
	bucket string // the lease bucket; empty means no lease can be taken
	now    func() time.Time
	mu     sync.Mutex // one Commit at a time from this process
}

var (
	_ api.Backend        = (*Store)(nil)
	_ api.EventLog       = (*Store)(nil)
	_ api.LeaseInspector = (*Store)(nil)
)

// Open builds a store from the `store` block: `spreadsheet` is required;
// `lease_bucket` is needed to take the lease.
func Open(ctx context.Context, cfg api.Config) (*Store, error) {
	id, _ := cfg["spreadsheet"].(string)
	if id == "" {
		return nil, errors.New("sheets store: `store.spreadsheet` is required (run `leadscore setup sheet` to create one)")
	}
	svc, err := Connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("sheets store: %w", err)
	}
	bucket, _ := cfg["lease_bucket"].(string)
	return New(svc, id, bucket), nil
}

// New returns a store over an existing spreadsheet.
func New(svc *Services, spreadsheet, leaseBucket string) *Store {
	return &Store{svc: svc, id: spreadsheet, bucket: leaseBucket, now: time.Now}
}

// SpreadsheetID returns the spreadsheet the store writes.
func (s *Store) SpreadsheetID() string { return s.id }

// Services returns the store's Google clients, for the doctor checks.
func (s *Store) Services() *Services { return s.svc }

// peopleOwned tabs are typed by people, so they are read as people see them.
func peopleOwned(name string) bool {
	switch name {
	case model.TableOverrides, model.TableCompanies, "Leads":
		return true
	}
	return false
}

func renderOption(name string) string {
	if peopleOwned(name) {
		return "FORMATTED_VALUE"
	}
	return "UNFORMATTED_VALUE"
}

// QuoteTab quotes a tab name for an A1 range: 'Seen events', with any
// apostrophe doubled.
func QuoteTab(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

// ReadTable returns every row of a tab, skipping blank rows; a missing tab
// returns no rows and no error. Columns are the named header cells.
func (s *Store) ReadTable(ctx context.Context, name string) ([]api.Row, error) {
	var vr *sheetsapi.ValueRange
	err := retry(ctx, readTries, func() error {
		var err error
		vr, err = s.svc.Sheets.Spreadsheets.Values.Get(s.id, QuoteTab(name)).
			ValueRenderOption(renderOption(name)).Context(ctx).Do()
		return err
	})
	if missingRange(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	header := headerOf(name, vr.Values)
	var out []api.Row
	for _, r := range dataRows(header, vr.Values) {
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// healthFormulaColumn is the zero-based column of Health!H1, which holds the
// staleness formula, not a column of the table.
const healthFormulaColumn = 7

// headerOf reads the header row by position: a column's name, or "" for a
// column with no name (its cells are not part of the table). Trailing empty
// cells are dropped; for Health the formula cell and anything after it too.
func headerOf(name string, values [][]any) []string {
	if len(values) == 0 {
		return nil
	}
	var h []string
	for i, v := range values[0] {
		if name == model.TableHealth && i >= healthFormulaColumn {
			break
		}
		h = append(h, cellText(v))
	}
	for len(h) > 0 && h[len(h)-1] == "" {
		h = h[:len(h)-1]
	}
	return h
}

// dataRows turns the rows after the header into Rows by header, one per sheet
// row; a blank row is nil, so positions are kept. When a header is written
// twice, the first column's value is kept; unnamed columns are skipped.
func dataRows(header []string, values [][]any) []api.Row {
	if len(values) <= 1 {
		return nil
	}
	out := make([]api.Row, len(values)-1)
	for i, cells := range values[1:] {
		r := make(api.Row, len(header))
		blank := true
		for j, h := range header {
			if h == "" {
				continue
			}
			if _, seen := r[h]; seen {
				continue
			}
			v := ""
			if j < len(cells) {
				v = cellText(cells[j])
			}
			if v != "" {
				blank = false
			}
			r[h] = v
		}
		if !blank {
			out[i] = r
		}
	}
	return out
}

// cellText turns a values-API cell into text: numbers in plain decimal,
// booleans as Sheets shows them.
func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	default:
		return fmt.Sprint(x)
	}
}

// missingRange reports Sheets' answer to a range naming a tab that does not exist.
func missingRange(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest && strings.Contains(e.Message, "Unable to parse range")
}

// slowDown reports Google's "slow down" answer (429, RESOURCE_EXHAUSTED).
func slowDown(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusTooManyRequests
}

// readTries bounds how often a read or a commit is retried on "slow down";
// AppendEvents retries until its context ends.
const readTries = 6

// retry runs f, retrying "slow down" answers with a growing wait, at most
// tries times (0: until ctx is done).
func retry(ctx context.Context, tries int, f func() error) error {
	wait := 250 * time.Millisecond
	for n := 1; ; n++ {
		err := f()
		if err == nil || !slowDown(err) || (tries > 0 && n >= tries) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (gave up: %w)", err, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(2*wait, 8*time.Second)
	}
}

// colName is a zero-based column index in A1 letters: 0 is A, 26 is AA.
func colName(c int) string {
	name := ""
	for c++; c > 0; c = (c - 1) / 26 {
		name = string(rune('A'+(c-1)%26)) + name
	}
	return name
}
