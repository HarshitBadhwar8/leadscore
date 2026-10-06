package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// tabSpec is one tab of the template.
type tabSpec struct {
	name    string
	columns []string
	people  bool // typed by people: unprotected, green tab
}

// template lists the tabs in the order a person meets them: what to read
// (Ranked, Health), what to type (Leads, Companies, Overrides), then every
// tool tab, then this month's Events tab. The view has only Ranked and Health;
// export tabs are created by their first write.
func template(view bool, now time.Time) []tabSpec {
	def := func(name string) []string { d, _ := model.Def(name); return d.Columns }
	out := []tabSpec{
		{name: model.TableRanked, columns: def(model.TableRanked)},
		{name: model.TableHealth, columns: def(model.TableHealth)},
	}
	if view {
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
	return append(out, tabSpec{name: EventsTab(now), columns: eventColumns})
}

// Accounts are the service accounts the spreadsheet is shared with: Run
// writes every tool tab, Receiver appends to the Events tabs. On Docker one
// account is both.
type Accounts struct {
	Run, Receiver string
}

func (a Accounts) list() []string {
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
// header, hourly recalculation, UTC, the tool tabs protected for the
// accounts (Events tabs for both, every other tool tab for the run account),
// and the staleness formula in Health!H1. It does not share the spreadsheet;
// call Share next. It returns the new spreadsheet's id.
func Create(ctx context.Context, svc *Services, title string, view bool, acc Accounts, now time.Time) (string, error) {
	if acc.Run == "" {
		return "", errors.New("setup sheet: no account to share the spreadsheet with")
	}
	tabs := template(view, now)
	book := &sheetsapi.Spreadsheet{Properties: &sheetsapi.SpreadsheetProperties{
		Title: title, TimeZone: TimeZone, AutoRecalc: AutoRecalc, Locale: "en_US"}}
	for i, t := range tabs {
		width := int64(len(t.columns))
		if t.name == model.TableHealth {
			width = healthFormulaColumn + 1
		}
		props := &sheetsapi.SheetProperties{SheetId: int64(i + 1), Title: t.name, Index: int64(i),
			GridProperties: &sheetsapi.GridProperties{RowCount: 2, ColumnCount: width, FrozenRowCount: 1}}
		if t.people {
			props.TabColorStyle = &sheetsapi.ColorStyle{RgbColor: &sheetsapi.Color{Red: 0.2, Green: 0.66, Blue: 0.33}}
		}
		header := headerCells(0, 0, t.columns).UpdateCells.Rows
		book.Sheets = append(book.Sheets, &sheetsapi.Sheet{Properties: props,
			Data: []*sheetsapi.GridData{{RowData: header}}})
	}
	created, err := svc.Sheets.Spreadsheets.Create(book).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("creating the spreadsheet: %w", err)
	}
	id := created.SpreadsheetId
	var reqs []*sheetsapi.Request
	for _, sh := range created.Sheets {
		if r := protectTemplateTab(sh.Properties, acc, view); r != nil {
			reqs = append(reqs, r)
		}
		if sh.Properties.Title == model.TableHealth {
			reqs = append(reqs, FormulaRequest(sh.Properties.SheetId, Formula(nil, nil)))
		}
	}
	s := New(svc, id, "")
	if err := s.batchUpdate(ctx, reqs, readTries); err != nil {
		return id, fmt.Errorf("protecting the tabs of spreadsheet %s: %w", id, err)
	}
	return id, nil
}

// protectTemplateTab protects a tab setup knows: an Events tab for both
// accounts, any other tool tab (or a view tab) for the run account; nil for
// a people-owned or unknown tab.
func protectTemplateTab(p *sheetsapi.SheetProperties, acc Accounts, view bool) *sheetsapi.Request {
	name := p.Title
	switch {
	case isEventsTab(name):
		return protectRequest(p.SheetId, acc.list(), eventsTabNote)
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
	for _, a := range acc.list() {
		p := &drive.Permission{Type: "user", Role: "writer", EmailAddress: a}
		if _, err := svc.Drive.Permissions.Create(id, p).SendNotificationEmail(false).
			SupportsAllDrives(true).Context(ctx).Do(); err != nil {
			return fmt.Errorf("sharing the spreadsheet with %s: %w", a, err)
		}
	}
	return nil
}

// Repair puts an existing spreadsheet's settings back (`setup sheet
// --repair`): hourly recalculation, UTC, protection on every tool tab that
// has none, and the Health!H1 formula. It does not add or change tabs or rows.
func Repair(ctx context.Context, svc *Services, id string, view bool, acc Accounts) error {
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
		if len(sh.ProtectedRanges) == 0 && acc.Run != "" {
			if r := protectTemplateTab(sh.Properties, acc, view); r != nil {
				reqs = append(reqs, r)
			}
		}
		if sh.Properties.Title == model.TableHealth {
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
	return s.batchUpdate(ctx, reqs, readTries)
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
	sortTabs(info.Tabs)
	return info, nil
}

func sortTabs(t []TabCells) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j].Cells > t[j-1].Cells; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

// SharedWith returns the lowercased email of everyone the spreadsheet is
// shared with, as Drive lists it. Listing needs the caller to be allowed to
// see the sharing; the caller decides what a failure means.
func SharedWith(ctx context.Context, svc *Services, id string) (map[string]bool, error) {
	out := map[string]bool{}
	err := svc.Drive.Permissions.List(id).SupportsAllDrives(true).Fields("permissions(emailAddress,role,type)").
		Pages(ctx, func(pl *drive.PermissionList) error {
			for _, p := range pl.Permissions {
				if p.EmailAddress != "" {
					out[strings.ToLower(p.EmailAddress)] = true
				}
			}
			return nil
		})
	return out, err
}

// ViewConfig is the block that reaches a spreadsheet other than the store's
// (the SQLite view): the store block with `spreadsheet` set to it.
func ViewConfig(store api.Config, spreadsheet string) api.Config {
	out := api.Config{}
	for k, v := range store {
		out[k] = v
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
