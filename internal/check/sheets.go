package check

import (
	"context"
	"fmt"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

func init() {
	Register(sheetAccess{})
	Register(sheetsSettings{})
}

// CellCap is Google Sheets' cell limit per spreadsheet; the `sheets` check
// warns past CellWarnShare of it (contracts section 11).
const (
	CellCap       = 10_000_000
	CellWarnShare = 0.70
)

// sheetTarget is the spreadsheet an install uses (the Sheets store, or a
// SQLite store's view) and the clients that reach it.
type sheetTarget struct {
	id   string
	view bool
	svc  *sheets.Services
}

// sheetTargetOf finds the install's spreadsheet: the open Sheets store, else
// the one leadscore.yml names. ok is false when the install uses none.
func sheetTargetOf(ctx context.Context, env Env) (t sheetTarget, ok bool, err error) {
	if s, isSheets := env.Store.(*sheets.Store); isSheets {
		return sheetTarget{id: s.SpreadsheetID(), svc: s.Services()}, true, nil
	}
	if env.Config == nil {
		return t, false, nil
	}
	st := env.Config.Store
	switch {
	case st.Type == "sheets" && st.Spreadsheet != "":
		t = sheetTarget{id: st.Spreadsheet}
	case st.Type == "sqlite" && st.ViewSpreadsheet != "":
		t = sheetTarget{id: st.ViewSpreadsheet, view: true}
	default:
		return t, false, nil
	}
	t.svc, err = sheets.Connect(ctx, sheets.ViewConfig(st.Block, t.id))
	return t, true, err
}

// sheetAccess is the `sheet-access` check (contracts section 10): this
// account must open the spreadsheet, and on Google Cloud both service
// accounts must be among its editors.
type sheetAccess struct{}

func (sheetAccess) Name() string { return "sheet-access" }
func (sheetAccess) InRun() bool  { return true }

func (sheetAccess) Run(ctx context.Context, env Env) []Problem {
	t, ok, err := sheetTargetOf(ctx, env)
	if !ok {
		return nil
	}
	fix := "share it with the account as an editor, or ask the Workspace admin for an exception to the sharing policy"
	if err == nil {
		_, err = sheets.Inspect(ctx, t.svc, t.id)
	}
	if err != nil {
		return []Problem{{Key: "sheet-access:" + t.id,
			Message: fmt.Sprintf("this account cannot open the spreadsheet %s: %v", t.id, err), Fix: fix}}
	}
	if env.Config == nil || !env.Config.Hosted() {
		return nil
	}
	acc, err := sheets.AccountsFrom(env.Config, t.view)
	if err != nil {
		return nil
	}
	shared, err := sheets.SharedWith(ctx, t.svc, t.id)
	if err != nil {
		return nil // this account may not see the sharing; opening it worked
	}
	var out []Problem
	for _, a := range []string{acc.Run, acc.Receiver} {
		if a != "" && !sheets.CanEdit(shared[strings.ToLower(a)]) {
			out = append(out, Problem{Key: "sheet-access:" + a,
				Message: fmt.Sprintf("the spreadsheet %s is not shared with %s as an editor", t.id, a),
				Fix:     "leadscore setup sheet --repair, or " + fix})
		}
	}
	return out
}

// sheetsSettings is the `sheets` check (contracts section 10): the
// spreadsheet recalculates hourly on UTC, so the Health staleness formula
// moves on its own, and its cell use stays under 70% of the cap.
type sheetsSettings struct{}

func (sheetsSettings) Name() string { return "sheets" }
func (sheetsSettings) InRun() bool  { return true }

func (sheetsSettings) Run(ctx context.Context, env Env) []Problem {
	t, ok, err := sheetTargetOf(ctx, env)
	if !ok || err != nil {
		return nil // sheet-access reports a spreadsheet that cannot be opened
	}
	info, err := sheets.Inspect(ctx, t.svc, t.id)
	if err != nil {
		return nil
	}
	repair := "leadscore setup sheet --repair"
	var out []Problem
	if info.AutoRecalc != sheets.AutoRecalc {
		out = append(out, Problem{Key: "sheets:recalc",
			Message: fmt.Sprintf("the spreadsheet recalculates %q, not every hour, so Health!H1 does not show staleness on its own", info.AutoRecalc),
			Fix:     repair})
	}
	if !sheets.UTCZone(info.TimeZone) {
		out = append(out, Problem{Key: "sheets:timezone",
			Message: fmt.Sprintf("the spreadsheet's time zone is %q, not UTC, so the Health!H1 staleness formula is off by the offset", info.TimeZone),
			Fix:     repair})
	}
	if share := float64(info.Cells) / CellCap; share > CellWarnShare {
		var big []string
		for i, tab := range info.Tabs {
			if i == 3 {
				break
			}
			big = append(big, fmt.Sprintf("%s (%d)", tab.Name, tab.Cells))
		}
		out = append(out, Problem{Key: "sheets:cells", Warning: true,
			Message: fmt.Sprintf("the spreadsheet uses %d of Google's %d cells (%.0f%%); the largest tabs are %s",
				info.Cells, CellCap, share*100, strings.Join(big, ", ")),
			Fix: "shorten log_retention, or move to SQLite"})
	}
	return out
}
