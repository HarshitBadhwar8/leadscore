package sheets

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// StaleMessage is what Health!H1 shows when no run succeeded in three
// schedule intervals; NoSuccessMessage before any run has succeeded.
const (
	StaleMessage     = "STALE: no successful run in 3 intervals"
	NoSuccessMessage = "STALE: no successful run yet"
)

// Formula is the staleness formula for Health!H1 (contracts section 4), given
// the Health tab's header and its rows in sheet order (nil for a blank row).
// It names the cell holding the `last_success_at` result and compares it with
// NOW() against three `schedule` intervals; the schedule comes from the
// `schedule` result row (the run writes it), else the 15-minute default.
// Before any run has succeeded it shows NoSuccessMessage. The times compared
// are UTC, so setup sets the spreadsheet's time zone to UTC.
func Formula(header []string, rows []api.Row) string {
	value := indexOf(header, "value")
	cell := ""
	schedule := config.DefaultSchedule
	for i, r := range rows {
		if r == nil || r["kind"] != "result" {
			continue
		}
		switch r["key"] {
		case "last_success_at":
			if value >= 0 && r["value"] != "" {
				cell = fmt.Sprintf("%s%d", colName(value), i+2)
			}
		case "schedule":
			if r["value"] != "" {
				schedule = r["value"]
			}
		}
	}
	if cell == "" {
		return `="` + NoSuccessMessage + `"`
	}
	d, err := config.ParseDuration(schedule)
	if err != nil || d <= 0 {
		d, _ = config.ParseDuration(config.DefaultSchedule)
	}
	return fmt.Sprintf(`=IF(NOW()-DATEVALUE(LEFT(%s,10))-TIMEVALUE(MID(%s,12,8))>3*%s,"%s","ok")`,
		cell, cell, days(d), StaleMessage)
}

// days writes a duration as a decimal number of days, as short as is exact
// to ten places.
func days(d time.Duration) string {
	s := strconv.FormatFloat(d.Hours()/24, 'f', 10, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}

// FormulaRequest writes a formula into H1 of the tab, as a person typing it
// would (USER_ENTERED), the one cell the store writes that way.
func FormulaRequest(sheetID int64, formula string) *sheetsapi.Request {
	return &sheetsapi.Request{UpdateCells: &sheetsapi.UpdateCellsRequest{
		Start: &sheetsapi.GridCoordinate{SheetId: sheetID, ColumnIndex: healthFormulaColumn},
		Rows: []*sheetsapi.RowData{{Values: []*sheetsapi.CellData{
			{UserEnteredValue: &sheetsapi.ExtendedValue{FormulaValue: &formula}},
		}}},
		Fields: "userEnteredValue",
	}}
}

// isEventsTab reports a monthly events tab name ("Events 2026-10").
func isEventsTab(name string) bool {
	_, ok := eventsMonth(name)
	return ok
}

// protectionFor protects a tab the store creates the way setup protected its
// siblings: an Events tab with the editors of an existing protected Events tab
// (the receiver and run accounts), any other tool tab with the editors of an
// existing protected tool tab (the run account). People-owned tabs, and a
// spreadsheet with nothing protected yet, get none.
func protectionFor(book *sheetsapi.Spreadsheet, name string, sheetID int64) *sheetsapi.Request {
	if peopleOwned(name) {
		return nil
	}
	events := isEventsTab(name)
	for _, sh := range book.Sheets {
		if sh.Properties == nil || isEventsTab(sh.Properties.Title) != events || peopleOwned(sh.Properties.Title) {
			continue
		}
		for _, pr := range sh.ProtectedRanges {
			if pr.Editors == nil || len(pr.Editors.Users) == 0 || pr.WarningOnly {
				continue
			}
			return protectRequest(sheetID, pr.Editors.Users, pr.Description)
		}
	}
	return nil
}

func protectRequest(sheetID int64, editors []string, description string) *sheetsapi.Request {
	return &sheetsapi.Request{AddProtectedRange: &sheetsapi.AddProtectedRangeRequest{
		ProtectedRange: &sheetsapi.ProtectedRange{
			Range:       &sheetsapi.GridRange{SheetId: sheetID},
			Description: description,
			Editors:     &sheetsapi.Editors{Users: append([]string(nil), editors...)},
		}}}
}
