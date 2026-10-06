package sheets_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

var testAccounts = sheets.Accounts{Run: "run@p.iam.gserviceaccount.com", Receiver: "recv@p.iam.gserviceaccount.com"}

// The template: every section 4 tab at its exact width with a frozen header,
// in reading order; people tabs open, Events tabs protected for both
// accounts, every other tool tab for the run account; hourly recalculation
// on UTC; the staleness formula in Health!H1.
func TestCreateTemplate(t *testing.T) {
	f := newFakeGoogle(t)
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	id, err := sheets.Create(t.Context(), mustConnect(t, f), "leadscore", false, testAccounts, now)
	if err != nil {
		t.Fatal(err)
	}
	book := f.Sheets.Spreadsheet(id)
	if p := book.Properties; p.AutoRecalc != "HOUR" || !sheets.UTCZone(p.TimeZone) || p.Title != "leadscore" {
		t.Errorf("spreadsheet properties = %+v", p)
	}
	var names []string
	for _, sh := range book.Sheets {
		names = append(names, sh.Properties.Title)
	}
	want := []string{"Ranked", "Health", "Leads", "Companies", "Overrides", "Applied overrides", "People", "Identities",
		"Company facts", "Window events", "Applied rows", "Seen events", "Outcomes", "Pushes", "Log", "State", "Events 2026-10"}
	if !slices.Equal(names, want) {
		t.Fatalf("tabs = %q\nwant %q", names, want)
	}
	for _, sh := range book.Sheets {
		name := sh.Properties.Title
		gp := sh.Properties.GridProperties
		width := int64(0)
		switch {
		case name == "Leads":
			width = int64(len(sheets.LeadsHeaders))
		case name == "Companies":
			width = 1
		case name == "Health":
			width = 8
		default:
			d, _ := model.Def(name)
			width = int64(len(d.Columns))
		}
		if gp.ColumnCount != width || gp.FrozenRowCount != 1 || gp.RowCount != 2 {
			t.Errorf("%s grid = %d x %d, %d frozen; want 2 x %d, 1 frozen", name, gp.RowCount, gp.ColumnCount, gp.FrozenRowCount, width)
		}
		var editors []string
		for _, pr := range sh.ProtectedRanges {
			editors = append(editors, pr.Editors.Users...)
		}
		switch name {
		case "Leads", "Companies", "Overrides":
			if len(editors) != 0 {
				t.Errorf("%s is people's and must not be protected; editors %v", name, editors)
			}
		case "Events 2026-10":
			if !slices.Equal(editors, []string{testAccounts.Run, testAccounts.Receiver}) {
				t.Errorf("%s editors = %v, want both accounts", name, editors)
			}
		default:
			if !slices.Equal(editors, []string{testAccounts.Run}) {
				t.Errorf("%s editors = %v, want the run account", name, editors)
			}
		}
	}
	if got := f.Sheets.Cell(id, "Health", "H1"); got != `="`+sheets.NoSuccessMessage+`"` {
		t.Errorf("Health!H1 = %q", got)
	}
	if got := f.Sheets.Cell(id, "Overrides", "A1") + f.Sheets.Cell(id, "Overrides", "D1"); got != "personnote" {
		t.Errorf("Overrides header starts %q", got)
	}

	// The first run loads it and saves into it.
	s, err := sheets.Open(t.Context(), f.cfg(id))
	if err != nil {
		t.Fatal(err)
	}
	m, err := codec.Load(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	m.Put(model.TablePeople, model.Person{LeadID: "L1", CreatedAt: now})
	m.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "last_success_at", Value: model.FormatTime(now), UpdatedAt: now})
	if err := s.Commit(t.Context(), codec.Encode(m)); err != nil {
		t.Fatal(err)
	}
	if got := f.Sheets.Cell(id, "Health", "H1"); !strings.Contains(got, "LEFT(C2,10)") {
		t.Errorf("Health!H1 after the first run = %q", got)
	}
}

// The SQLite view holds only Ranked and Health, protected for its one account.
func TestCreateView(t *testing.T) {
	f := newFakeGoogle(t)
	id, err := sheets.Create(t.Context(), mustConnect(t, f), "leadscore view", true, sheets.Accounts{Run: "one@p.iam.gserviceaccount.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	book := f.Sheets.Spreadsheet(id)
	if len(book.Sheets) != 2 || book.Sheets[0].Properties.Title != "Ranked" || book.Sheets[1].Properties.Title != "Health" {
		t.Fatalf("view tabs = %v", book.Sheets)
	}
	for _, sh := range book.Sheets {
		if len(sh.ProtectedRanges) != 1 || !slices.Equal(sh.ProtectedRanges[0].Editors.Users, []string{"one@p.iam.gserviceaccount.com"}) {
			t.Errorf("%s protection = %+v", sh.Properties.Title, sh.ProtectedRanges)
		}
	}
}

// Sharing gives both accounts edit access with no email; a Workspace policy
// that blocks it comes back as Drive's own message.
func TestShare(t *testing.T) {
	f := newFakeGoogle(t)
	svc := mustConnect(t, f)
	id, err := sheets.Create(t.Context(), svc, "leadscore", false, testAccounts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := sheets.Share(t.Context(), svc, id, testAccounts); err != nil {
		t.Fatal(err)
	}
	shared, err := sheets.SharedWith(t.Context(), svc, id)
	if err != nil || !shared[testAccounts.Run] || !shared[testAccounts.Receiver] {
		t.Errorf("shared with %v, %v", shared, err)
	}
	for _, p := range f.Sheets.Permissions(id) {
		if p.Role != "writer" || p.Type != "user" {
			t.Errorf("permission %+v, want a writer user", p)
		}
	}
	f.Sheets.BlockSharing(id, "Sharing outside of your organization is not allowed.")
	if err := sheets.Share(t.Context(), svc, id, testAccounts); err == nil || !strings.Contains(err.Error(), "outside of your organization") {
		t.Errorf("a blocked share = %v", err)
	}
}

// --repair puts back hourly recalculation, UTC, missing protection and the
// formula, and changes no rows.
func TestRepair(t *testing.T) {
	f := newFakeGoogle(t)
	svc := mustConnect(t, f)
	id := f.Sheets.NewSpreadsheet("hand-made")
	s, _ := sheets.Open(t.Context(), f.cfg(id))
	commit(t, s, apiHealth("last_success_at", "2026-10-07T10:00:00.000Z"), apiPeople("L1"))
	// Someone narrowed Health and changed the settings.
	_, err := svc.Sheets.Spreadsheets.BatchUpdate(id, &sheetsapi.BatchUpdateSpreadsheetRequest{Requests: []*sheetsapi.Request{
		{UpdateSpreadsheetProperties: &sheetsapi.UpdateSpreadsheetPropertiesRequest{
			Properties: &sheetsapi.SpreadsheetProperties{AutoRecalc: "ON_CHANGE", TimeZone: "Asia/Kolkata"}, Fields: "autoRecalc,timeZone"}},
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if err := sheets.Repair(t.Context(), svc, id, false, testAccounts); err != nil {
		t.Fatal(err)
	}
	book := f.Sheets.Spreadsheet(id)
	if book.Properties.AutoRecalc != "HOUR" || !sheets.UTCZone(book.Properties.TimeZone) {
		t.Errorf("after repair: %+v", book.Properties)
	}
	if got := f.Sheets.Cell(id, "Health", "H1"); !strings.Contains(got, "LEFT(C2,10)") {
		t.Errorf("Health!H1 after repair = %q", got)
	}
	if p := tabNamed(t, book, "People").ProtectedRanges; len(p) != 1 {
		t.Errorf("People protection after repair = %+v", p)
	}
	if rows, _ := s.ReadTable(t.Context(), "People"); len(rows) != 1 {
		t.Errorf("repair changed rows: %v", rows)
	}
}

// Who the spreadsheet is shared with: the hosting accounts (a bare name gets
// the project's service-account domain), else the key file's account.
func TestAccountsFrom(t *testing.T) {
	c := &config.Config{Hosting: &config.Hosting{Project: "p", RunAccount: "leadscore-run", ReceiverAccount: "r@q.iam.gserviceaccount.com"}}
	acc, err := sheets.AccountsFrom(c, false)
	if err != nil || acc.Run != "leadscore-run@p.iam.gserviceaccount.com" || acc.Receiver != "r@q.iam.gserviceaccount.com" {
		t.Errorf("hosted accounts = %+v, %v", acc, err)
	}
	if acc, _ := sheets.AccountsFrom(c, true); acc.Receiver != "" {
		t.Errorf("the view is shared with the run account only: %+v", acc)
	}
	key := t.TempDir() + "/key.json"
	writeFile(t, key, `{"type":"service_account","client_email":"one@p.iam.gserviceaccount.com"}`)
	acc, err = sheets.AccountsFrom(&config.Config{Store: config.Store{Credentials: key}}, false)
	if err != nil || acc.Run != "one@p.iam.gserviceaccount.com" || acc.Receiver != "" {
		t.Errorf("key-file accounts = %+v, %v", acc, err)
	}
	if _, err := sheets.AccountsFrom(&config.Config{}, false); err == nil || !strings.Contains(err.Error(), "gcp.sh accounts") {
		t.Errorf("no accounts = %v", err)
	}
}

func apiHealth(key, value string) api.TableWrite {
	return api.TableWrite{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"},
		Rows: []api.Row{{"kind": "result", "key": key, "value": value}}}
}

func apiPeople(id string) api.TableWrite {
	return api.TableWrite{Table: model.TablePeople, Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{{"lead_id": id}}}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
