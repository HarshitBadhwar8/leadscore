package sheets

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// `leadscore setup sheet` (contracts section 9.1 step 6): the spreadsheet is
// created from the section 4 schema, so the template is this code.

// Spreadsheet settings setup writes and the `sheets` check expects. The
// staleness formula compares stored UTC times with NOW(), so the spreadsheet
// runs on UTC; HOUR recalculation makes NOW() move with no edits.
const (
	TimeZone   = "Etc/GMT"
	AutoRecalc = "HOUR"
)

// UTCZone reports a spreadsheet time zone that is UTC.
func UTCZone(tz string) bool {
	switch tz {
	case "Etc/GMT", "Etc/UTC", "GMT", "UTC", "Etc/Greenwich", "Etc/Universal", "Etc/Zulu":
		return true
	}
	return false
}

// LeadsHeaders are the starting columns of the template's Leads tab: the
// built-in fields a team most often has, in words a person reads. The tab is
// the team's; they add, rename or remove columns freely.
var LeadsHeaders = []string{"Email", "Full name", "Title", "Company", "Company domain", "LinkedIn URL"}

// peopleRows is how many rows the people tabs start with, so there is room
// to type or paste.
const peopleRows = 1000

// Header notes a person sees when they point at a column of a tab they type
// in (contracts section 4 and section 7).
var headerNotes = map[string]map[string]string{
	model.TableOverrides: {
		"person": "Who this row is about: an email, a LinkedIn URL or a lead id. " +
			"Only a retry row may use * (every lead).",
		"action": "One of: status (set this person's status), same_as (they are the same person as value), " +
			"distinct (they are a different person from value, with the same name and company), " +
			"retry (retry failed pushes).",
		"value": "For status: unsubscribed, blocked, replied_positive, replied_negative, replied_neutral, " +
			"replied_unlabelled, or resubscribe (undo a manual unsubscribed). For same_as and distinct: the other person. " +
			"For retry: the lane id, or empty for every lane.",
		"note": "Why, in your words. The leadscore commands write the request time here for retry and resubscribe rows.",
	},
	model.TableCompanies: {
		"domain": "The company's website domain, like acme.com. Add any other columns as facts " +
			"(name, employees, funding stage, region): values here win over enrichment.",
	},
}

// HealthNote explains Health!H1 to a person.
const HealthNote = "ok: a run succeeded within the last three schedule intervals. " +
	"STALE: none did, so leadscore may have stopped; run `leadscore doctor`. " +
	"The spreadsheet recalculates this every hour."

// Tab colors: green for tabs people type in, blue for Ranked, amber for Health.
var (
	peopleColor = &sheetsapi.Color{Red: 0.2, Green: 0.66, Blue: 0.33}
	tabColors   = map[string]*sheetsapi.Color{
		model.TableRanked: {Red: 0.26, Green: 0.52, Blue: 0.96},
		model.TableHealth: {Red: 0.98, Green: 0.74, Blue: 0.02},
	}
)

// tabSpec is one tab of the template.
type tabSpec struct {
	name    string
	columns []string
	people  bool // typed by people: unprotected, green tab, room to type
}

// Template says what Create makes.
type Template struct {
	Title    string
	View     bool     // the SQLite view: Ranked, Health and the export tabs only
	Accounts Accounts // who the tool tabs are protected for
	Now      time.Time
	// ExportLanes are the rubric's export lane ids; the view gets a tab each
	// (the store gets them from the run's first write).
	ExportLanes []string
}

// tabs lists the template's tabs in the order a person meets them: what to
// read (Ranked, Health), what to type (Leads, Companies, Overrides), then
// every tool tab, then this month's Events tab. The view has Ranked, Health
// and one tab per export lane.
func (t Template) tabs() []tabSpec {
	def := func(name string) []string { d, _ := model.Def(name); return d.Columns }
	out := []tabSpec{
		{name: model.TableRanked, columns: def(model.TableRanked)},
		{name: model.TableHealth, columns: def(model.TableHealth)},
	}
	if t.View {
		for _, lane := range t.ExportLanes {
			name := model.ExportTable(lane)
			out = append(out, tabSpec{name: name, columns: def(name)})
		}
		return out
	}
	out = append(out,
		tabSpec{name: "Leads", columns: LeadsHeaders, people: true},
		tabSpec{name: model.TableCompanies, columns: []string{"domain"}, people: true},
		tabSpec{name: model.TableOverrides, columns: def(model.TableOverrides), people: true},
	)
	for _, d := range model.Tables {
		switch {
		case d.Pattern, d.Name == model.TableOverrides, d.Name == model.TableRanked, d.Name == model.TableHealth:
			continue
		}
		out = append(out, tabSpec{name: d.Name, columns: d.Columns})
	}
	return append(out, tabSpec{name: EventsTab(t.Now), columns: eventColumns})
}

// Accounts are the service accounts the spreadsheet is shared with: Run
// writes every tool tab, Receiver appends to the Events tabs. On Docker one
// account is both.
type Accounts struct {
	Run, Receiver string
}

// List is the distinct accounts, the run account first.
func (a Accounts) List() []string {
	out := []string{a.Run}
	if a.Receiver != "" && !strings.EqualFold(a.Receiver, a.Run) {
		out = append(out, a.Receiver)
	}
	return out
}

// Protection descriptions, shown to a person who tries to edit the tab.
const (
	toolTabNote   = "leadscore writes this tab. To change a lead, use the Overrides tab."
	eventsTabNote = "leadscore's receiver writes this tab."
)

// Create makes a new spreadsheet from the template, owned by whoever svc
// signs in as (the person): every tab at its exact width with a bold, frozen
// header (notes on the Overrides and Companies headers), the machine-data tabs
// hidden, the people tabs green with room to type, Ranked and Health colored,
// hourly recalculation, UTC, editors unable to re-share, the tool tabs
// protected for the accounts (Events tabs for both, every other tool tab for
// the run account), and the staleness formula in Health!H1 with its note and
// a red highlight on STALE. It does not share the spreadsheet; call Share
// next. It returns the new spreadsheet's id.
func Create(ctx context.Context, svc *Services, t Template) (string, error) {
	acc := t.Accounts
	if acc.Run == "" {
		return "", errors.New("setup sheet: no account to share the spreadsheet with")
	}
	book := &sheetsapi.Spreadsheet{Properties: &sheetsapi.SpreadsheetProperties{
		Title: t.Title, TimeZone: TimeZone, AutoRecalc: AutoRecalc, Locale: "en_US"}}
	for i, tab := range t.tabs() {
		width, rows := int64(len(tab.columns)), int64(2)
		if tab.name == model.TableHealth {
			width = healthFormulaColumn + 1
		}
		props := &sheetsapi.SheetProperties{SheetId: int64(i + 1), Title: tab.name, Index: int64(i),
			Hidden: hiddenTab(tab.name)}
		color := tabColors[tab.name]
		if tab.people {
			rows, color = peopleRows, peopleColor
		}
		if color != nil {
			props.TabColorStyle = &sheetsapi.ColorStyle{RgbColor: color}
		}
		props.GridProperties = &sheetsapi.GridProperties{RowCount: rows, ColumnCount: width, FrozenRowCount: 1}
		header := headerRow(tab.columns, headerNotes[tab.name])
		if tab.name == model.TableHealth {
			formula := Formula(nil, nil)
			for len(header.Values) < healthFormulaColumn {
				header.Values = append(header.Values, &sheetsapi.CellData{})
			}
			header.Values = append(header.Values, &sheetsapi.CellData{Note: HealthNote,
				UserEnteredValue: &sheetsapi.ExtendedValue{FormulaValue: &formula}})
		}
		book.Sheets = append(book.Sheets, &sheetsapi.Sheet{Properties: props,
			Data: []*sheetsapi.GridData{{RowData: []*sheetsapi.RowData{header}}}})
	}
	created, err := svc.Sheets.Spreadsheets.Create(book).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("creating the spreadsheet: %w", err)
	}
	id := created.SpreadsheetId
	var reqs []*sheetsapi.Request
	for _, sh := range created.Sheets {
		if r := protectTemplateTab(sh.Properties, acc); r != nil {
			reqs = append(reqs, r)
		}
		if sh.Properties.Title == model.TableHealth {
			reqs = append(reqs, staleHighlight(sh.Properties.SheetId))
		}
	}
	if err := New(svc, id, "").batchUpdate(ctx, reqs, readTries); err != nil {
		return id, fmt.Errorf("protecting the tabs of spreadsheet %s: %w", id, err)
	}
	if err := noReshare(ctx, svc, id); err != nil {
		return id, err
	}
	return id, nil
}

// staleHighlight colors Health!H1 red while it starts with STALE.
func staleHighlight(sheetID int64) *sheetsapi.Request {
	return &sheetsapi.Request{AddConditionalFormatRule: &sheetsapi.AddConditionalFormatRuleRequest{
		Rule: &sheetsapi.ConditionalFormatRule{
			Ranges: []*sheetsapi.GridRange{{SheetId: sheetID, StartRowIndex: 0, EndRowIndex: 1,
				StartColumnIndex: healthFormulaColumn, EndColumnIndex: healthFormulaColumn + 1}},
			BooleanRule: &sheetsapi.BooleanRule{
				Condition: &sheetsapi.BooleanCondition{Type: "TEXT_STARTS_WITH",
					Values: []*sheetsapi.ConditionValue{{UserEnteredValue: "STALE"}}},
				Format: &sheetsapi.CellFormat{
					BackgroundColor: &sheetsapi.Color{Red: 0.92, Green: 0.26, Blue: 0.21},
					TextFormat:      &sheetsapi.TextFormat{Bold: true, ForegroundColor: &sheetsapi.Color{Red: 1, Green: 1, Blue: 1}},
				},
			},
		}}}
}

// noReshare stops editors, the service accounts among them, from sharing the
// spreadsheet further.
func noReshare(ctx context.Context, svc *Services, id string) error {
	_, err := svc.Drive.Files.Update(id, &drive.File{WritersCanShare: false, ForceSendFields: []string{"WritersCanShare"}}).
		SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("stopping editors from re-sharing spreadsheet %s: %w", id, err)
	}
	return nil
}

// protectTemplateTab protects a tab setup knows: an Events tab for both
// accounts, any other tool tab (or a view tab) for the run account; nil for
// a people-owned or unknown tab.
func protectTemplateTab(p *sheetsapi.SheetProperties, acc Accounts) *sheetsapi.Request {
	name := p.Title
	switch {
	case isEventsTab(name):
		return protectRequest(p.SheetId, acc.List(), eventsTabNote)
	case peopleOwned(name):
		return nil
	}
	if _, known := model.Def(name); !known {
		return nil
	}
	return protectRequest(p.SheetId, []string{acc.Run}, toolTabNote)
}

// Share gives each account edit access to the spreadsheet, with no email sent.
// A Workspace sharing policy that blocks it comes back as Drive's own error.
func Share(ctx context.Context, svc *Services, id string, acc Accounts) error {
	for _, a := range acc.List() {
		p := &drive.Permission{Type: "user", Role: "writer", EmailAddress: a}
		if _, err := svc.Drive.Permissions.Create(id, p).SendNotificationEmail(false).
			SupportsAllDrives(true).Context(ctx).Do(); err != nil {
			return fmt.Errorf("sharing the spreadsheet with %s: %w", a, err)
		}
	}
	return nil
}

// fullyProtected reports a protected range over a whole tab that blocks edits
// (not just warns).
func fullyProtected(sh *sheetsapi.Sheet) bool {
	for _, pr := range sh.ProtectedRanges {
		r := pr.Range
		if pr.WarningOnly || r == nil {
			continue
		}
		if r.StartRowIndex == 0 && r.EndRowIndex == 0 && r.StartColumnIndex == 0 && r.EndColumnIndex == 0 &&
			len(pr.UnprotectedRanges) == 0 {
			return true
		}
	}
	return false
}

// Repair puts an existing spreadsheet's settings back (`setup sheet
// --repair`): hourly recalculation, UTC, editors unable to re-share,
// protection on every tool tab not already fully protected, and the
// Health!H1 formula. It does not add or change tabs or rows.
func Repair(ctx context.Context, svc *Services, id string, acc Accounts) error {
	s := New(svc, id, "")
	book, err := s.meta(ctx)
	if err != nil {
		return err
	}
	reqs := []*sheetsapi.Request{{UpdateSpreadsheetProperties: &sheetsapi.UpdateSpreadsheetPropertiesRequest{
		Properties: &sheetsapi.SpreadsheetProperties{AutoRecalc: AutoRecalc, TimeZone: TimeZone},
		Fields:     "autoRecalc,timeZone",
	}}}
	for _, sh := range book.Sheets {
		if sh.Properties == nil {
			continue
		}
		if !fullyProtected(sh) && acc.Run != "" {
			if r := protectTemplateTab(sh.Properties, acc); r != nil {
				reqs = append(reqs, r)
			}
		}
		if strings.EqualFold(sh.Properties.Title, model.TableHealth) {
			formula, err := s.healthFormula(ctx)
			if err != nil {
				return err
			}
			if gp := sh.Properties.GridProperties; gp != nil && gp.ColumnCount < healthFormulaColumn+1 {
				reqs = append(reqs, &sheetsapi.Request{AppendDimension: &sheetsapi.AppendDimensionRequest{
					SheetId: sh.Properties.SheetId, Dimension: "COLUMNS", Length: healthFormulaColumn + 1 - gp.ColumnCount}})
			}
			reqs = append(reqs, FormulaRequest(sh.Properties.SheetId, formula))
		}
	}
	if err := s.batchUpdate(ctx, reqs, readTries); err != nil {
		return err
	}
	return noReshare(ctx, svc, id)
}

// healthFormula reads Health as the commit does and returns its formula.
func (s *Store) healthFormula(ctx context.Context) (string, error) {
	vrs, err := s.batchGet(ctx, []string{QuoteTab(model.TableHealth)}, renderOption)
	if err != nil {
		return "", err
	}
	header := headerOf(model.TableHealth, vrs[0].Values)
	return Formula(header, dataRows(header, vrs[0].Values)), nil
}

// Info is what the doctor checks read about a spreadsheet.
type Info struct {
	Title      string
	AutoRecalc string
	TimeZone   string
	Tabs       []TabCells // every tab, largest first
	Cells      int64      // grid cells in the whole spreadsheet
}

// TabCells is one tab's grid size in cells.
type TabCells struct {
	Name  string
	Cells int64
}

// Inspect reads a spreadsheet's settings and cell use. It fails when the
// signed-in account cannot open the spreadsheet.
func Inspect(ctx context.Context, svc *Services, id string) (Info, error) {
	book, err := New(svc, id, "").meta(ctx)
	if err != nil {
		return Info{}, err
	}
	info := Info{}
	if p := book.Properties; p != nil {
		info.Title, info.AutoRecalc, info.TimeZone = p.Title, p.AutoRecalc, p.TimeZone
	}
	for _, sh := range book.Sheets {
		if sh.Properties == nil || sh.Properties.GridProperties == nil {
			continue
		}
		gp := sh.Properties.GridProperties
		n := gp.RowCount * gp.ColumnCount
		info.Cells += n
		info.Tabs = append(info.Tabs, TabCells{Name: sh.Properties.Title, Cells: n})
	}
	slices.SortStableFunc(info.Tabs, func(a, b TabCells) int { return cmp.Compare(b.Cells, a.Cells) })
	return info, nil
}

// SharedWith returns each account the spreadsheet is shared with, by
// lowercased email, and its Drive role. Listing needs the caller to be
// allowed to see the sharing; the caller decides what a failure means.
func SharedWith(ctx context.Context, svc *Services, id string) (map[string]string, error) {
	out := map[string]string{}
	err := svc.Drive.Permissions.List(id).SupportsAllDrives(true).Fields("nextPageToken,permissions(emailAddress,role,type)").
		Pages(ctx, func(pl *drive.PermissionList) error {
			for _, p := range pl.Permissions {
				if p.EmailAddress != "" {
					out[strings.ToLower(p.EmailAddress)] = p.Role
				}
			}
			return nil
		})
	return out, err
}

// CanEdit reports a Drive role that can edit the spreadsheet.
func CanEdit(role string) bool {
	switch role {
	case "writer", "owner", "organizer", "fileOrganizer":
		return true
	}
	return false
}

// ViewConfig is the block that reaches a spreadsheet other than the store's
// (the SQLite view): the store block with `spreadsheet` set to it.
func ViewConfig(store api.Config, spreadsheet string) api.Config {
	out := maps.Clone(store)
	if out == nil {
		out = api.Config{}
	}
	out["spreadsheet"] = spreadsheet
	return out
}

// AccountsFrom works out which accounts the spreadsheet is shared with. A
// Google Cloud install names them in `hosting` (run_account and
// receiver_account; a bare name is completed with the project's
// service-account domain). Otherwise the one account is the service account
// in `store.credentials`. The view is shared with the run account only.
func AccountsFrom(c *config.Config, view bool) (Accounts, error) {
	if h := c.Hosting; h != nil && h.RunAccount != "" {
		acc := Accounts{Run: accountEmail(h.RunAccount, h.Project)}
		if h.ReceiverAccount != "" && !view {
			acc.Receiver = accountEmail(h.ReceiverAccount, h.Project)
		}
		return acc, nil
	}
	if c.Store.Credentials != "" {
		data, err := os.ReadFile(c.Store.Credentials)
		if err != nil {
			return Accounts{}, fmt.Errorf("reading store.credentials: %w", err)
		}
		var key struct {
			ClientEmail string `json:"client_email"`
		}
		if err := json.Unmarshal(data, &key); err != nil || key.ClientEmail == "" {
			return Accounts{}, fmt.Errorf("store.credentials %s is not a service-account key file (no client_email)", c.Store.Credentials)
		}
		return Accounts{Run: key.ClientEmail}, nil
	}
	return Accounts{}, errors.New("no account to share the spreadsheet with: run `setup/gcp.sh accounts` " +
		"(it sets hosting.run_account and hosting.receiver_account), or set store.credentials to the service account's key file")
}

func accountEmail(name, project string) string {
	if strings.Contains(name, "@") || project == "" {
		return name
	}
	return name + "@" + project + ".iam.gserviceaccount.com"
}
