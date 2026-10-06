package check

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

type sheetsFixture struct {
	fs    *fakesheets.Server
	block api.Config
	svc   *sheets.Services
	id    string
	cfg   *config.Config
}

var hostedAccounts = sheets.Accounts{Run: "run@p.iam.gserviceaccount.com", Receiver: "recv@p.iam.gserviceaccount.com"}

// newSheetsFixture makes a spreadsheet from the template, shared with both
// hosted accounts, and a hosted config naming it.
func newSheetsFixture(t *testing.T) *sheetsFixture {
	t.Helper()
	fs := fakesheets.New()
	srv := httptest.NewServer(gcs.Route(gcs.New(), fs))
	t.Cleanup(srv.Close)
	block := api.Config{"type": "sheets", "base_url": srv.URL, "_http_client": srv.Client()}
	svc, err := sheets.Connect(t.Context(), block)
	if err != nil {
		t.Fatal(err)
	}
	id, err := sheets.Create(t.Context(), svc, "leadscore", false, hostedAccounts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := sheets.Share(t.Context(), svc, id, hostedAccounts); err != nil {
		t.Fatal(err)
	}
	block["spreadsheet"] = id
	cfg := &config.Config{
		Store:   config.Store{Type: "sheets", Spreadsheet: id, Block: block},
		Hosting: &config.Hosting{Project: "p", RunAccount: "run", ReceiverAccount: "recv"},
	}
	return &sheetsFixture{fs: fs, block: block, svc: svc, id: id, cfg: cfg}
}

func keys(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Key)
	}
	return out
}

func TestSheetAccess(t *testing.T) {
	f := newSheetsFixture(t)
	ctx := t.Context()
	if ps := (sheetAccess{}).Run(ctx, Env{Config: f.cfg}); len(ps) != 0 {
		t.Errorf("a shared spreadsheet: %v", ps)
	}
	// Through the open store too.
	st, err := sheets.Open(ctx, f.block)
	if err != nil {
		t.Fatal(err)
	}
	if ps := (sheetAccess{}).Run(ctx, Env{Config: f.cfg, Store: st}); len(ps) != 0 {
		t.Errorf("through the store: %v", ps)
	}
	// The receiver account is not among the editors.
	f.cfg.Hosting.ReceiverAccount = "other"
	ps := (sheetAccess{}).Run(ctx, Env{Config: f.cfg})
	if got := keys(ps); len(got) != 1 || got[0] != "sheet-access:other@p.iam.gserviceaccount.com" {
		t.Errorf("unshared receiver: %v", got)
	}
	// This account cannot open it at all.
	f.fs.Deny(f.id, true)
	ps = (sheetAccess{}).Run(ctx, Env{Config: f.cfg})
	if got := keys(ps); len(got) != 1 || got[0] != "sheet-access:"+f.id || ps[0].Warning || !strings.Contains(ps[0].Fix, "Workspace admin") {
		t.Errorf("denied: %+v", ps)
	}
	// An install with no spreadsheet has nothing to check.
	if ps := (sheetAccess{}).Run(ctx, Env{Config: &config.Config{Store: config.Store{Type: "sqlite"}}}); len(ps) != 0 {
		t.Errorf("sqlite without a view: %v", ps)
	}
}

func TestSheetsSettings(t *testing.T) {
	f := newSheetsFixture(t)
	ctx := t.Context()
	if ps := (sheetsSettings{}).Run(ctx, Env{Config: f.cfg}); len(ps) != 0 {
		t.Errorf("a fresh template: %v", ps)
	}
	_, err := f.svc.Sheets.Spreadsheets.BatchUpdate(f.id, &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: []*sheetsapi.Request{
		{UpdateSpreadsheetProperties: &sheetsapi.UpdateSpreadsheetPropertiesRequest{
			Properties: &sheetsapi.SpreadsheetProperties{AutoRecalc: "ON_CHANGE", TimeZone: "Asia/Kolkata"}, Fields: "autoRecalc,timeZone"}},
		// About 7.2 million cells in Log: past 70% of the cap.
		{AppendDimension: &sheetsapi.AppendDimensionRequest{SheetId: logSheetID(t, f), Dimension: "ROWS", Length: 900_000}},
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	ps := (sheetsSettings{}).Run(ctx, Env{Config: f.cfg})
	got := strings.Join(keys(ps), " ")
	if got != "sheets:recalc sheets:timezone sheets:cells" {
		t.Fatalf("problems = %q", got)
	}
	for _, p := range ps {
		switch p.Key {
		case "sheets:cells":
			if !p.Warning || !strings.Contains(p.Message, "Log (") {
				t.Errorf("cells: %+v; want a warning naming Log", p)
			}
		default:
			if p.Warning || p.Fix != "leadscore setup sheet --repair" {
				t.Errorf("%s: %+v; want a failure fixed by --repair", p.Key, p)
			}
		}
	}
}

func logSheetID(t *testing.T, f *sheetsFixture) int64 {
	for _, sh := range f.fs.Spreadsheet(f.id).Sheets {
		if sh.Properties.Title == model.TableLog {
			return sh.Properties.SheetId
		}
	}
	t.Fatal("no Log tab")
	return 0
}

// The store check's version case reads State from a Sheets store too.
func TestStoreCheckOnSheets(t *testing.T) {
	f := newSheetsFixture(t)
	st, err := sheets.Open(t.Context(), f.block)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Commit(context.Background(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "schema_version", "value": "2.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	c := storeCheck{inContainer: func() bool { return false }, mountType: func(string) (string, bool) { return "", false }}
	if got := keys(c.Run(t.Context(), Env{Config: f.cfg, Store: st})); len(got) != 1 || got[0] != "store:newer_schema" {
		t.Errorf("problems = %v", got)
	}
}

func TestSheetsChecksRegisteredInRun(t *testing.T) {
	want := map[string]bool{"sheet-access": true, "sheets": true}
	for _, c := range InRun() {
		delete(want, c.Name())
	}
	if len(want) != 0 {
		t.Errorf("not registered to run in every run: %v", want)
	}
}
