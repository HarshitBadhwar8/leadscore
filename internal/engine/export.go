package engine

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/csvsafe"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// exportFileMode is each export CSV's mode: the lists hold personal data, so
// only the owner may read them.
const exportFileMode = 0o600

// csvStores are the store types whose export tables are also written as CSV
// files in export.dir (RFC 6.11: on SQLite; a Sheets store shows them as
// tabs). A variable so a test can add its wrapped SQLite store.
var csvStores = map[string]bool{"sqlite": true}

// exportHook is the Export hook (RFC 6.11, contracts section 4 "Export
// rows"). It runs every run, after Push and before phase 2, pushes on or off
// and with or without a backlog; export lanes write no Pushes rows, so the
// table is their once-only record.
//
// It first adds each live lead that newly matches an export lane (its `when`
// held and it is not blocked on every lane) to that lane's table, once: a
// lead counts as listed when it, or any lead in its merge family, already has
// a row there. It then recomputes `status` and `do_not_contact` for every row
// of every export table in the store, including tables of lanes since removed
// from the rubric (State export_lane:<lane id>), following merged_into to the
// live lead, and puts only the rows whose values changed (updated_at moves
// only then).
//
// A run that did not score (the deadline, Stop, or a phase 1 too large)
// cannot judge the lane rules, so it adds no row and only ever turns
// do_not_contact on: an opt-out learned this run still reaches the list, and
// nothing is reopened on partial information.
func exportHook(r *Run) error {
	if r.DryRun {
		return nil
	}
	r.invalidate() // Push and PrePush may have changed the ledger and Outcomes
	v := r.view()
	defer r.invalidate()
	var errs []error
	if r.scored {
		for _, l := range r.Rubric.Lanes() {
			if l.Kind == kindExport {
				if err := addListed(r, v, l.ID); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	for _, lane := range exportLanes(r.Model, nil) {
		if err := refreshListed(r, v, lane); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// addListed adds every live lead that newly matches the lane to its table.
// The new rows' status and do_not_contact are set by refreshListed.
func addListed(r *Run, v *view, lane string) error {
	m := r.Model
	listed := map[api.LeadID]bool{}
	for _, row := range m.Exports[lane] {
		listed[v.live(row.LeadID)] = true
	}
	now := r.Now()
	for _, ref := range r.Input.Leads {
		id := ref.ID
		if listed[id] || !MatchesLane(r, id, lane) {
			continue
		}
		verdict := r.Result.Verdicts[id]
		row := model.ExportRow{
			LeadID: id, Email: v.idx.PrimaryEmail(id), LinkedInURL: v.idx.PrimaryLinkedIn(id),
			FullName: ref.FullName, CompanyDomain: ref.Domain,
			Score: verdict.AccountScore + verdict.ContactScore, Reasons: rules.ReasonsText(verdict),
			FirstListedAt: now, UpdatedAt: now,
			DoNotContact: true, // until refreshListed judges it
		}
		if err := m.Put(model.ExportTable(lane), row); err != nil {
			return fmt.Errorf("listing on export lane %s: %w", lane, err)
		}
		listed[id] = true
	}
	return nil
}

// refreshListed recomputes status and do_not_contact on every row of one
// export table and puts the rows that changed.
func refreshListed(r *Run, v *view, lane string) error {
	m := r.Model
	rows := m.Exports[lane]
	keys := make([]model.Key, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	// One row per person stays contactable: when several rows of the table
	// belong to one merge family, the live lead's own row is kept, else the
	// earliest listed (then the lowest lead id); every other row of the
	// family is do_not_contact (its lead was merged into a listed person).
	keeper := map[api.LeadID]model.Key{}
	for _, k := range keys {
		row := rows[k]
		live := v.live(row.LeadID)
		cur, ok := keeper[live]
		if !ok || betterKeeper(row, rows[cur], live) {
			keeper[live] = k
		}
	}

	now := r.Now()
	var errs []error
	for _, k := range keys {
		row := rows[k]
		live := v.live(row.LeadID)
		status := m.Outcomes[model.Key(live)].Status
		dnc := keeper[live] != k || doNotContact(r, v, row.LeadID) != ""
		if !r.scored && row.DoNotContact {
			dnc = true // a run that did not score never reopens a row
		}
		if status == row.Status && dnc == row.DoNotContact {
			continue
		}
		row.Status, row.DoNotContact, row.UpdatedAt = status, dnc, now
		if err := m.Put(model.ExportTable(lane), row); err != nil {
			errs = append(errs, fmt.Errorf("refreshing export lane %s: %w", lane, err))
			break // Put refuses the whole table, so every row fails alike
		}
	}
	return errors.Join(errs...)
}

// betterKeeper reports whether row a should stay contactable over row b, two
// rows of one merge family whose live lead is live.
func betterKeeper(a, b model.ExportRow, live api.LeadID) bool {
	if (a.LeadID == live) != (b.LeadID == live) {
		return a.LeadID == live
	}
	if !a.FirstListedAt.Equal(b.FirstListedAt) {
		return a.FirstListedAt.Before(b.FirstListedAt)
	}
	return a.LeadID < b.LeadID
}

// doNotContact is the contracts section 4 rule for a listed lead, judged on
// the live lead it leads to (merged_into followed), with the reason it holds,
// or "" when the lead may be contacted. The merged-into-a-listed-lead part of
// the rule is refreshListed's, since it reads the whole table.
func doNotContact(r *Run, v *view, id api.LeadID) string {
	if b, why := Blocked(r, id); b {
		return why // unsubscribed, a duplicate, a rubric or Overrides conflict, an unknown Overrides value
	}
	live := v.live(id)
	if v.deal(live, "") {
		return "the company has an open or won deal"
	}
	if s := v.status(live); blocksCold(s) {
		return "status " + s
	}
	for _, f := range v.family(live) {
		if !v.m.Outcomes[model.Key(f)].ContactedAt.IsZero() {
			return "already contacted"
		}
	}
	for _, p := range v.familyRows(live) {
		if v.holds(p) {
			return "already cold-pushed in lane " + p.LaneID
		}
		if v.isCold(p) && p.State == statePending {
			return "an open cold push in lane " + p.LaneID
		}
	}
	for _, name := range r.Result.Lanes[live] {
		if l, ok := v.lanes[name]; ok && l.Kind == kindCold {
			return "matches cold lane " + name
		}
	}
	return ""
}

// exportLanes lists, sorted, every lane with an export table to keep: each
// loaded table, each lane recorded in State (export_lane:<lane id>), and the
// extra lanes given (the rubric's export lanes, for the CSVs).
func exportLanes(m *model.Model, extra []string) []string {
	set := map[string]bool{}
	for lane := range m.Exports {
		set[lane] = true
	}
	for k := range m.State {
		if lane, ok := strings.CutPrefix(string(k), model.ExportLaneKey); ok && lane != "" {
			set[lane] = true
		}
	}
	for _, lane := range extra {
		set[lane] = true
	}
	out := make([]string, 0, len(set))
	for lane := range set {
		out = append(out, lane)
	}
	sort.Strings(out)
	return out
}

// writeExportCSVs is S13's part of AfterSave (contracts section 4): on a
// SQLite store it rewrites `<export.dir>/<lane id>.csv` for every export
// table, including those of lanes since removed, and for each export lane of
// the rubric (a lane nobody matched yet gets a header-only file). It runs
// only after phase 2 committed, never on a dry run, and reads each table
// back from the store, so a file only ever shows committed rows. Each file
// is written to a temporary file in export.dir and renamed over the old one,
// UTF-8, the header row in the section 4 column order, every cell made safe
// with csvsafe, mode 0600.
func writeExportCSVs(r *Run) error {
	if r.DryRun || !csvStores[r.Config.Store.Type] {
		return nil
	}
	var lanes []string
	for _, l := range r.Rubric.Lanes() {
		if l.Kind == kindExport {
			lanes = append(lanes, l.ID)
		}
	}
	lanes = exportLanes(r.Model, lanes)
	if len(lanes) == 0 {
		return nil
	}
	dir := r.Config.Export.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("export CSVs: creating export.dir: %w", err)
	}
	var errs []error
	for _, lane := range lanes {
		rows, err := r.Store.ReadTable(r.Ctx, model.ExportTable(lane))
		if err == nil {
			err = writeCSV(dir, lane, rows)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("export CSV for lane %s: %w", lane, err))
		}
	}
	return errors.Join(errs...)
}

// exportColumns is the export table's column order (contracts section 4).
func exportColumns() []string {
	for _, d := range model.Tables {
		if d.Name == model.ExportPrefix {
			return d.Columns
		}
	}
	panic("engine: no export table definition")
}

// writeCSV writes one lane's rows, oldest listed first, to a temporary file
// in dir and renames it to <lane>.csv.
func writeCSV(dir, lane string, rows []api.Row) (err error) {
	cols := exportColumns()
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i]["first_listed_at"] != rows[j]["first_listed_at"] {
			return rows[i]["first_listed_at"] < rows[j]["first_listed_at"]
		}
		return rows[i]["lead_id"] < rows[j]["lead_id"]
	})
	f, err := os.CreateTemp(dir, "."+lane+".csv.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(exportFileMode); err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.Write(cols); err != nil {
		return err
	}
	cells := make([]string, len(cols))
	for _, row := range rows {
		for i, c := range cols {
			cells[i] = row[c]
		}
		if err := w.Write(csvsafe.Row(cells)); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, lane+".csv"))
}
