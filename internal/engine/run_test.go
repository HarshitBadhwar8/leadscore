package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

const leadsCSV = "sources:\n  - { id: leads, type: csv, path: leads.csv }\n"

func basicInstall(t *testing.T) *install {
	in := newInstall(t, leadsCSV, testRubric)
	in.write("leads.csv", csvText("Email,Name,Title",
		"ana@acme.example,Ana A,Head of Ops",
		"bo@acme.example,Bo B,Clerk"))
	return in
}

// The end-to-end smoke: the example rubric and leads on SQLite fill Ranked.
func TestExampleRunFillsRanked(t *testing.T) {
	examples, err := filepath.Abs("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	in := newInstall(t, "rubric: "+filepath.Join(examples, "rubric.yml")+"\nsources:\n  - { id: leads, type: csv, path: "+
		filepath.Join(examples, "leads.csv")+" }\n", "")
	// Hide the hubspot sink, as a build without it would be.
	old := sinkFactory
	sinkFactory = func(typ string) (func(api.Config) (api.Sink, error), bool) {
		if typ == "hubspot" {
			return nil, false
		}
		return old(typ)
	}
	t.Cleanup(func() { sinkFactory = old })
	res, out, err := in.run(DefaultHooks())
	if err != nil {
		t.Fatal(err)
	}
	ranked := in.rows(model.TableRanked)
	if len(ranked) != 8 {
		t.Fatalf("Ranked has %d rows, want one per example lead (8); output %s", len(ranked), out)
	}
	byEmail := map[string]api.Row{}
	for _, r := range ranked {
		byEmail[r["email"]] = r
	}
	anna := byEmail["anna.weber@kranlogistik.example"]
	if anna["tier"] != "1" || anna["priority"] != "A" || anna["score"] != "80" || anna["status"] != "new" || anna["lane"] != "fleet-ops" {
		t.Errorf("Anna's row: %v", anna)
	}
	// A vendor lane whose sink this build does not have is reported and
	// makes the run unhealthy; one whose sink it has is not reported. The
	// hubspot sink is hidden for this run (see below), apollo is real.
	if res.Healthy || !hasKey(res.Problems, "lane_sink_unregistered:demo-followup") || hasKey(res.Problems, "lane_sink_unregistered:fleet-ops") {
		t.Errorf("healthy %v, problems %v", res.Healthy, res.Problems)
	}
	if hasKey(res.Problems, "lane_sink_unregistered:nurture") {
		t.Errorf("problems %v", res.Problems)
	}
	h := in.health()
	if h["result:last_result"] != map[bool]string{true: "healthy", false: "unhealthy"}[res.Healthy] ||
		h["result:rubric_version"] == "" || h["result:schedule"] != "15m" {
		t.Errorf("Health %v", h)
	}
	t.Logf("output: %s", out)
}

func TestRunWritesBothPhases(t *testing.T) {
	in := basicInstall(t)
	var calls []string
	hook := func(name string) func(*Run) error {
		return func(*Run) error { calls = append(calls, name); return nil }
	}
	hooks := DefaultHooks()
	hooks.Intake, hooks.Enrich, hooks.Push, hooks.Export, hooks.AfterSave =
		hook("intake"), hook("enrich"), hook("push"), hook("export"), hook("aftersave")
	hooks.PrePush = func(_ *Run, _ []api.LeadID) error { calls = append(calls, "prepush"); return nil }
	res, out, err := in.run(hooks)
	if err != nil || !res.Healthy {
		t.Fatalf("run: %v %+v %s", err, res, out)
	}
	if got := strings.Join(calls, ","); got != "intake,enrich,prepush,push,export,aftersave" {
		t.Errorf("hook order %s", got)
	}
	if n := len(in.rows(model.TablePeople)); n != 2 {
		t.Errorf("People %d rows", n)
	}
	ranked := in.rows(model.TableRanked)
	if len(ranked) != 2 {
		t.Fatalf("Ranked %v", ranked)
	}
	h := in.health()
	if h["result:last_result"] != "healthy" || h["result:last_success_at"] == "" || in.state("first_run_at") == "" {
		t.Errorf("Health %v, first_run_at %q", h, in.state("first_run_at"))
	}
	if in.state("lease_owner") != "" {
		t.Error("the lease must be released")
	}
	// A second run keeps first_run_at and merges nothing new.
	first := in.state("first_run_at")
	if _, out, _ := in.run(DefaultHooks()); !strings.Contains(out, "0 input row(s) merged") {
		t.Errorf("second run: %s", out)
	}
	if in.state("first_run_at") != first {
		t.Error("first_run_at moved")
	}
}

func TestLeaseHeldSkipsAndWritesNothing(t *testing.T) {
	in := basicInstall(t)
	in.setLease("other-run", time.Now().Add(time.Hour))
	called := false
	hooks := Hooks{Intake: func(*Run) error { called = true; return nil }}
	res, out, err := in.run(hooks)
	if err != nil || !res.Skipped || !res.Healthy {
		t.Fatalf("got %+v %v", res, err)
	}
	if called || len(in.rows(model.TablePeople)) != 0 || len(in.rows(model.TableHealth)) != 0 {
		t.Error("a skipped run must write nothing and call no hook")
	}
	if in.state("lease_owner") != "other-run" {
		t.Error("the other run's lease must be untouched")
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("output %q", out)
	}
}

func TestExpiredLeaseIsTakenOverAndLogged(t *testing.T) {
	in := basicInstall(t)
	in.setLease("crashed-run", time.Now().Add(-time.Minute))
	res, _, err := in.run(DefaultHooks())
	if err != nil || res.Skipped || !res.Healthy {
		t.Fatalf("got %+v %v", res, err)
	}
	found := false
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "lease_takeover" && strings.Contains(r["message"], "crashed-run") {
			found = true
		}
	}
	if !found {
		t.Error("the takeover must be logged")
	}
	if in.state("lease_owner") != "" {
		t.Error("the run must release the lease it took over")
	}
}

// A run that stalls while another takes the lease writes nothing and does not
// release the successor's lease.
func TestStalledRunWritesNothing(t *testing.T) {
	in := basicInstall(t)
	hooks := DefaultHooks()
	hooks.Intake = func(_ *Run) error {
		in.setLease("successor", time.Now().Add(time.Hour)) // the lease expired and was taken over
		return nil
	}
	res, _, err := in.run(hooks)
	if !errors.Is(err, api.ErrLeaseLost) || res.Healthy {
		t.Fatalf("got %+v %v", res, err)
	}
	for _, table := range []string{model.TablePeople, model.TableIdentities, model.TableAppliedRows, model.TableHealth, model.TableRanked} {
		if rows := in.rows(table); len(rows) != 0 {
			t.Errorf("%s: %d rows written by a stalled run", table, len(rows))
		}
	}
	if in.state("lease_owner") != "successor" {
		t.Error("the stalled run released its successor's lease")
	}
}

// The deadline before phase 1: the rows merged so far are saved, nothing is
// scored or pushed, and the run is unhealthy.
func TestDeadlineBeforePhase1(t *testing.T) {
	shrink(t, 10*time.Second)
	in := basicInstall(t)
	in.config(leadsCSV + "deadline: 200ms\n")
	pushed, folded := false, false
	hooks := Hooks{
		Intake: func(r *Run) error { <-r.PushCtx.Done(); return nil },
		Fold:   func(*Run) error { folded = true; return nil },
		Push:   func(*Run) error { pushed = true; return nil },
	}
	res, _, err := in.run(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy || !hasKey(res.Problems, "deadline_passed") || pushed || folded {
		t.Errorf("result %+v pushed %v folded %v", res, pushed, folded)
	}
	if len(in.rows(model.TablePeople)) != 2 || len(in.rows(model.TableAppliedRows)) != 2 {
		t.Error("phase 1 must save the rows merged before the deadline")
	}
	if len(in.rows(model.TableRanked)) != 0 {
		t.Error("nothing was scored, so Ranked must stay as it was")
	}
	if h := in.health(); h["result:last_result"] != "unhealthy" || !strings.Contains(h["problem:deadline_passed"], "during intake") {
		t.Errorf("Health %v", h)
	}
}

// The deadline after phase 1: pushing stops, the run still saves phase 2 and
// Ranked within its budget.
func TestDeadlineAfterPhase1(t *testing.T) {
	shrink(t, 10*time.Second)
	in := basicInstall(t)
	in.config(leadsCSV + "deadline: 300ms\n")
	exported := false
	hooks := DefaultHooks()
	hooks.Push = func(r *Run) error { <-r.PushCtx.Done(); return nil }
	hooks.Export = func(*Run) error { exported = true; return nil }
	res, _, err := in.run(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy || !hasKey(res.Problems, "deadline_passed") || !exported {
		t.Errorf("result %+v exported %v", res, exported)
	}
	if len(in.rows(model.TableRanked)) != 2 {
		t.Error("Ranked must be written within the save budget")
	}
	if h := in.health(); !strings.Contains(h["problem:deadline_passed"], "while pushing") {
		t.Errorf("Health %v", h)
	}
}

// The hard stop: at the deadline plus the save budget the run's context ends,
// so a hook stuck past it cannot save anything more; the run ends and gives
// the lease back.
func TestHardStop(t *testing.T) {
	shrink(t, 300*time.Millisecond)
	in := basicInstall(t)
	in.config(leadsCSV + "deadline: 200ms\n")
	hooks := DefaultHooks()
	hooks.Push = func(r *Run) error { <-r.Ctx.Done(); return r.Ctx.Err() }
	start := time.Now()
	res, _, err := in.run(hooks)
	if err == nil || res.Healthy {
		t.Fatalf("a hard-stopped run must fail: %+v %v", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the run outlived its hard stop: %s", d)
	}
	if len(in.rows(model.TablePeople)) != 2 {
		t.Error("phase 1, committed before the stop, must stay")
	}
	if len(in.rows(model.TableRanked)) != 0 || len(in.rows(model.TableHealth)) != 0 {
		t.Error("nothing after the hard stop may be written")
	}
	if in.state("lease_owner") != "" {
		t.Error("the lease must still be released (owner-only)")
	}
}

// Closing Stop is the graceful stop: like the deadline it skips the steps
// left before phase 1 and every push, the run saves what it merged, and the
// run is healthy with a warning.
func TestStopIsGraceful(t *testing.T) {
	in := basicInstall(t)
	stop := make(chan struct{})
	hooks := DefaultHooks()
	hooks.Enrich = func(r *Run) error { close(stop); <-r.PushCtx.Done(); return nil }
	pushed := false
	hooks.Push = func(*Run) error { pushed = true; return nil }
	res, _, err := in.run(hooks, func(o *api.RunOptions, _ *settings) { o.Stop = stop })
	if err != nil || !res.Healthy || pushed || !hasKey(res.Problems, "run_stopped") || hasKey(res.Problems, "deadline_passed") {
		t.Fatalf("got %+v %v pushed %v", res, err, pushed)
	}
	if len(in.rows(model.TablePeople)) != 2 || len(in.rows(model.TableRanked)) != 0 {
		t.Error("a stopped run saves what it merged and scores nothing more")
	}
	if h := in.health(); h["result:last_success_at"] != "" || h["result:last_run_at"] == "" {
		t.Errorf("a run stopped before scoring is not a success: %v", h)
	}
}

// Closing Stop starts the save budget: the hard stop comes that long after,
// not at the deadline plus the budget.
func TestStopStartsTheSaveBudget(t *testing.T) {
	shrink(t, 300*time.Millisecond)
	in := basicInstall(t) // deadline 12m
	stop := make(chan struct{})
	hooks := DefaultHooks()
	hooks.Intake = func(*Run) error { close(stop); return nil }
	hooks.Export = func(r *Run) error { <-r.Ctx.Done(); return nil }
	start := time.Now()
	res, _, err := in.run(hooks, func(o *api.RunOptions, _ *settings) { o.Stop = stop })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the run outlived its save budget after Stop: %s", d)
	}
	if err == nil || res.Healthy {
		t.Errorf("a run hard-stopped before phase 2 fails: %+v %v", res, err)
	}
}

// leadscore.yml and the rubric are read fresh by every run.
func TestConfigReReadEveryRun(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head", "b@x.example,B,Clerk", "c@x.example,C,Clerk"))
	in.config(leadsCSV + "ingest_chunk_rows: 1\n")
	_, out, err := in.run(DefaultHooks())
	if err != nil || !strings.Contains(out, "1 input row(s) merged, 2 left") {
		t.Fatalf("run 1: %v %s", err, out)
	}
	v1 := in.health()["result:rubric_version"]

	in.config(leadsCSV + "ingest_chunk_rows: 5\n")
	in.write("rubric.yml", strings.Replace(testRubric, "points: 5", "points: 7", 1))
	_, out, err = in.run(DefaultHooks())
	if err != nil || !strings.Contains(out, "2 input row(s) merged, 0 left") {
		t.Fatalf("run 2: %v %s", err, out)
	}
	if v2 := in.health()["result:rubric_version"]; v2 == v1 {
		t.Error("the edited rubric was not re-read")
	}
	for _, r := range in.rows(model.TableRanked) {
		if r["email"] == "a@x.example" && r["contact_score"] != "7" {
			t.Errorf("scored with the old rubric: %v", r)
		}
	}
}

// A large import finishes over several runs, a chunk at a time; row groups
// are never split, a duplicated row id across a chunk boundary settles, and
// pushing waits until the backlog is empty.
func TestChunkedImportOverSeveralRuns(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title",
		"a@x.example,A,Head", // group a: lines 1 and 3
		"b@x.example,B,Clerk",
		"a@x.example,A,Head of Ops",
		"c@x.example,C,Clerk",
		"d@x.example,D,Clerk"))
	in.config(leadsCSV + "ingest_chunk_rows: 2\n")
	var noPush []string
	hooks := DefaultHooks()
	hooks.Push = func(r *Run) error { noPush = append(noPush, r.NoPush); return nil }

	wantMerged := []string{"2 input row(s) merged, 3 left", "2 input row(s) merged, 1 left", "1 input row(s) merged, 0 left", "0 input row(s) merged, 0 left"}
	for i, want := range wantMerged {
		res, out, err := in.run(hooks)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("run %d: %v %s", i+1, err, out)
		}
		if backlog := i < 2; backlog != hasKey(res.Problems, "ingest_backlog") {
			t.Errorf("run %d problems %v", i+1, res.Problems)
		}
		if i == 0 {
			// Group a (two rows, lines 1 and 3) was taken whole, before b.
			applied := map[string]bool{}
			for _, r := range in.rows(model.TableAppliedRows) {
				applied[r["row_id"]] = true
			}
			if len(applied) != 1 || !applied["a@x.example"] {
				t.Errorf("run 1 applied %v, want the whole group a", applied)
			}
		}
	}
	if len(noPush) != 4 || noPush[0] == "" || noPush[1] == "" || noPush[2] != "" || noPush[3] != "" {
		t.Errorf("NoPush per run %q: set while a backlog remains, clear after", noPush)
	}
	if n := len(in.rows(model.TablePeople)); n != 4 {
		t.Errorf("People %d, want 4 (a's two rows are one lead)", n)
	}
}

func TestCursorSavedOnlyWhenEveryRowWasTaken(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: feed, type: stub }\n", testRubric)
	in.config("sources:\n  - { id: feed, type: stub }\ningest_chunk_rows: 1\n")
	row := func(email string) api.InputRow {
		return api.InputRow{Headers: []string{"Email"}, Columns: map[string]string{"Email": email}}
	}
	out := &stubOut{rows: []api.InputRow{row("a@x.example"), row("b@x.example")}, next: "c2"}
	setStub(t, "feed", out)
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	if in.state("cursor:feed") != "" {
		t.Error("a source with rows left over keeps its cursor")
	}
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	if in.state("cursor:feed") != "c2" {
		t.Errorf("cursor %q after every row was taken", in.state("cursor:feed"))
	}
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	if got := out.seen; len(got) != 3 || got[0] != "" || got[1] != "" || got[2] != "c2" {
		t.Errorf("Fetch cursors %q", got)
	}
}

// A source that fails its Fetch is skipped: its cursor is kept, the rest of
// the run goes on, the run is unhealthy, and pushing waits.
func TestFailedSourceIsSkipped(t *testing.T) {
	in := basicInstall(t)
	in.config(leadsCSV + "  - { id: broken, type: stub }\n")
	setStub(t, "broken", &stubOut{err: errors.New("line 3: bad quote"), next: "never"})
	var noPush string
	hooks := DefaultHooks()
	hooks.Push = func(r *Run) error { noPush = r.NoPush; return nil }
	res, _, err := in.run(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy || !hasKey(res.Problems, "source_failed:broken") || !strings.Contains(noPush, "broken") {
		t.Errorf("result %+v NoPush %q", res, noPush)
	}
	if len(in.rows(model.TablePeople)) != 2 || in.state("cursor:broken") != "" {
		t.Error("the other source must be merged and the broken one's cursor kept")
	}
	// Fixed: the problem clears at the next run.
	setStub(t, "broken", &stubOut{})
	res, _, _ = in.run(DefaultHooks())
	if hasKey(res.Problems, "source_failed:broken") || in.health()["problem:source_failed:broken"] != "" {
		t.Error("a resolved problem must be cleared from Health")
	}
}

// Source events reach Intake once, with their keys normalized.
func TestSourceEventsAreNormalizedForIntake(t *testing.T) {
	in := newInstall(t, "sources:\n  - { id: site, type: stub }\n", testRubric)
	setStub(t, "site", &stubOut{events: []api.Event{{
		Kind: "visit_site", Email: " Ana@ACME.Example", LinkedInURL: "https://uk.linkedin.com/in/Ana-A/en?x=1",
		Domain: "https://www.Acme.example:443/pricing", At: time.Now(),
	}}})
	var got []api.Event
	hooks := DefaultHooks()
	hooks.Intake = func(r *Run) error { got = r.SourceEvents; return nil }
	if _, _, err := in.run(hooks); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events %v", got)
	}
	e := got[0]
	if e.Email != "ana@acme.example" || e.LinkedInURL != "linkedin.com/in/ana-a" || e.Domain != "acme.example" || e.Origin != "site" {
		t.Errorf("event keys not normalized: %+v", e)
	}
}

// The merge index feeds LeadRef: a person seen on two channels counts two.
func TestSourcesSeenReachScoring(t *testing.T) {
	in := basicInstall(t)
	in.write("more.csv", csvText("Work Email,Title", "ana@acme.example,Head of Ops"))
	in.config(leadsCSV + "  - { id: more, type: csv, path: more.csv }\n")
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	for _, r := range in.rows(model.TableRanked) {
		want := "0"
		if r["email"] == "ana@acme.example" {
			want = "15" // 5 for the title, 10 for a second channel
		}
		if r["contact_score"] != want {
			t.Errorf("%s contact_score %s, want %s", r["email"], r["contact_score"], want)
		}
	}
}

// Before scoring, a rubric field no input carries fails the run; a column
// whose every cell is empty still counts as loaded.
func TestUnknownRubricFieldFailsTheRun(t *testing.T) {
	reads := strings.Replace(testRubric, "{ field: title, contains: head }\n      then: 1", "{ field: budget, present: true }\n      then: 1", 1)
	in := basicInstall(t)
	in.write("rubric.yml", reads)
	res, _, err := in.run(DefaultHooks())
	if err == nil || !strings.Contains(err.Error(), "budget") || !hasKey(res.Problems, "rubric_unknown_field:budget") {
		t.Fatalf("got %+v %v", res, err)
	}
	if len(in.rows(model.TablePeople)) != 0 {
		t.Error("a failed run saves no merge")
	}
	if h := in.health(); h["result:last_result"] != "unhealthy" || h["problem:rubric_unknown_field:budget"] == "" || h["problem:run_failed"] == "" {
		t.Errorf("Health %v", h)
	}
	in.write("leads.csv", csvText("Email,Name,Title,Budget", "ana@acme.example,Ana A,Head,", "bo@acme.example,Bo B,Clerk,"))
	if res, _, err := in.run(DefaultHooks()); err != nil || !res.Healthy {
		t.Fatalf("an empty Budget column is still loaded: %+v %v", res, err)
	}
	if in.health()["problem:run_failed"] != "" {
		t.Error("run_failed must clear once a run finishes")
	}
}

// Each lane's sink must be registered; an export lane needs none.
func TestLaneSinksAreChecked(t *testing.T) {
	in := basicInstall(t)
	in.write("rubric.yml", testRubric+
		"  - { id: seq, kind: cold, when: { field: receiver_only, eq: false }, push: \"nosuchsink:x\" }\n"+
		"  - { id: ok, kind: non-cold, when: { field: tier, eq: 1 }, push: \"testsink:x\" }\n")
	res, _, err := in.run(DefaultHooks())
	if err != nil {
		t.Fatal(err)
	}
	if !hasKey(res.Problems, "lane_sink_unregistered:seq") || hasKey(res.Problems, "lane_sink_unregistered:ok") ||
		hasKey(res.Problems, "lane_sink_unregistered:list") || res.Healthy {
		t.Errorf("problems %v", res.Problems)
	}
}

// ErrTooLarge on phase 1: reload and redo with half the rows; a second one
// saves nothing of the chunk and stops pushing.
func TestErrTooLargeHalvesThenStopsPushing(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head", "b@x.example,B,C", "c@x.example,C,C", "d@x.example,D,C"))
	in.config("store: { type: flaky, path: leadscore.db }\n" + leadsCSV + "ingest_chunk_rows: 4\n")

	flaky.Lock()
	flaky.tooLarge = 1
	flaky.Unlock()
	res, out, err := in.run(DefaultHooks())
	if err != nil || !strings.Contains(out, "2 input row(s) merged, 2 left") {
		t.Fatalf("halved redo: %+v %v %s", res, err, out)
	}
	if len(in.rows(model.TablePeople)) != 2 {
		t.Error("the redo must save half the rows")
	}

	flaky.Lock()
	flaky.tooLarge = 2
	flaky.Unlock()
	var noPush string
	pushed := false
	hooks := DefaultHooks()
	hooks.Push = func(r *Run) error { pushed = true; noPush = r.NoPush; return nil }
	res, _, err = in.run(hooks)
	if err != nil || res.Healthy || !hasKey(res.Problems, "commit_too_large") || pushed {
		t.Fatalf("second ErrTooLarge: %+v %v pushed %v %q", res, err, pushed, noPush)
	}
	if len(in.rows(model.TablePeople)) != 2 {
		t.Error("nothing of the refused chunk may be saved")
	}
	if !strings.Contains(in.health()["problem:commit_too_large"], "ingest_chunk_rows") {
		t.Error("Health must name the setting to lower")
	}
}

func TestHookErrors(t *testing.T) {
	t.Run("events shrank: score and save, no push", func(t *testing.T) {
		in := basicInstall(t)
		var noPush string
		hooks := DefaultHooks()
		hooks.Intake = func(*Run) error { return api.ErrEventsShrank }
		hooks.Push = func(r *Run) error { noPush = r.NoPush; return nil }
		res, _, err := in.run(hooks)
		if err != nil || noPush == "" || !hasKey(res.Problems, "events_shrank") || len(in.rows(model.TableRanked)) != 2 {
			t.Errorf("%+v %v %q", res, err, noPush)
		}
	})
	t.Run("intake fails the run before phase 1", func(t *testing.T) {
		in := basicInstall(t)
		hooks := DefaultHooks()
		hooks.Intake = func(*Run) error { return errors.New("bad body") }
		res, _, err := in.run(hooks)
		if err == nil || res.Healthy || len(in.rows(model.TablePeople)) != 0 || in.health()["problem:run_failed"] == "" {
			t.Errorf("%+v %v", res, err)
		}
	})
	t.Run("fold fails the run before phase 1", func(t *testing.T) {
		in := basicInstall(t)
		hooks := Hooks{Fold: func(*Run) error { return errors.New("fold broke") }}
		if _, _, err := in.run(hooks); err == nil || len(in.rows(model.TablePeople)) != 0 {
			t.Errorf("err %v", err)
		}
	})
	t.Run("enrich, detect and export only make the run unhealthy", func(t *testing.T) {
		in := basicInstall(t)
		hooks := DefaultHooks()
		hooks.Enrich = func(*Run) error { return errors.New("apollo down") }
		hooks.Detect = func(*Run) (rules.DetectorResults, error) {
			return rules.DetectorResults{}, errors.New("detector broke")
		}
		hooks.Export = func(*Run) error { return errors.New("export broke") }
		res, _, err := in.run(hooks)
		if err != nil || res.Healthy || len(in.rows(model.TableRanked)) != 2 {
			t.Fatalf("%+v %v", res, err)
		}
		for _, k := range []string{"step_failed:enrich", "step_failed:detect", "step_failed:export"} {
			if !hasKey(res.Problems, k) {
				t.Errorf("missing %s in %v", k, res.Problems)
			}
		}
	})
	t.Run("a prepush error skips push", func(t *testing.T) {
		in := basicInstall(t)
		pushed := false
		hooks := DefaultHooks()
		hooks.PrePush = func(*Run, []api.LeadID) error { return errors.New("lookup down") }
		hooks.Push = func(*Run) error { pushed = true; return nil }
		res, _, err := in.run(hooks)
		if err != nil || pushed || !hasKey(res.Problems, "step_failed:prepush") {
			t.Errorf("%+v %v pushed %v", res, err, pushed)
		}
	})
	t.Run("hooks raise problems through Run.Problem", func(t *testing.T) {
		in := basicInstall(t)
		hooks := DefaultHooks()
		hooks.Push = func(r *Run) error { r.Problem("push_failed:x:y:z", "it failed", "retry", false); return nil }
		res, _, _ := in.run(hooks)
		if res.Healthy || !strings.Contains(in.health()["problem:push_failed:x:y:z"], "it failed. Fix: retry.") {
			t.Errorf("%+v %v", res, in.health())
		}
	})
}

func TestSkippedRunsAreFlagged(t *testing.T) {
	in := basicInstall(t)
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	old := model.FormatTime(time.Now().Add(-31 * time.Minute)) // more than two 15m intervals
	if err := in.store().Commit(t.Context(), []api.TableWrite{{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"},
		Rows: []api.Row{{"kind": "result", "key": "last_run_at", "value": old}}}}); err != nil {
		t.Fatal(err)
	}
	res, _, err := in.run(DefaultHooks())
	if err != nil || !hasKey(res.Problems, "skipped_runs") || !res.Healthy {
		t.Errorf("skipped runs are a warning: %+v %v", res, err)
	}
	if res, _, _ := in.run(DefaultHooks()); hasKey(res.Problems, "skipped_runs") {
		t.Error("back on schedule, the warning clears")
	}
}

func TestTierChangesAreLogged(t *testing.T) {
	in := basicInstall(t)
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	// Clerks become tier 1, and a new lead arrives.
	in.write("rubric.yml", strings.Replace(testRubric, "contains: head }\n      then: 1", "contains: clerk }\n      then: 1", 1))
	in.write("leads.csv", csvText("Email,Name,Title", "ana@acme.example,Ana A,Head of Ops", "bo@acme.example,Bo B,Clerk", "cy@acme.example,Cy C,Clerk"))
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "tier_change" {
			changes = append(changes, r["email"]+": "+r["message"])
		}
	}
	got := strings.Join(changes, " | ")
	if got != "ana@acme.example: tier 1 -> 2 | bo@acme.example: tier 2 -> 1" && got != "bo@acme.example: tier 2 -> 1 | ana@acme.example: tier 1 -> 2" {
		t.Errorf("tier changes %q; a lead with no Ranked row (cy) is never logged", got)
	}
}

func TestCloudRunRefusesLocalFiles(t *testing.T) {
	in := basicInstall(t)
	_, _, err := in.run(DefaultHooks(), func(_ *api.RunOptions, s *settings) {
		s.getenv = func(k string) string {
			if k == "CLOUD_RUN_JOB" {
				return "leadscore-run"
			}
			return ""
		}
	})
	if err == nil || !strings.Contains(err.Error(), "SQLite") || !strings.Contains(err.Error(), "source leads") {
		t.Fatalf("got %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(in.dir, "leadscore.db")); len(m) != 0 {
		t.Error("the refused run must not open the store")
	}
}

// A rejected row is recorded through Apply (Applied rows with no lead, and a
// row_rejected Log line), never dropped before it.
func TestRejectedRowsAreRecorded(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "ana@acme.example,Ana A,Head", "not-an-email,Bo B,Clerk"))
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	rejected := 0
	for _, r := range in.rows(model.TableAppliedRows) {
		if r["lead_id"] == "" {
			rejected++
		}
	}
	logged := false
	for _, r := range in.rows(model.TableLog) {
		logged = logged || r["kind"] == "row_rejected"
	}
	if rejected != 1 || !logged || len(in.rows(model.TablePeople)) != 1 {
		t.Errorf("rejected rows %d, logged %v", rejected, logged)
	}
}

// A commit the store saved but flagged (ErrCommittedWithProblems) is done:
// the run carries on without resending it (a resend would fail on keys
// already appended) and shows the store's message in Health as a warning.
func TestCommittedWithProblems(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head", "b@x.example,B,C"))
	in.config("store: { type: flaky, path: leadscore.db }\n" + leadsCSV)
	flaky.Lock()
	flaky.savedWithProblems = 1
	flaky.Unlock()
	res, _, err := in.run(DefaultHooks())
	if err != nil || !res.Healthy || !hasKey(res.Problems, "people_tab_check") {
		t.Fatalf("run = %+v, %v; want a healthy run raising people_tab_check", res, err)
	}
	if len(in.rows(model.TablePeople)) != 2 {
		t.Error("the flagged commit's rows must be saved once")
	}
	if !strings.Contains(in.health()["problem:people_tab_check"], "Overrides") {
		t.Errorf("Health = %q; want the store's message", in.health()["problem:people_tab_check"])
	}
}
