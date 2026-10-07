package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

// viewProblem is the Health problem a failed view write raises. It is a
// warning: the view is a convenience, so the run stays healthy (contracts
// section 9.2).
const viewProblem = "view_write_failed"

// afterSaveProblems are problems only an AfterSave step raises. Phase 2's
// Health write, which comes before AfterSave, keeps such a problem open
// rather than deleting it as resolved, since nothing has re-checked it yet;
// the Health write after AfterSave settles it.
var afterSaveProblems = map[string]bool{viewProblem: true}

var (
	// viewTimeout bounds one view write; the run's hard stop bounds it too.
	viewTimeout = 3 * time.Minute
	// viewChunkRows bounds one commit to a view tab (halved on ErrTooLarge).
	// A variable so tests can shrink it.
	viewChunkRows = 5000
)

// writeView is S16's AfterSave step (contracts section 9.2): on a SQLite
// store with store.view_spreadsheet set, it copies the committed Ranked,
// every export table and Health into the read-only Sheet view, straight
// through the Sheets API, with no lease and no schema version. Each tab is
// replaced whole, so the view always mirrors the store; an export lane added
// to the rubric gets its tab on the first write. A failure raises
// view_write_failed (a warning) and never fails the run.
func writeView(r *Run) error {
	id := r.Config.Store.ViewSpreadsheet
	if r.DryRun || r.Config.Store.Type != "sqlite" || id == "" {
		return nil
	}
	if err := copyToView(r, id); err != nil {
		r.Problem(viewProblem, "the Sheet view "+id+" was not updated: "+errText(err),
			"see the message; if the view cannot be opened, share it with the service account as an editor "+
				"(`leadscore setup sheet --repair`); if it was deleted, remove store.view_spreadsheet from leadscore.yml "+
				"and run `leadscore setup sheet --view` to make a new one; the next run tries again", true)
	}
	return nil
}

func copyToView(r *Run, id string) error {
	ctx, cancel := context.WithTimeout(r.Ctx, viewTimeout)
	defer cancel()
	svc, err := sheets.Connect(ctx, sheets.ViewConfig(r.Config.Store.Block, id))
	if err != nil {
		return err
	}
	// A spreadsheet with a State tab is a Sheets store, not a view: never
	// overwrite a store's Ranked and Health with this one's.
	info, err := sheets.Inspect(ctx, svc, id)
	if err != nil {
		return err
	}
	for _, tab := range info.Tabs {
		if tab.Name == model.TableState {
			return fmt.Errorf("spreadsheet %s has a State tab, so it is a Sheets store, not a view; refusing to write to it "+
				"(set store.view_spreadsheet to the view `leadscore setup sheet --view` made)", id)
		}
	}
	view := sheets.New(svc, id, "")
	tables := []string{model.TableRanked}
	for _, lane := range csvLanes(r) {
		tables = append(tables, model.ExportTable(lane))
	}
	tables = append(tables, model.TableHealth) // last, so it shows this write's outcome
	for _, t := range tables {
		rows, err := r.Store.ReadTable(ctx, t)
		if err != nil {
			return fmt.Errorf("reading %s: %w", t, err)
		}
		if t == model.TableHealth {
			// This write is succeeding: the view need not show an older
			// view_write_failed, which the Health write after AfterSave clears.
			kept := rows[:0:0]
			for _, row := range rows {
				if row["kind"] != healthProblem || row["key"] != viewProblem {
					kept = append(kept, row)
				}
			}
			rows = kept
		}
		if err := replaceTab(ctx, view, t, rows); err != nil {
			return fmt.Errorf("writing %s: %w", t, err)
		}
	}
	return nil
}

// replaceTab rewrites one view tab: the first chunk replaces the tab, later
// chunks append; a chunk too large for one request is halved.
func replaceTab(ctx context.Context, view api.Backend, table string, rows []api.Row) error {
	size, op := viewChunkRows, api.OpReplace
	for {
		n := min(size, len(rows))
		err := view.Commit(ctx, []api.TableWrite{{Table: table, Op: op, Rows: rows[:n]}})
		if errors.Is(err, api.ErrTooLarge) && n > 1 {
			size = n / 2 // the same chunk again, smaller
			continue
		}
		if err != nil {
			return err
		}
		rows, op = rows[n:], api.OpAppend
		if len(rows) == 0 {
			return nil
		}
	}
}
