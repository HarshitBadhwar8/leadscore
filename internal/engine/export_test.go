package engine

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
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

// The S13 proof: a later opt-out, an Overrides typo, an earlier cold push
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

// A resubscribe clears a manual opt-out, and the row goes back to no only
// when nothing else in the C4 rule holds: an automated opt-out, or an
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
// after it fails, so a list never lags an opt-out phase 2 saved.
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
	if _, out, err := in.run(DefaultHooks()); err == nil {
		t.Fatalf("the Ranked write should fail the run: %s", out)
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
