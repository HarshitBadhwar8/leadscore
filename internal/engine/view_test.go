package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

// viewWorld is an export world on SQLite with a Sheet view made by
// `setup sheet --view` in a fake Google, and runs that reach it.
type viewWorld struct {
	*world
	fs     *fakesheets.Server
	sheet  string // the view's spreadsheet id
	client *http.Client
	view   *sheets.Store
}

func newViewWorld(t *testing.T, leads ...string) *viewWorld {
	t.Helper()
	fs := fakesheets.New()
	srv := httptest.NewServer(gcs.Route(gcs.New(), fs))
	t.Cleanup(srv.Close)
	svc, err := sheets.Connect(context.Background(), api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	id, err := sheets.Create(context.Background(), svc, sheets.Template{Title: "leadscore view", View: true,
		Accounts: sheets.Accounts{Run: "run@p.iam.gserviceaccount.com"}, ExportLanes: []string{"list"}})
	if err != nil {
		t.Fatal(err)
	}
	w := exportWorld(t, false, leads...)
	w.config("sinks:\n  apollo: {}\n  hubspot: {}\n  fake: {}\n", "")
	w.config("sources:", `store: { type: sqlite, path: leadscore.db, view_spreadsheet: "`+id+`", base_url: `+srv.URL+" }\nsources:")
	return &viewWorld{world: w, fs: fs, sheet: id, client: srv.Client(), view: sheets.New(svc, id, "")}
}

func (v *viewWorld) withClient(_ *api.RunOptions, s *settings) { s.client = v.client }

// tab reads a view tab.
func (v *viewWorld) tab(name string) []api.Row {
	v.t.Helper()
	rows, err := v.view.ReadTable(context.Background(), name)
	if err != nil {
		v.t.Fatal(err)
	}
	return rows
}

func leadIDs(rows []api.Row) string {
	var ids []string
	for _, r := range rows {
		ids = append(ids, r["lead_id"])
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func healthRow(rows []api.Row, kind, key string) api.Row {
	for _, r := range rows {
		if r["kind"] == kind && r["key"] == key {
			return r
		}
	}
	return nil
}

// After each run the view holds the committed Ranked, every export table and
// Health; a new export lane gets its tab; a dry run writes nothing to it.
func TestViewMirrorsTheStore(t *testing.T) {
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
	res, out := v.mustRun(v.withClient)
	if !res.Healthy {
		t.Fatalf("run unhealthy: %v\n%s", res.Problems, out)
	}
	if got, want := leadIDs(v.tab(model.TableRanked)), leadIDs(v.rows(model.TableRanked)); got != want || got == "" {
		t.Errorf("view Ranked %q, store %q", got, want)
	}
	if got, want := leadIDs(v.tab(model.ExportTable("list"))), leadIDs(v.rows(model.ExportTable("list"))); got != want || got == "" {
		t.Errorf("view export tab %q, store %q", got, want)
	}
	if r := healthRow(v.tab(model.TableHealth), "result", "last_result"); r == nil || r["value"] != "healthy" {
		t.Errorf("view Health last_result %v", r)
	}
	if ranked := v.tab(model.TableRanked); ranked[0]["score"] == "" || ranked[0]["email"] == "" {
		t.Errorf("view Ranked row lacks its columns: %v", ranked[0])
	}

	// A lane added to the rubric gets its own tab at the next run.
	v.write("rubric.yml", exportRubric(false)+`  - { id: heads, kind: export, priority: 2, when: { field: tier, eq: 1 }, push: "export:heads" }
`)
	v.mustRun(v.withClient)
	if got := leadIDs(v.tab(model.ExportTable("heads"))); got != string(v.id("ana@acme.example")) {
		t.Errorf("new lane's view tab lists %q", got)
	}

	before := v.fs.Calls("batchUpdate")
	v.mustRun(dry, v.withClient)
	if v.fs.Calls("batchUpdate") != before {
		t.Error("a dry run wrote the view")
	}
}

// A view write that fails (here every request is too large) is the warning view_write_failed: the run stays
// healthy, the problem keeps its first_seen_at while it lasts, and the next
// successful write clears it (also from the view's own Health tab).
func TestViewWriteFailureIsAWarning(t *testing.T) {
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	v.mustRun(v.withClient)
	// The view still opens (sheet-access passes), but no write fits.
	fits := sheets.MaxCommitBytes
	sheets.MaxCommitBytes = 10
	t.Cleanup(func() { sheets.MaxCommitBytes = fits })
	res, out := v.mustRun(v.withClient)
	if !res.Healthy {
		t.Fatalf("a failed view write made the run unhealthy: %v\n%s", res.Problems, out)
	}
	p := healthRow(v.rows(model.TableHealth), "problem", viewProblem)
	if p == nil || !strings.HasPrefix(p["value"], "warning: ") || !strings.Contains(p["value"], "setup sheet --repair") {
		t.Fatalf("Health problem %v", p)
	}
	if r := healthRow(v.rows(model.TableHealth), "result", "last_result"); r["value"] != "healthy" {
		t.Errorf("last_result %v", r)
	}
	v.mustRun(v.withClient)
	again := healthRow(v.rows(model.TableHealth), "problem", viewProblem)
	if again == nil || again["first_seen_at"] != p["first_seen_at"] {
		t.Errorf("a lasting view failure lost its first_seen_at: %v then %v", p, again)
	}

	sheets.MaxCommitBytes = fits
	v.mustRun(v.withClient)
	if r := healthRow(v.rows(model.TableHealth), "problem", viewProblem); r != nil {
		t.Errorf("view_write_failed still open after a good write: %v", r)
	}
	if r := healthRow(v.tab(model.TableHealth), "problem", viewProblem); r != nil {
		t.Errorf("the view's Health still shows view_write_failed: %v", r)
	}
}

// Without view_spreadsheet, or on a run that is not on SQLite, nothing
// reaches Google.
func TestNoViewNoCalls(t *testing.T) {
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	v.config(`view_spreadsheet: "`+v.sheet+`", `, "")
	before := v.fs.Calls("batchUpdate") + v.fs.Calls("get") + v.fs.Calls("values")
	v.mustRun(v.withClient)
	if after := v.fs.Calls("batchUpdate") + v.fs.Calls("get") + v.fs.Calls("values"); after != before {
		t.Errorf("a run with no view made %d Google calls", after-before)
	}
}

// A view that cannot be opened at all (deleted, or no longer shared) leaves
// the run healthy: sheet-access on the view is a warning inside a run, and
// the write failure is view_write_failed.
func TestViewThatCannotBeOpenedKeepsTheRunHealthy(t *testing.T) {
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	v.mustRun(v.withClient)
	v.fs.Deny(v.sheet, true)
	res, out := v.mustRun(v.withClient)
	if !res.Healthy {
		t.Fatalf("an unreachable view made the run unhealthy: %v\n%s", res.Problems, out)
	}
	for _, key := range []string{"sheet-access:" + v.sheet, viewProblem} {
		if p := healthRow(v.rows(model.TableHealth), "problem", key); p == nil || !strings.HasPrefix(p["value"], "warning: ") {
			t.Errorf("Health %s: %v, want a warning", key, p)
		}
	}
}

// A run cut short at its deadline still runs AfterSave; when its view write
// succeeds, an open view_write_failed is cleared, while the problems the run
// did not re-check stay.
func TestCutShortRunClearsAResolvedViewFailure(t *testing.T) {
	shrink(t, 10*time.Second)
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	fits := sheets.MaxCommitBytes
	sheets.MaxCommitBytes = 10
	t.Cleanup(func() { sheets.MaxCommitBytes = fits })
	v.mustRun(v.withClient)
	if healthRow(v.rows(model.TableHealth), "problem", viewProblem) == nil {
		t.Fatal("setup: no view_write_failed")
	}
	sheets.MaxCommitBytes = fits
	v.config("pushes_enabled: false", "pushes_enabled: false\ndeadline: 200ms")
	hooks := DefaultHooks()
	hooks.Intake = func(r *Run) error { <-r.PushCtx.Done(); return nil }
	if _, out, err := v.install.run(hooks, v.withClient); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := healthRow(v.rows(model.TableHealth), "problem", viewProblem); p != nil {
		t.Errorf("a cut-short run whose view write succeeded kept %v", p)
	}
	if p := healthRow(v.rows(model.TableHealth), "problem", "deadline_passed"); p == nil {
		t.Error("setup: the run was not cut short")
	}
}

// A view_spreadsheet that is really a Sheets store (it has a State tab) is
// never written: the store's own Ranked stays as it was.
func TestViewRefusesASheetsStore(t *testing.T) {
	v := newViewWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	storeID, err := sheets.Create(context.Background(), v.view.Services(), sheets.Template{Title: "a real store",
		Accounts: sheets.Accounts{Run: "run@p.iam.gserviceaccount.com"}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	v.config(`view_spreadsheet: "`+v.sheet+`"`, `view_spreadsheet: "`+storeID+`"`)
	res, _ := v.mustRun(v.withClient)
	if !res.Healthy {
		t.Errorf("unhealthy: %v", res.Problems)
	}
	p := healthRow(v.rows(model.TableHealth), "problem", viewProblem)
	if p == nil || !strings.Contains(p["value"], "State tab") {
		t.Errorf("Health %v", p)
	}
	rows, err := sheets.New(v.view.Services(), storeID, "").ReadTable(context.Background(), model.TableRanked)
	if err != nil || len(rows) != 0 {
		t.Errorf("the store's Ranked was written: %v %v", rows, err)
	}
}

// A run cut short at its deadline whose AfterSave fails keeps the problems it
// did not re-check, with their first_seen_at (the late Health write).
func TestCutShortRunWithFailedAfterSaveKeepsProblems(t *testing.T) {
	shrink(t, 10*time.Second)
	in := basicInstall(t)
	in.config(leadsCSV + "  - { id: broken, type: stub }\n")
	setStub(t, "broken", &stubOut{err: errors.New("bad file")})
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	first := in.problemSince("source_failed:broken")

	in.config(leadsCSV + "deadline: 200ms\n")
	hooks := DefaultHooks()
	hooks.Intake = func(r *Run) error { <-r.PushCtx.Done(); return nil }
	hooks.AfterSave = func(*Run) error { return errors.New("after save broke") }
	if _, _, err := in.run(hooks); err != nil {
		t.Fatal(err)
	}
	if in.problemSince("step_failed:aftersave") == "" {
		t.Fatal("setup: AfterSave's failure is not in Health")
	}
	if got := in.problemSince("source_failed:broken"); got == "" || got != first {
		t.Errorf("a cut-short run's late Health write dropped or reset a problem it never re-checked: %q, first seen %q", got, first)
	}
}
