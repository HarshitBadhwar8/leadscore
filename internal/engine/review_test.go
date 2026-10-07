package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func setFlaky(tooLarge, failRanked int) {
	flaky.Lock()
	flaky.tooLarge, flaky.failRanked = tooLarge, failRanked
	flaky.Unlock()
}

// A source's cursor is saved only once its events reached Intake: not when
// the deadline cut the run short during the source reads, and not when no
// Intake ran. A source with no events saves its cursor with its rows.
func TestEventSourceCursorWaitsForIntake(t *testing.T) {
	shrink(t, 10*time.Second)
	in := newInstall(t, "sources:\n  - { id: site, type: stub }\n  - { id: rows, type: stub }\ndeadline: 200ms\n", testRubric)
	setStub(t, "site", &stubOut{next: "e2", events: []api.Event{{Kind: "visit_site", Email: "a@x.example", At: time.Now()}}})
	setStub(t, "rows", &stubOut{delay: 400 * time.Millisecond, next: "r2", rows: []api.InputRow{{Headers: []string{"Email"}, Columns: map[string]string{"Email": "a@x.example"}}}})

	// The deadline passes during the source reads.
	hooks := Hooks{Intake: func(*Run) error { t.Error("Intake must not run after the deadline"); return nil }}
	res, _, err := in.run(hooks)
	if err != nil || !hasKey(res.Problems, "deadline_passed") {
		t.Fatalf("%+v %v", res, err)
	}
	if in.state("cursor:site") != "" || in.state("cursor:rows") != "r2" {
		t.Errorf("deadline in the source reads: cursor:site %q (must stay), cursor:rows %q", in.state("cursor:site"), in.state("cursor:rows"))
	}

	// No Intake hook: the events were never taken.
	stubMu.Lock()
	stubData["rows"].delay = 0
	stubMu.Unlock()
	in.config("sources:\n  - { id: site, type: stub }\n  - { id: rows, type: stub }\n")
	if _, _, err := in.run(Hooks{}); err != nil {
		t.Fatal(err)
	}
	if in.state("cursor:site") != "" {
		t.Error("with no Intake the event source's cursor must stay")
	}

	// Intake fails with ErrEventsShrank: the cursor stays too.
	if _, _, err := in.run(Hooks{Intake: func(*Run) error { return api.ErrEventsShrank }}); err != nil {
		t.Fatal(err)
	}
	if in.state("cursor:site") != "" {
		t.Error("events Intake did not take keep their cursor")
	}

	// Intake takes them: the cursor moves.
	if _, _, err := in.run(Hooks{Intake: func(*Run) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if in.state("cursor:site") != "e2" {
		t.Errorf("cursor:site %q after Intake took the events", in.state("cursor:site"))
	}
}

// The ErrTooLarge redo halves the rows taken, not the configured chunk.
func TestErrTooLargeHalvesTheRowsTaken(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head", "b@x.example,B,C", "c@x.example,C,C", "d@x.example,D,C"))
	in.config("store: { type: flaky, path: leadscore.db }\n" + leadsCSV) // chunk 2000, 4 rows pending
	setFlaky(1, 0)
	res, out, err := in.run(DefaultHooks())
	if err != nil || !strings.Contains(out, "2 input row(s) merged, 2 left") {
		t.Fatalf("%+v %v %s", res, err, out)
	}
}

// Run carries what later slices need: ReRead (calling the hook, its error
// making the run unhealthy), Input and Result, and Pushed for RunResult.
func TestRunValueServesThePushLoop(t *testing.T) {
	in := basicInstall(t)
	hooks := DefaultHooks()
	reread := 0
	hooks.ReRead = func(r *Run) ([]api.LeadID, error) {
		reread++
		if reread == 2 {
			return nil, errors.New("events unreadable")
		}
		return []api.LeadID{r.Input.Leads[0].ID}, nil
	}
	var changed []api.LeadID
	hooks.Push = func(r *Run) error {
		if len(r.Input.Leads) != 2 || len(r.Result.Verdicts) != 2 {
			t.Errorf("Input %d leads, Result %d verdicts", len(r.Input.Leads), len(r.Result.Verdicts))
		}
		changed, _ = r.ReRead()
		_, _ = r.ReRead()
		r.Pushed = 3
		return nil
	}
	res, err := RunWith(context.Background(), in.opts(), hooks, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || res.Pushed != 3 || res.Healthy || !hasKey(res.Problems, "step_failed:reread") {
		t.Errorf("changed %v result %+v", changed, res)
	}
	// With no ReRead hook, Run.ReRead returns nothing.
	hooks.ReRead = nil
	hooks.Push = func(r *Run) error {
		if c, err := r.ReRead(); c != nil || err != nil {
			t.Errorf("no hook: %v %v", c, err)
		}
		return nil
	}
	if _, err := RunWith(context.Background(), in.opts(), hooks, time.Now, nil); err != nil {
		t.Fatal(err)
	}
}

// A panicking hook fails the run with run_failed written under the lease,
// and the lease is still released.
func TestPanicIsRecordedAsRunFailed(t *testing.T) {
	in := basicInstall(t)
	hooks := DefaultHooks()
	hooks.Detect = func(*Run) (rules.DetectorResults, error) { panic("detector bug") }
	res, _, err := in.run(hooks)
	if err == nil || !strings.Contains(err.Error(), "panic: detector bug") || res.Healthy {
		t.Fatalf("%+v %v", res, err)
	}
	if h := in.health(); !strings.Contains(h["problem:run_failed"], "detector bug") || h["result:last_result"] != "unhealthy" {
		t.Errorf("Health %v", h)
	}
	if in.state("lease_owner") != "" {
		t.Error("the lease must be released")
	}
}

// LEADSCORE_CONFIG_VERSION (set by the hosted deploy) is written to
// State.config_version; unset, the stored value is kept.
func TestConfigVersionFromEnvironment(t *testing.T) {
	in := basicInstall(t)
	withVersion := func(v string) func(*api.RunOptions, *settings) {
		return func(_ *api.RunOptions, s *settings) {
			s.getenv = func(k string) string {
				if k == "LEADSCORE_CONFIG_VERSION" {
					return v
				}
				return ""
			}
		}
	}
	if _, _, err := in.run(DefaultHooks(), withVersion("7")); err != nil {
		t.Fatal(err)
	}
	if in.state("config_version") != "7" {
		t.Errorf("config_version %q", in.state("config_version"))
	}
	if _, _, err := in.run(DefaultHooks(), withVersion("")); err != nil {
		t.Fatal(err)
	}
	if in.state("config_version") != "7" {
		t.Error("an unset variable must leave config_version alone")
	}
}

// Everything merge writes because a row was applied is saved with the row
// in phase 1: a run that loses the lease after phase 1 still leaves the
// row's company fact, so the next run (which skips the applied row) has it.
func TestCompanyFactsSaveWithTheirRows(t *testing.T) {
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Company Domain,Employees", "ana@acme.example,Ana A,acme.example,420"))
	hooks := DefaultHooks()
	hooks.Push = func(*Run) error { in.setLease("successor", time.Now().Add(-time.Second)); return nil }
	if _, _, err := in.run(hooks); !errors.Is(err, api.ErrLeaseLost) {
		t.Fatalf("run 1 must lose the lease after phase 1: %v", err)
	}
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	facts := in.rows(model.TableCompanyFacts)
	if len(facts) != 1 || !strings.Contains(facts[0]["facts"], `"employees"`) || !strings.Contains(facts[0]["facts"], "420") {
		t.Errorf("Company facts %v", facts)
	}
}

// A run cut short before its checks ran keeps the problems it could not
// re-check, with their first_seen_at.
func TestCutShortRunKeepsProblems(t *testing.T) {
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
	if _, _, err := in.run(hooks); err != nil {
		t.Fatal(err)
	}
	if got := in.problemSince("source_failed:broken"); got == "" || got != first {
		t.Errorf("a cut-short run dropped or reset a problem it never re-checked: %q, first seen %q", got, first)
	}
}

// A failed run keeps the problems it did not get to re-check.
func TestFailedRunKeepsOtherProblems(t *testing.T) {
	in := basicInstall(t)
	in.config(leadsCSV + "  - { id: broken, type: stub }\n")
	setStub(t, "broken", &stubOut{err: errors.New("bad file")})
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	setStub(t, "broken", &stubOut{})
	hooks := DefaultHooks()
	hooks.Fold = func(*Run) error { return errors.New("fold broke") }
	if _, _, err := in.run(hooks); err == nil {
		t.Fatal("the run must fail")
	}
	h := in.health()
	if h["problem:source_failed:broken"] == "" || h["problem:run_failed"] == "" || h["problem:step_failed:fold"] == "" {
		t.Errorf("Health %v", h)
	}
}

// A Ranked write that fails twice fails the run (run_failed); the tier
// change lines it carried are not saved, so the next run logs them once.
func TestRankedWriteFailureFailsTheRun(t *testing.T) {
	in := basicInstall(t)
	in.config("store: { type: flaky, path: leadscore.db }\n" + leadsCSV)
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	in.write("rubric.yml", strings.Replace(testRubric, "contains: head }\n      then: 1", "contains: clerk }\n      then: 1", 1))
	setFlaky(0, 2)
	res, _, err := in.run(DefaultHooks())
	if err == nil || res.Healthy || !strings.Contains(in.health()["problem:run_failed"], "Ranked") {
		t.Fatalf("%+v %v %v", res, err, in.health())
	}
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "tier_change" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d tier_change lines, want 2 (each change once)", n)
	}
}

func TestAfterSaveFailureIsRecorded(t *testing.T) {
	in := basicInstall(t)
	hooks := DefaultHooks()
	ran := 0
	hooks.AfterSave = Chain(
		func(*Run) error { ran++; return errors.New("view write failed") },
		func(*Run) error { ran++; return nil },
	)
	res, _, err := in.run(hooks)
	if err != nil || res.Healthy || ran != 2 || !strings.Contains(in.health()["problem:step_failed:aftersave"], "view write failed") ||
		in.health()["result:last_result"] != "unhealthy" {
		t.Errorf("%+v %v ran %d %v", res, err, ran, in.health())
	}
}

func TestRankedIsWrittenInChunks(t *testing.T) {
	old := rankedChunkRows
	rankedChunkRows = 1
	t.Cleanup(func() { rankedChunkRows = old })
	in := basicInstall(t)
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head", "b@x.example,B,C", "c@x.example,C,C"))
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	if n := len(in.rows(model.TableRanked)); n != 3 {
		t.Errorf("Ranked %d rows", n)
	}
	in.write("leads.csv", csvText("Email,Name,Title", "a@x.example,A,Head"))
	// A lead never leaves People, so all three stay ranked; the rewrite
	// replaces the table rather than appending to it.
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	if n := len(in.rows(model.TableRanked)); n != 3 {
		t.Errorf("Ranked %d rows after a rewrite in chunks", n)
	}
}

func TestLogIsTrimmed(t *testing.T) {
	in := basicInstall(t)
	in.config(leadsCSV + "log_retention: 1d\n")
	old := model.FormatTime(time.Now().Add(-48 * time.Hour))
	if err := in.store().Commit(context.Background(), []api.TableWrite{{Table: model.TableLog, Op: api.OpAppend,
		Rows: []api.Row{{"at": old, "kind": "old_line"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] == "old_line" {
			t.Error("a Log line older than log_retention must be trimmed")
		}
	}
}

// Two runs on a new SQLite file at once: neither fails opening it; at most
// one holds the lease at a time.
func TestConcurrentRunsOnANewStore(t *testing.T) {
	in := basicInstall(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]api.RunResult, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = execute(context.Background(), in.opts(),
				settings{hooks: DefaultHooks(), now: time.Now, getenv: func(string) string { return "" }})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
	if len(in.rows(model.TablePeople)) != 2 {
		t.Error("the runs must have merged the two leads once")
	}
}

func (in *install) problemSince(key string) string {
	for _, r := range in.rows(model.TableHealth) {
		if r["kind"] == "problem" && r["key"] == key {
			return r["first_seen_at"]
		}
	}
	return ""
}

// The in-run rubric check judges the rubric the run is scoring with, not the
// file as edited mid-run.
func TestRubricCheckUsesTheRunsRubric(t *testing.T) {
	in := basicInstall(t)
	hooks := DefaultHooks()
	hooks.Intake = func(*Run) error { in.write("rubric.yml", "version: 1\nlanes: [ { kind: cold } ]\n"); return nil }
	res, _, err := in.run(hooks)
	if err != nil || !res.Healthy || hasKey(res.Problems, "rubric_invalid:compile") {
		t.Errorf("%+v %v", res, err)
	}
}

// RunWithOutput with a nil writer discards the summary instead of failing.
func TestRunWithOutputNilDiscards(t *testing.T) {
	setStub(t, "leads", &stubOut{rows: []api.InputRow{{SourceID: "leads", Headers: []string{"Email", "Title"},
		Columns: map[string]string{"Email": "ana@acme.example", "Title": "Head of Ops"}}}})
	in := newInstall(t, "sources:\n  - { id: leads, type: stub }\n", testRubric)
	res, err := RunWithOutput(context.Background(), in.opts(), DefaultHooks(), time.Now, nil, nil)
	if err != nil || !res.Healthy {
		t.Fatalf("a run with no output writer: %v %+v", err, res)
	}
	if n := len(in.rows(model.TableRanked)); n != 1 {
		t.Errorf("%d Ranked rows, want 1", n)
	}
}
