package engine

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// exportRubric lists every lead on the export lane `list`; with withCold it
// also has a cold lane for leads whose title says "cold".
func exportRubric(withCold bool) string {
	s := `version: 1
derive:
  tier:
    - when: { field: title, contains: head }
      then: 1
    - else: 2
score:
  contact:
    - { when: { field: title, contains: head }, points: 5 }
lanes:
  - { id: list, kind: export, priority: 1, when: { field: tier, lte: 2 }, push: "export:list" }
`
	if withCold {
		s += `  - id: cold-a
    kind: cold
    priority: 20
    when: { all: [ { field: title, contains: cold }, { field: receiver_only, eq: false } ] }
    push: "fake:a"
`
	}
	return s
}

// exportWorld is a world with pushes off and the export rubric.
func exportWorld(t *testing.T, withCold bool, leads ...string) *world {
	t.Helper()
	w := newWorld(t, leads...)
	w.config("pushes_enabled: true", "pushes_enabled: false")
	w.write("rubric.yml", exportRubric(withCold))
	return w
}

// listed returns the export rows of a lane by email (the row's own email).
func listed(w *world, lane string) map[string]api.Row {
	out := map[string]api.Row{}
	for _, r := range w.rows(model.ExportTable(lane)) {
		out[r["email"]] = r
	}
	return out
}

// dnc returns each listed email's do_not_contact.
func dnc(w *world, lane string) map[string]string {
	out := map[string]string{}
	for e, r := range listed(w, lane) {
		out[e] = r["do_not_contact"]
	}
	return out
}

func wantDNC(t *testing.T, w *world, lane string, want map[string]string) {
	t.Helper()
	got := dnc(w, lane)
	if len(got) != len(want) {
		t.Fatalf("lane %s lists %v, want %v", lane, got, want)
	}
	for e, v := range want {
		if got[e] != v {
			t.Errorf("lane %s: %s do_not_contact %q, want %q (all: %v)", lane, e, got[e], v, got)
		}
	}
}

// readCSV reads <dir>/out/<lane>.csv.
func readCSV(t *testing.T, w *world, lane string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(w.dir, "out", lane+".csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

const exportHeader = "lead_id,email,linkedin_url,full_name,company_domain,score,reasons,first_listed_at,status,do_not_contact,updated_at"

// A CSV-only run on SQLite lists every matching lead once and writes
// out/<lane>.csv from the committed table: the header in column order, mode
// 0600, every cell formula-safe, no temporary file left. A second run with
// nothing new changes no row.
func TestExportCSVOnlyRun(t *testing.T) {
	w := exportWorld(t, false,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,\"=HYPERLINK(\"\"http://x\"\")\",Clerk,beta.example")
	w.mustRun()
	rows := listed(w, "list")
	if len(rows) != 2 {
		t.Fatalf("listed %v", rows)
	}
	ana := rows["ana@acme.example"]
	if ana["status"] != statusNew || ana["do_not_contact"] != "no" || ana["score"] != "5" ||
		ana["company_domain"] != "acme.example" || ana["full_name"] != "Ana A" || ana["first_listed_at"] == "" ||
		ana["updated_at"] != ana["first_listed_at"] || !strings.Contains(ana["reasons"], "title contains head") {
		t.Errorf("ana's row %v", ana)
	}
	if w.state(model.ExportLaneKey+"list") != "yes" {
		t.Error("the lane's table is not recorded in State")
	}
	if n := len(w.rows(model.TablePushes)); n != 0 {
		t.Errorf("an export lane wrote %d Pushes rows", n)
	}

	recs := readCSV(t, w, "list")
	if strings.Join(recs[0], ",") != exportHeader || len(recs) != 3 {
		t.Fatalf("CSV %v", recs)
	}
	var bo []string
	for _, r := range recs[1:] {
		if r[1] == "bo@beta.example" {
			bo = r
		}
	}
	if bo == nil || bo[3] != `'=HYPERLINK("http://x")` || bo[5] != "0" {
		t.Errorf("bo's CSV line %q: the name must be quoted, the score left a number", bo)
	}
	info, err := os.Stat(filepath.Join(w.dir, "out", "list.csv"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("CSV mode %v %v, want 0600", info.Mode(), err)
	}
	entries, _ := os.ReadDir(filepath.Join(w.dir, "out"))
	if len(entries) != 1 {
		t.Errorf("out holds %d files, want only list.csv", len(entries))
	}

	before := listed(w, "list")
	w.mustRun()
	for e, r := range listed(w, "list") {
		if r["updated_at"] != before[e]["updated_at"] {
			t.Errorf("%s's row was rewritten with nothing changed", e)
		}
	}
}

// A dry run lists nobody and writes no CSV.
func TestExportDryRunWritesNothing(t *testing.T) {
	w := exportWorld(t, false, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.mustRun(dry)
	if _, err := os.Stat(filepath.Join(w.dir, "out")); !os.IsNotExist(err) {
		t.Errorf("a dry run made export.dir: %v", err)
	}
	w.mustRun()
	when := readCSV(t, w, "list")[1][10]
	w.event("ana@acme.example", api.Event{Kind: "optout", Email: "ana@acme.example", Origin: events.OriginReceiver})
	w.mustRun(dry)
	if got := readCSV(t, w, "list")[1]; got[10] != when || got[9] != "no" {
		t.Errorf("a dry run rewrote the CSV: %v", got)
	}
}

// The export proof: a later opt-out, an Overrides typo, an earlier cold push
// and a merge of two listed people each set the right row's do_not_contact
// to yes, and leave the other rows alone.
func TestExportDoNotContactFlips(t *testing.T) {
	t.Run("later opt-out", func(t *testing.T) {
		w := exportWorld(t, false,
			"ana@acme.example,Ana A,Head of Ops,acme.example",
			"bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "no", "bo@beta.example": "no"})
		boBefore := listed(w, "list")["bo@beta.example"]["updated_at"]
		w.event("ana@acme.example", api.Event{Kind: "optout", Email: "ana@acme.example", Origin: events.OriginReceiver})
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
		rows := listed(w, "list")
		if rows["ana@acme.example"]["status"] != statusUnsubscribed {
			t.Errorf("status column %q", rows["ana@acme.example"]["status"])
		}
		if rows["bo@beta.example"]["updated_at"] != boBefore {
			t.Error("an unchanged row was rewritten")
		}
		for _, r := range readCSV(t, w, "list")[1:] {
			if r[1] == "ana@acme.example" && (r[9] != "yes" || r[8] != statusUnsubscribed) {
				t.Errorf("the CSV still offers the opted-out lead: %v", r)
			}
		}
	})

	t.Run("Overrides typo", func(t *testing.T) {
		w := exportWorld(t, false,
			"ana@acme.example,Ana A,Head of Ops,acme.example",
			"bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		w.override("ana@acme.example", "status", "unsubscibed", "") // misspelt
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
	})

	t.Run("earlier cold push", func(t *testing.T) {
		w := exportWorld(t, false,
			"cal@acme.example,Cal C,Cold head,acme.example",
			"bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"cal@acme.example": "no", "bo@beta.example": "no"})
		// A cold lane arrives and pushes Cal; then it leaves the rubric
		// again. The held cold push alone keeps Cal's row at yes.
		w.write("rubric.yml", exportRubric(true))
		w.config("pushes_enabled: false", "pushes_enabled: true")
		w.mustRun()
		if got := pushedTo(w, w.fake, "push"); len(got) != 1 || got[0] != "cal@acme.example" {
			t.Fatalf("cold pushes %v", got)
		}
		wantDNC(t, w, "list", map[string]string{"cal@acme.example": "yes", "bo@beta.example": "no"})
		w.write("rubric.yml", exportRubric(false))
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"cal@acme.example": "yes", "bo@beta.example": "no"})
		// A cold step that was called and then cancelled, in a lane since
		// removed, sets no contacted_at; the ledger row alone holds Bo.
		bo := w.id("bo@beta.example")
		w.edit(func(m *model.Model) {
			m.Put(model.TablePushes, model.Push{LeadID: bo, LaneID: "gone", Step: "push", LaneKind: kindCold, Dest: "x",
				State: stateCancelled, CalledAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		})
		w.mustRun()
		if o := w.outcome("bo@beta.example"); o["contacted_at"] != "" {
			t.Fatalf("the test needs Bo without contacted_at: %v", o)
		}
		wantDNC(t, w, "list", map[string]string{"cal@acme.example": "yes", "bo@beta.example": "yes"})
	})

	t.Run("merge of two listed people", func(t *testing.T) {
		w := exportWorld(t, false,
			"ana@acme.example,Ana A,Head of Ops,acme.example",
			"ana.home@home.example,Ana Home,Clerk,home.example",
			"bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "no", "ana.home@home.example": "no", "bo@beta.example": "no"})
		w.override("ana@acme.example", "same_as", "ana.home@home.example", "")
		w.mustRun()
		a, h := w.id("ana@acme.example"), w.id("ana.home@home.example")
		survivor, absorbed := "ana@acme.example", "ana.home@home.example"
		if h < a {
			survivor, absorbed = absorbed, survivor
		}
		wantDNC(t, w, "list", map[string]string{survivor: "no", absorbed: "yes", "bo@beta.example": "no"})
	})
}

// A lead merged into a lead not yet listed: the family counts as listed, so
// the survivor is not added a second time, and the absorbed lead's row
// follows the survivor.
func TestExportFamilyListedOnce(t *testing.T) {
	w := exportWorld(t, false, "old@acme.example,Old O,,acme.example")
	w.write("rubric.yml", strings.Replace(exportRubric(false), "lte: 2", "eq: 1", 1))
	w.mustRun() // Old has no title: tier 2, not listed
	w.leads("old@acme.example,Old O,,acme.example", "new@acme.example,New N,Head of Ops,acme.example")
	w.mustRun()
	wantDNC(t, w, "list", map[string]string{"new@acme.example": "no"})
	w.override("old@acme.example", "same_as", "new@acme.example", "")
	w.mustRun() // Old survives (created first) and takes New's title: tier 1
	if live := merge.Live(mustModel(t, w), w.id("new@acme.example")); live != w.id("old@acme.example") {
		t.Fatalf("survivor %s", live)
	}
	wantDNC(t, w, "list", map[string]string{"new@acme.example": "no"})
	w.event("old@acme.example", api.Event{Kind: "optout", Email: "old@acme.example", Origin: events.OriginReceiver})
	w.mustRun()
	wantDNC(t, w, "list", map[string]string{"new@acme.example": "yes"})
}

// The table of a lane removed from the rubric is still refreshed every run,
// and its CSV rewritten.
func TestExportRemovedLaneStillRefreshed(t *testing.T) {
	w := exportWorld(t, false,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.mustRun()
	w.write("rubric.yml", strings.Replace(exportRubric(false), "id: list", "id: list2", 1))
	w.mustRun()
	wantDNC(t, w, "list2", map[string]string{"ana@acme.example": "no", "bo@beta.example": "no"})
	w.override("ana@acme.example", "status", "unsubscribed", "")
	w.mustRun()
	wantDNC(t, w, "list", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
	wantDNC(t, w, "list2", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
	for _, r := range readCSV(t, w, "list")[1:] {
		if r[1] == "ana@acme.example" && r[9] != "yes" {
			t.Errorf("the removed lane's CSV was not rewritten: %v", r)
		}
	}
}

// A resubscribe clears a manual opt-out, and the row goes back to no only when
// nothing else in the do_not_contact rule holds: an automated opt-out, or an
// earlier contact, keeps it at yes.
func TestExportResubscribe(t *testing.T) {
	w := exportWorld(t, false,
		"man@acme.example,Man M,Clerk,acme.example",
		"auto@beta.example,Auto A,Clerk,beta.example",
		"sent@gamma.example,Sent S,Clerk,gamma.example")
	w.mustRun()
	w.edit(func(m *model.Model) {
		for _, e := range []string{"man@acme.example", "auto@beta.example", "sent@gamma.example"} {
			merge.SetStatus(m, e, "unsubscribed", time.Now())
		}
	})
	w.event("sent@gamma.example", api.Event{Kind: "sent", Email: "sent@gamma.example", Origin: events.OriginReceiver})
	w.mustRun()
	wantDNC(t, w, "list", map[string]string{"man@acme.example": "yes", "auto@beta.example": "yes", "sent@gamma.example": "yes"})
	w.event("auto@beta.example", api.Event{Kind: "optout", Email: "auto@beta.example", Origin: events.OriginApolloLookup})
	w.edit(func(m *model.Model) {
		for _, e := range []string{"man@acme.example", "auto@beta.example", "sent@gamma.example"} {
			merge.SetStatus(m, e, "resubscribe", time.Now())
		}
	})
	w.mustRun()
	wantDNC(t, w, "list", map[string]string{"man@acme.example": "no", "auto@beta.example": "yes", "sent@gamma.example": "yes"})
}

// A run that did not score never turns a row back to no: here a row a
// person set to yes by hand stays yes through a run whose first save was too
// large twice, and the next full run judges it again.
func TestExportUnscoredRunNeverReopens(t *testing.T) {
	in := newInstall(t, "store: { type: flaky, path: leadscore.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\n", exportRubric(false))
	in.write("leads.csv", csvText("Email,Name,Title,Domain", "ana@acme.example,Ana A,Head of Ops,acme.example"))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	s := in.store()
	row := in.rows(model.ExportTable("list"))[0]
	row["do_not_contact"] = "yes"
	if err := s.Commit(t.Context(), []api.TableWrite{{Table: model.ExportTable("list"), Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{row}}}); err != nil {
		t.Fatal(err)
	}
	in.write("leads.csv", csvText("Email,Name,Title,Domain", "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example"))
	flaky.Lock()
	flaky.tooLarge = 2
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.tooLarge = 0; flaky.Unlock() })
	res, out, err := in.run(DefaultHooks())
	if err != nil || !hasKey(res.Problems, "commit_too_large") {
		t.Fatalf("%v %v %s", res, err, out)
	}
	rows := in.rows(model.ExportTable("list"))
	if len(rows) != 1 || rows[0]["do_not_contact"] != "yes" {
		t.Fatalf("an unscored run listed or reopened: %v", rows)
	}
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	for _, r := range in.rows(model.ExportTable("list")) {
		if r["do_not_contact"] != "no" {
			t.Errorf("a full run judges the row again: %v", r)
		}
	}
}

// The CSV is rewritten once phase 2 committed even when the Ranked write
// after it fails, so a list never lags an opt-out phase 2 saved; the rest of
// AfterSave (deleting processed events) does not run.
func TestExportCSVAfterRankedFailure(t *testing.T) {
	csvStores["flaky"] = true
	t.Cleanup(func() { delete(csvStores, "flaky") })
	in := newInstall(t, "store: { type: flaky, path: leadscore.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\n", exportRubric(false))
	in.write("leads.csv", csvText("Email,Name,Title,Domain", "ana@acme.example,Ana A,Head of Ops,acme.example"))
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	s := in.store()
	m := mustLoad(t, s)
	merge.SetStatus(m, "ana@acme.example", "unsubscribed", time.Now())
	if err := s.Commit(t.Context(), encodeAll(m)); err != nil {
		t.Fatal(err)
	}
	flaky.Lock()
	flaky.failRanked = 2
	flaky.Unlock()
	t.Cleanup(func() { flaky.Lock(); flaky.failRanked = 0; flaky.Unlock() })
	hooks := DefaultHooks()
	afterSave := false
	hooks.AfterSave = func(*Run) error { afterSave = true; return nil }
	if _, out, err := in.run(hooks); err == nil {
		t.Fatalf("the Ranked write should fail the run: %s", out)
	}
	if afterSave {
		t.Error("a failed Ranked write ran the whole AfterSave (deleting processed events), not only the CSV writer")
	}
	f, err := os.Open(filepath.Join(in.dir, "out", "list.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil || len(recs) != 2 || recs[1][9] != "yes" {
		t.Errorf("CSV after a failed Ranked write: %v %v", recs, err)
	}
}

func mustLoad(t *testing.T, s api.Backend) *model.Model {
	t.Helper()
	m, err := codec.Load(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustModel(t *testing.T, w *world) *model.Model { return mustLoad(t, w.store()) }

func encodeAll(m *model.Model) []api.TableWrite { return codec.Encode(m) }

// Each part of the do_not_contact rule, by the reason it gives: a deal at
// the company, a status that blocks cold lanes, an open cold row, and a
// matching cold lane (pushes off, so nothing is pushed).
func TestExportDoNotContactReasons(t *testing.T) {
	w := exportWorld(t, true,
		"dee@deal.example,Dee D,Clerk,deal.example",
		"dan@deal.example,Dan D,Clerk,deal.example",
		"rex@rex.example,Rex R,Clerk,rex.example",
		"cal@cal.example,Cal C,Cold clerk,cal.example",
		"pen@pen.example,Pen P,Cold clerk,pen.example",
		"ok@ok.example,Ok O,Clerk,ok.example")
	w.mustRun()
	dan, pen := w.id("dan@deal.example"), w.id("pen@pen.example")
	w.edit(func(m *model.Model) {
		o := m.Outcomes[model.Key(dan)]
		o.LeadID, o.DealID, o.DealStage = dan, "d1", "open"
		m.Put(model.TableOutcomes, o)
		m.Put(model.TablePushes, model.Push{LeadID: pen, LaneID: "cold-a", Step: "push", LaneKind: kindCold, Dest: "a",
			State: statePending, UpdatedAt: time.Now().UTC()})
	})
	w.override("rex@rex.example", "status", "replied_negative", "")
	got := map[string]string{}
	hooks := DefaultHooks()
	hooks.Export = func(r *Run) error {
		err := exportHook(r)
		v := r.view()
		for _, row := range r.Model.Exports["list"] {
			got[row.Email] = doNotContact(r, v, row.LeadID)
		}
		return err
	}
	if _, out, err := w.install.run(hooks); err != nil {
		t.Fatal(err, out)
	}
	want := map[string]string{
		"dee@deal.example": "the company has an open or won deal",
		"dan@deal.example": "the company has an open or won deal",
		"rex@rex.example":  "status replied_negative",
		"cal@cal.example":  "matches cold lane cold-a",
		"pen@pen.example":  "an open cold push in lane cold-a",
		"ok@ok.example":    "",
	}
	for e, why := range want {
		if got[e] != why {
			t.Errorf("%s: reason %q, want %q", e, got[e], why)
		}
	}
	for e, v := range dnc(w, "list") {
		if (v == "yes") != (want[e] != "") {
			t.Errorf("%s: do_not_contact %q, reason %q", e, v, want[e])
		}
	}
}

// exportInstall is an install with the export rubric, a CSV source holding
// the given lead lines (Email,Name,Title,Domain) and extra config lines.
func exportInstall(t *testing.T, store, extra string, leads ...string) *install {
	t.Helper()
	in := newInstall(t, "store: { type: "+store+", path: leadscore.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\n"+extra, exportRubric(false))
	in.write("leads.csv", csvText(append([]string{"Email,Name,Title,Domain"}, leads...)...))
	return in
}

func (in *install) mustRun(hooks Hooks, mod ...func(*api.RunOptions, *settings)) api.RunResult {
	in.t.Helper()
	res, out, err := in.run(hooks, mod...)
	if err != nil {
		in.t.Fatalf("run: %v\n%s", err, out)
	}
	return res
}

func (in *install) dnc(lane string) map[string]string {
	out := map[string]string{}
	for _, r := range in.rows(model.ExportTable(lane)) {
		out[r["email"]] = r["do_not_contact"]
	}
	return out
}

func (in *install) override(person, action, value string) {
	in.t.Helper()
	s := in.store()
	m := mustLoad(in.t, s)
	m.Put(model.TableOverrides, model.Override{Person: person, Action: action, Value: value})
	if err := s.Commit(in.t.Context(), codec.Encode(m)); err != nil {
		in.t.Fatal(err)
	}
}

// A manual opt-out in Overrides reaches the list even in a run that did not
// score: one stopped before the status fold, and one whose phase 1 was too
// large twice (so the fold's work was discarded).
func TestExportManualOptOutInUnscoredRuns(t *testing.T) {
	t.Run("stopped before the fold", func(t *testing.T) {
		in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
		in.mustRun(DefaultHooks())
		in.override("ana@acme.example", "status", "unsubscribed")
		stop := make(chan struct{})
		hooks := DefaultHooks()
		hooks.Enrich = func(r *Run) error { close(stop); <-r.PushCtx.Done(); return nil }
		res := in.mustRun(hooks, func(o *api.RunOptions, _ *settings) { o.Stop = stop })
		if !hasKey(res.Problems, "run_stopped") {
			t.Fatalf("problems %v", res.Problems)
		}
		for _, r := range in.rows(model.TableOutcomes) {
			if r["status"] == statusUnsubscribed {
				t.Fatal("the fold ran; the test needs a run stopped before it")
			}
		}
		if got := in.dnc("list"); got["ana@acme.example"] != "yes" || got["bo@beta.example"] != "no" {
			t.Errorf("do_not_contact %v", got)
		}
		recs := readInstallCSV(t, in, "list")
		for _, r := range recs[1:] {
			if r[1] == "ana@acme.example" && r[9] != "yes" {
				t.Errorf("CSV row %v", r)
			}
		}
	})
	t.Run("phase 1 too large twice", func(t *testing.T) {
		in := exportInstall(t, "flaky", "", "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
		in.mustRun(DefaultHooks())
		in.override("bo@beta.example", "status", "replied_negative")
		in.write("leads.csv", csvText("Email,Name,Title,Domain", "ana@acme.example,Ana A,Head of Ops,acme.example",
			"bo@beta.example,Bo B,Clerk,beta.example", "cy@gamma.example,Cy C,Clerk,gamma.example"))
		flaky.Lock()
		flaky.tooLarge = 2
		flaky.Unlock()
		t.Cleanup(func() { flaky.Lock(); flaky.tooLarge = 0; flaky.Unlock() })
		res := in.mustRun(DefaultHooks())
		if !hasKey(res.Problems, "commit_too_large") {
			t.Fatalf("problems %v", res.Problems)
		}
		if got := in.dnc("list"); got["bo@beta.example"] != "yes" || got["ana@acme.example"] != "no" || len(got) != 2 {
			t.Errorf("do_not_contact %v", got)
		}
	})
}

// New rows are capped at ingest_chunk_rows per lane per run (the rest come
// in later runs), while refreshes of listed rows are always written.
func TestExportNewRowsCapped(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "a@a.example,A A,Head,a.example", "b@b.example,B B,Head,b.example",
		"c@c.example,C C,Clerk,c.example", "d@d.example,D D,Clerk,d.example", "e@e.example,E E,Clerk,e.example")
	in.write("rubric.yml", strings.Replace(exportRubric(false), "lte: 2", "eq: 1", 1))
	in.mustRun(DefaultHooks())
	if got := in.dnc("list"); len(got) != 2 {
		t.Fatalf("listed %v, want the two heads", got)
	}
	// The lane widens to every lead with a chunk of 2.
	in.config("sources:\n  - { id: leads, type: csv, path: leads.csv }\ningest_chunk_rows: 2\n")
	// A second export lane matches everyone too: the cap is shared.
	in.write("rubric.yml", exportRubric(false)+`  - { id: list-b, kind: export, when: { field: tier, lte: 2 }, push: "export:b" }
`)
	in.override("a@a.example", "status", "unsubscribed")
	in.mustRun(DefaultHooks())
	got := in.dnc("list")
	if n := len(got) + len(in.dnc("list-b")); n != 4 || got["a@a.example"] != "yes" {
		t.Fatalf("after one capped run: %v and %v (2 listed before + 2 new across both lanes; a's refresh written)", got, in.dnc("list-b"))
	}
	for i := 0; i < 3; i++ {
		in.mustRun(DefaultHooks())
	}
	// Five on list, four on list-b: a opted out before list-b took her.
	if n := len(in.dnc("list")) + len(in.dnc("list-b")); n != 9 {
		t.Errorf("the rest is listed in later runs: %d rows", n)
	}
}

// Rows where only the "blocked on every lane" part of the rule holds: an
// unresolved duplicate and a rubric conflict, each by its reason.
func TestExportBlockedOnly(t *testing.T) {
	capture := func(w *world) map[string]string {
		got := map[string]string{}
		hooks := DefaultHooks()
		hooks.Export = func(r *Run) error {
			err := exportHook(r)
			v := r.view()
			for _, row := range r.Model.Exports["list"] {
				got[row.Email] = doNotContact(r, v, row.LeadID)
			}
			return err
		}
		if _, out, err := w.install.run(hooks); err != nil {
			t.Fatal(err, out)
		}
		return got
	}
	t.Run("unresolved duplicate", func(t *testing.T) {
		w := exportWorld(t, false, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		w.leads("ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example",
			"ana.two@acme.example,Ana A,Clerk,acme.example")
		got := capture(w)
		if !strings.Contains(got["ana@acme.example"], "unresolved duplicate") || got["bo@beta.example"] != "" {
			t.Errorf("reasons %v", got)
		}
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
	})
	t.Run("rubric conflict", func(t *testing.T) {
		w := exportWorld(t, false, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
		w.mustRun()
		w.write("rubric.yml", exportRubric(false)+"conflicts:\n  - { field: title }\n")
		ana := w.id("ana@acme.example")
		w.edit(func(m *model.Model) {
			p := m.People[model.Key(ana)]
			p.Conflicts = map[string][]model.Conflict{"title": {{Value: "CFO", SourceID: "other"}}}
			m.Put(model.TablePeople, p)
		})
		got := capture(w)
		if !strings.HasPrefix(got["ana@acme.example"], "a rubric conflict") || got["bo@beta.example"] != "" {
			t.Errorf("reasons %v", got)
		}
		wantDNC(t, w, "list", map[string]string{"ana@acme.example": "yes", "bo@beta.example": "no"})
	})
}

// A run whose Detect (or Enrich) failed lists nobody new and never turns a
// row back to no; the next full run does.
func TestExportDegradedRunNeverReopens(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	in.mustRun(DefaultHooks())
	s := in.store()
	row := in.rows(model.ExportTable("list"))[0]
	row["do_not_contact"] = "yes"
	if err := s.Commit(t.Context(), []api.TableWrite{{Table: model.ExportTable("list"), Op: api.OpUpsert, Key: []string{"lead_id"}, Rows: []api.Row{row}}}); err != nil {
		t.Fatal(err)
	}
	in.write("leads.csv", csvText("Email,Name,Title,Domain", "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example"))
	hooks := DefaultHooks()
	hooks.Detect = func(*Run) (rules.DetectorResults, error) { return rules.DetectorResults{}, errors.New("detector down") }
	in.run(hooks)
	if got := in.dnc("list"); len(got) != 1 || got["ana@acme.example"] != "yes" {
		t.Fatalf("a run with a failed Detect listed or reopened: %v", got)
	}
	in.mustRun(DefaultHooks())
	if got := in.dnc("list"); len(got) != 2 || got["ana@acme.example"] != "no" {
		t.Errorf("the next full run judges again: %v", got)
	}
}

// The CSV rewrite reads the tables under its own timeout, so it works even
// once the run's context is cancelled (a hard stop during the Ranked write).
func TestExportCSVAfterRunContextCancelled(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	var cerr error
	hooks := DefaultHooks()
	hooks.AfterSave = func(r *Run) error {
		ctx, cancel := context.WithCancel(r.Ctx)
		cancel()
		rr := *r
		rr.Ctx = ctx
		cerr = writeExportCSVs(&rr)
		return cerr
	}
	in.mustRun(hooks)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if recs := readInstallCSV(t, in, "list"); len(recs) != 2 {
		t.Errorf("CSV %v", recs)
	}
}

// Lane ids read from State are filtered through the lane id rule before
// they name a file, and a rubric lane matching a recorded lane only by case
// gets no file of its own.
func TestExportLaneIDsForFiles(t *testing.T) {
	m := model.New()
	m.SetState(model.ExportLaneKey+"../evil", "yes")
	m.SetState(model.ExportLaneKey+"List", "yes")
	m.SetState(model.ExportLaneKey+"", "yes")
	if got := exportLanes(m); strings.Join(got, ",") != "List" {
		t.Errorf("exportLanes %v", got)
	}
	rub, err := rules.Compile([]byte(exportRubric(false)))
	if err != nil {
		t.Fatal(err)
	}
	if got := csvLanes(&Run{Model: m, Rubric: rub}); strings.Join(got, ",") != "List" {
		t.Errorf("csvLanes %v: the rubric's `list` would overwrite List.csv on a case-insensitive disk", got)
	}
}

// Stale temporary files are removed; an export.dir others can read raises a
// warning.
func TestExportDirHousekeeping(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	dir := filepath.Join(in.dir, "out")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // past the umask
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".list.csv.123.tmp")
	keep := []string{".budget.csv.backup.tmp", ".list.csv.backup.tmp", ".other.csv.123.tmp", "list.csv.123.tmp", ".list.csv.123.tmp.bak"}
	for _, f := range append(keep, ".list.csv.123.tmp") {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := in.mustRun(DefaultHooks())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale temporary file was left")
	}
	for _, f := range keep {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("a file the writer never made was removed: %s", f)
		}
	}
	if !hasKey(res.Problems, exportDirProblem) || !res.Healthy {
		t.Errorf("want the %s warning on a healthy run: %+v", exportDirProblem, res)
	}
	if v := in.health()["problem:"+exportDirProblem]; !strings.HasPrefix(v, "warning: ") {
		t.Errorf("Health %q", v)
	}
	os.Chmod(dir, 0o700)
	if res := in.mustRun(DefaultHooks()); hasKey(res.Problems, exportDirProblem) {
		t.Error("the warning stays after chmod 700")
	}
}

// Export runs during an ingest backlog (pushing waits, listing does not).
func TestExportDuringBacklog(t *testing.T) {
	in := exportInstall(t, "sqlite", "ingest_chunk_rows: 1\n", "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
	in.mustRun(DefaultHooks())
	if got := in.dnc("list"); len(got) != 1 {
		t.Fatalf("listed %v during the backlog", got)
	}
	in.mustRun(DefaultHooks())
	if got := in.dnc("list"); len(got) != 2 {
		t.Errorf("listed %v", got)
	}
}

// A lane nobody matches still gets a header-only CSV.
func TestExportHeaderOnlyCSV(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "bo@beta.example,Bo B,Clerk,beta.example")
	in.write("rubric.yml", strings.Replace(exportRubric(false), "lte: 2", "eq: 1", 1))
	in.mustRun(DefaultHooks())
	if recs := readInstallCSV(t, in, "list"); len(recs) != 1 || strings.Join(recs[0], ",") != exportHeader {
		t.Errorf("CSV %v", recs)
	}
}

// A CSV write that fails keeps the old file and removes its temporary file.
func TestExportFailedCSVWriteKeepsOldFile(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	in.mustRun(DefaultHooks())
	path := filepath.Join(in.dir, "out", "list.csv")
	before, _ := os.ReadFile(path)
	in.override("ana@acme.example", "status", "unsubscribed")
	renameFile = func(string, string) error { return errors.New("disk full") }
	t.Cleanup(func() { renameFile = os.Rename })
	res, _, _ := in.run(DefaultHooks())
	if !hasKey(res.Problems, "step_failed:aftersave") {
		t.Errorf("problems %v", res.Problems)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("the old CSV was changed by a failed write")
	}
	entries, _ := os.ReadDir(filepath.Join(in.dir, "out"))
	if len(entries) != 1 {
		t.Errorf("out holds %d files, want only list.csv", len(entries))
	}
}

// csvsafe guards every column: a reasons cell starting with "+" is quoted.
func TestExportCSVReasonsCellIsSafe(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	in.write("rubric.yml", `version: 1
score:
  contact:
    - { when: { field: title, contains: head }, points: 5 }
lanes:
  - { id: list, kind: export, when: { field: title, contains: head }, push: "export:list" }
`)
	in.mustRun(DefaultHooks())
	recs := readInstallCSV(t, in, "list")
	if len(recs) != 2 || recs[1][6] != "'+5 contact: title contains head" {
		t.Errorf("reasons cell %q", recs[1][6])
	}
	if got := in.rows(model.ExportTable("list"))[0]["reasons"]; got != "+5 contact: title contains head" {
		t.Errorf("the table keeps the text as is: %q", got)
	}
}

func readInstallCSV(t *testing.T, in *install, lane string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(in.dir, "out", lane+".csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A mass switch to yes (a new cold lane claiming a whole list) lands even
// with a store that refuses big commits: the export tables are written after
// phase 2 in their own chunks, halved on ErrTooLarge, switches to yes first.
func TestExportMassSwitchLandsInChunks(t *testing.T) {
	var leads []string
	for i := 0; i < 12; i++ {
		leads = append(leads, fmt.Sprintf("c%02d@c%02d.example,C %02d,Cold clerk,c%02d.example", i, i, i, i))
	}
	in := exportInstall(t, "flaky", "", leads...)
	old := exportChunkRows
	exportChunkRows = 8
	flaky.Lock()
	flaky.maxExportRows, flaky.exportLog, flaky.exportInPhase2 = 3, nil, false
	flaky.Unlock()
	t.Cleanup(func() {
		exportChunkRows = old
		flaky.Lock()
		flaky.maxExportRows, flaky.exportLog, flaky.exportInPhase2 = 0, nil, false
		flaky.Unlock()
	})
	in.mustRun(DefaultHooks())
	if got := in.dnc("list"); len(got) != 12 {
		t.Fatalf("listed %d of 12", len(got))
	}
	in.write("rubric.yml", exportRubric(true))
	more := append([]string{}, leads...)
	for i := 0; i < 3; i++ {
		more = append(more, fmt.Sprintf("n%d@n%d.example,N %d,Clerk,n%d.example", i, i, i, i))
	}
	in.write("leads.csv", csvText(append([]string{"Email,Name,Title,Domain"}, more...)...))
	flaky.Lock()
	flaky.exportLog = nil
	flaky.Unlock()
	if _, out, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err, out)
	}
	got := in.dnc("list")
	yes := 0
	for e, v := range got {
		if v == "yes" {
			yes++
		} else if !strings.HasPrefix(e, "n") {
			t.Errorf("%s still no", e)
		}
	}
	if len(got) != 15 || yes != 12 {
		t.Errorf("after the switch: %d rows, %d yes: %v", len(got), yes, got)
	}
	flaky.Lock()
	log, inPhase2 := append([]string{}, flaky.exportLog...), flaky.exportInPhase2
	flaky.Unlock()
	if inPhase2 {
		t.Error("an export table was written in the phase 2 commit")
	}
	if strings.Join(log, ",") != strings.Repeat("yes,", 12)+"no,no,no" {
		t.Errorf("saved in the order %v, want every yes first", log)
	}
}

// The CSV rewrite stops before the lease runs out, and renames nothing
// once the lease is lost.
func TestExportCSVRespectsTheLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(rr *Run)
	}{
		{"lease about to run out", func(rr *Run) { rr.leaseUntil = time.Now().Add(csvLeaseMargin / 2) }},
		{"lease lost", func(rr *Run) { rr.Lease = lostLease{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
			in.mustRun(DefaultHooks())
			path := filepath.Join(in.dir, "out", "list.csv")
			before, _ := os.ReadFile(path)
			in.override("ana@acme.example", "status", "unsubscribed")
			var cerr error
			hooks := DefaultHooks()
			hooks.AfterSave = func(r *Run) error {
				rr := *r
				tc.mod(&rr)
				cerr = writeExportCSVs(&rr)
				return nil
			}
			in.mustRun(hooks)
			if cerr == nil {
				t.Error("the rewrite went ahead")
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Error("the CSV was replaced")
			}
			if entries, _ := os.ReadDir(filepath.Join(in.dir, "out")); len(entries) != 1 {
				t.Errorf("out holds %d files", len(entries))
			}
		})
	}
}

type lostLease struct{}

func (lostLease) Check(context.Context) error   { return api.ErrLeaseLost }
func (lostLease) Release(context.Context) error { return nil }

// A lane recorded in State with an id that breaks the lane id rule raises a
// problem instead of being dropped silently.
func TestExportInvalidRecordedLaneRaisesProblem(t *testing.T) {
	in := exportInstall(t, "sqlite", "", "ana@acme.example,Ana A,Head of Ops,acme.example")
	in.mustRun(DefaultHooks())
	err := in.store().Commit(t.Context(), []api.TableWrite{{Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": model.ExportLaneKey + "bad lane", "value": "yes"}}}})
	if err != nil {
		t.Fatal(err)
	}
	res := in.mustRun(DefaultHooks())
	if !hasKey(res.Problems, invalidLaneProblem+":bad lane") || res.Healthy {
		t.Errorf("problems %v healthy %v", res.Problems, res.Healthy)
	}
	if entries, _ := os.ReadDir(filepath.Join(in.dir, "out")); len(entries) != 1 {
		t.Errorf("out holds %d files, want only list.csv", len(entries))
	}
}
