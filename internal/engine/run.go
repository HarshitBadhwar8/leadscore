package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// configVersionVar carries the hosted bundle's secret version number; S14b's
// deploy sets it (contracts section 3).
const configVersionVar = "LEADSCORE_CONFIG_VERSION"

// exec is one run in progress.
type exec struct {
	s     settings
	opts  api.RunOptions
	cfg   *config.Config
	store api.Backend
	run   *Run

	startAt  time.Time       // the run's clock at start: last_run_at
	dlCtx    context.Context // done at the deadline only
	takeover string          // the owner of an expired lease this run took over

	mu       sync.Mutex
	problems map[string]problem

	// Per attempt (an ErrTooLarge redo starts these again).
	columns      []string // raw input headers fetched this run
	merged       int      // input rows merged this run
	backlog      int      // input rows left for later runs
	cursors      []cursorSet
	scored       bool
	phase1Failed bool
	cutShort     bool // the deadline or Stop came before the run finished its steps
	oldRanked    map[model.Key]model.RankedRow
	tierLogs     []model.LogEntry // written with Ranked, so a failed Ranked write never logs a change twice

	failed bool // the run stopped with an error
	lost   bool // the lease was lost: write nothing more
}

type problem struct {
	key, message, fix string
	warning           bool
}

// cursorSet is a source cursor to save in phase 1: at once for a source with
// no events, after Intake took its events otherwise.
type cursorSet struct {
	source    string
	next      api.Cursor
	hasEvents bool
}

// execute runs once. The error is non-nil when the run failed; a run that
// finished unhealthy returns a nil error with Healthy false.
func execute(ctx context.Context, opts api.RunOptions, s settings) (api.RunResult, error) {
	if s.now == nil {
		s.now = time.Now
	}
	if s.out == nil {
		s.out = io.Discard
	}
	if s.getenv == nil {
		s.getenv = func(string) string { return "" }
	}
	// Step 1: leadscore.yml and the rubric are read fresh every run.
	cfg, err := config.Load(config.Options{ConfigPath: opts.ConfigPath, RubricPath: opts.RubricPath})
	if err != nil {
		return api.RunResult{}, err
	}
	if ps := check.CloudRunRefusal(cfg, s.getenv); len(ps) > 0 {
		return api.RunResult{}, fmt.Errorf("%s (%s)", ps[0].Message, ps[0].Fix)
	}
	withTestClient(cfg, s.client)
	text, err := cfg.Rubric()
	if err != nil {
		return api.RunResult{}, err
	}
	rubric, err := rules.Compile(text)
	if err != nil {
		return api.RunResult{}, fmt.Errorf("the rubric does not compile (leadscore rules check lists every error): %w", err)
	}

	var store api.Backend
	var events api.EventLog
	if opts.DryRun && cfg.Store.Type == "sqlite" && missing(cfg.Store.Path) {
		// A dry run on a fresh install scores against an empty store rather
		// than creating the file.
		fmt.Fprintf(s.out, "dry run: no store yet at %s; scoring as on a first run\n", cfg.Store.Path)
		store, events = emptyStore{}, emptyStore{}
	} else {
		open, ok := api.BackendFactory(cfg.Store.Type)
		if !ok {
			return api.RunResult{}, fmt.Errorf("store type %q is not registered in this build", cfg.Store.Type)
		}
		store, events, err = open(cfg.Store.Block)
		if err != nil {
			return api.RunResult{}, fmt.Errorf("opening the store: %w", err)
		}
		if c, ok := store.(io.Closer); ok {
			defer c.Close()
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return api.RunResult{}, err
	}
	runID := id.String()

	// The deadline, the save budget after it, and the hard stop 30 seconds
	// before the lease expires. Closing Stop starts the save budget early.
	// Timers use the real clock.
	deadlineAt := time.Now().Add(cfg.Deadline)
	hardAt := deadlineAt.Add(saveBudget)
	runCtx, cancelRun := context.WithDeadline(ctx, hardAt)
	defer cancelRun()
	dlCtx, cancelDL := context.WithDeadline(runCtx, deadlineAt)
	defer cancelDL()
	pushCtx, cancelPush := context.WithCancel(dlCtx)
	defer cancelPush()
	if opts.Stop != nil {
		go func() {
			select {
			case <-opts.Stop:
				cancelPush()
				t := time.AfterFunc(min(saveBudget, time.Until(hardAt)), cancelRun)
				<-runCtx.Done()
				t.Stop()
			case <-pushCtx.Done():
			}
		}()
	}

	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	x := &exec{
		s: s, opts: opts, cfg: cfg, store: store,
		startAt: s.now().UTC(), dlCtx: dlCtx,
		problems: map[string]problem{},
	}
	x.run = &Run{
		ID: runID, Ctx: runCtx, PushCtx: pushCtx,
		Config: cfg, Rubric: rubric, Store: store, Events: events,
		DryRun: opts.DryRun, Now: func() time.Time { return s.now().UTC() }, HTTPClient: client,
		Problem: x.problem,
	}
	x.run.ReRead = x.reRead

	if !opts.DryRun {
		if li, ok := store.(api.LeaseInspector); ok {
			if owner, exp, err := li.LeaseInfo(runCtx); err == nil && owner != "" && !exp.After(time.Now()) {
				x.takeover = owner
			}
		}
		lease, err := store.Lease(runCtx, runID, cfg.Deadline+saveBudget+leaseMargin)
		if errors.Is(err, api.ErrLeaseHeld) {
			// A skipped run writes nothing; the next run that holds the lease
			// flags repeated skips.
			fmt.Fprintf(s.out, "run %s: skipped: another run holds the lease\n", runID)
			return api.RunResult{Healthy: true, Skipped: true}, nil
		}
		if err != nil {
			return api.RunResult{}, fmt.Errorf("taking the run lease: %w", err)
		}
		x.run.Lease = lease
		defer x.release(ctx)
	}

	// Step 2: load the tables (the schema version is checked here).
	m, err := codec.Load(runCtx, store)
	if err != nil {
		return x.finish(fmt.Errorf("loading the store: %w", err))
	}
	x.run.Model = m
	return x.finish(x.safeMain())
}

func missing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// safeMain is main with a panic turned into the run's error, so a panicking
// hook still writes run_failed under the lease (RFC 6.9).
func (x *exec) safeMain() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return x.main()
}

// release gives the lease up, only if this run still holds it. It runs after
// the hard stop too: releasing is owner-only, so it cannot touch a successor.
func (x *exec) release(parent context.Context) {
	if x.lost || x.run.Lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if err := x.run.Lease.Release(ctx); err != nil && !errors.Is(err, api.ErrLeaseLost) {
		fmt.Fprintf(x.s.out, "run %s: releasing the lease: %s\n", x.run.ID, logredact.Redact(err.Error()))
	}
}

// main is steps 2 to 10 once the lease is held and the model loaded.
func (x *exec) main() error {
	r := x.run
	chunk := x.cfg.IngestChunkRows
	for attempt := 1; ; attempt++ {
		if err := x.beforePhase1(chunk); err != nil {
			return err
		}
		if r.DryRun {
			break
		}
		err := x.commit("phase 1", codec.Encode(r.Model, phase1Tables...), true)
		if err == nil {
			break
		}
		if !errors.Is(err, api.ErrTooLarge) {
			return err
		}
		if attempt == 1 {
			// Discard, reload, and redo steps 3 to 6 with half the rows taken.
			m, err := codec.Load(r.Ctx, x.store)
			if err != nil {
				return fmt.Errorf("reloading the store after a commit too large: %w", err)
			}
			r.Model = m
			chunk = max(1, x.merged/2)
			continue
		}
		// A second ErrTooLarge: nothing of steps 3 to 6 is saved this run.
		r.Model.Discard()
		x.scored, x.merged = false, 0
		r.Input, r.Result = rules.Input{}, rules.Result{}
		x.phase1Failed, x.cutShort = true, true
		x.noPush("phase 1 was too large to save, even at half the rows")
		x.problem("commit_too_large", fmt.Sprintf("the first save of this run was too large for the store even at %d input rows; nothing it merged was saved", chunk),
			"lower ingest_chunk_rows in leadscore.yml", false)
		break
	}

	if !x.phase1Failed {
		x.push()
	}
	if x.s.hooks.Export != nil {
		if err := x.s.hooks.Export(r); err != nil {
			x.hookFailed("export", err)
		}
	}
	if r.DryRun {
		x.report()
		return nil
	}

	// Phase 2: everything else but Ranked, with the retention trims (Log,
	// Window events, Seen events) and Health.
	r.Model.Trim(model.TableLog, "at", r.Now().Add(-x.cfg.LogRetention))
	trimEvents(r)
	x.putHealth(!x.cutShort)
	if err := x.commit("phase 2", codec.Encode(r.Model, phase2Tables(r.Model)...), false); err != nil {
		return err
	}
	if x.scored {
		if err := x.writeRanked(); err != nil {
			return fmt.Errorf("writing Ranked: %w", err)
		}
	}
	if x.s.hooks.AfterSave != nil {
		if err := x.s.hooks.AfterSave(r); err != nil {
			x.lateProblem("step_failed:aftersave", "the step after saving failed: "+err.Error(), "see the message; the next run tries again")
		}
	}
	return nil
}

// Phase 1 tables (contracts section 12.6): with the keys and cursors, every
// change merge makes because a row was applied (Company facts, its Log lines
// and the key_conflicts count), so no crash leaves a row applied without them.
var phase1Tables = []string{
	model.TablePeople, model.TableIdentities, model.TableAppliedRows, model.TableCompanyFacts,
	model.TableSeenEvents, model.TableWindowEvents, model.TableOutcomes, model.TablePushes,
	model.TableAppliedOverrides, model.TableLog,
	model.TableState + ":cursor:", model.TableState + ":last_poll_at", model.TableState + ":first_run_at",
	model.TableState + ":key_conflicts", model.TableState + ":config_version",
}

// phase2Tables is every table the run writes but Ranked (written after) and
// the people-owned Overrides (never written by a run).
func phase2Tables(m *model.Model) []string {
	var out []string
	for _, d := range model.Tables {
		if d.Pattern || d.Name == model.TableRanked || d.Name == model.TableOverrides {
			continue
		}
		out = append(out, d.Name)
	}
	lanes := map[string]bool{}
	for lane := range m.Exports {
		lanes[lane] = true
	}
	for k := range m.State {
		if lane, ok := strings.CutPrefix(string(k), model.ExportLaneKey); ok && lane != "" {
			lanes[lane] = true
		}
	}
	names := make([]string, 0, len(lanes))
	for lane := range lanes {
		names = append(names, model.ExportTable(lane))
	}
	sort.Strings(names)
	return append(out, names...)
}

// beforePhase1 is steps 3 to 6. The deadline or Stop arriving skips the
// remaining steps: phase 1 then saves what was merged, and nothing is pushed.
func (x *exec) beforePhase1(chunk int) error {
	r := x.run
	x.resetAttempt()
	x.prepare()
	x.ingest(chunk)
	x.saveCursors(false)
	if x.cut("while reading sources") {
		return nil
	}

	if h := x.s.hooks.Intake; h != nil {
		err := h(r)
		switch {
		case errors.Is(err, api.ErrEventsShrank):
			r.EventsShrank = true
			x.noPush("the event log shrank below a saved cursor")
			x.problem("events_shrank", "the event log holds fewer events than a saved cursor says were read: "+err.Error(),
				"restore the deleted events (or rows) from a backup; pushing waits until then", false)
		case err != nil:
			x.hookFailed("intake", err)
			return fmt.Errorf("intake: %w", err)
		default:
			x.saveCursors(true) // the sources' events reached Intake
		}
	}
	if x.cut("during intake") {
		return nil
	}
	if h := x.s.hooks.Enrich; h != nil && !r.DryRun {
		if err := h(r); err != nil {
			x.hookFailed("enrich", err)
		}
	}
	if x.cut("during enrichment") {
		return nil
	}
	if h := x.s.hooks.Fold; h != nil {
		if err := h(r); err != nil {
			x.hookFailed("fold", err)
			return fmt.Errorf("status fold: %w", err)
		}
	}
	if x.cut("during the status fold") {
		return nil
	}

	// The in-run checks (contracts section 10) run here, after the merge and
	// fold, so the rubric check sees this run's columns. A rubric field no
	// input carries fails the run before scoring.
	env := check.Env{Config: x.cfg, Model: r.Model, Store: x.store, Events: r.Events, Columns: x.columns, Rubric: r.Rubric}
	var unknown []string
	for _, c := range check.InRun() {
		for _, p := range c.Run(r.Ctx, env) {
			x.problem(p.Key, p.Message, p.Fix, p.Warning)
			if f, ok := strings.CutPrefix(p.Key, check.UnknownFieldKind+":"); ok {
				unknown = append(unknown, f)
			}
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("the rubric reads %s, which is not built in, declared under fields, or a loaded column", strings.Join(unknown, ", "))
	}

	var det rules.DetectorResults
	if h := x.s.hooks.Detect; h != nil {
		d, err := h(r)
		if err != nil {
			x.hookFailed("detect", err)
		} else {
			det = d
		}
	}
	if x.cut("during detection") {
		return nil
	}
	return x.score(det)
}

// resetAttempt clears what one pass of steps 3 to 6 sets, for the redo after
// ErrTooLarge.
func (x *exec) resetAttempt() {
	x.mu.Lock()
	x.problems = map[string]problem{}
	x.mu.Unlock()
	r := x.run
	r.NoPush, r.SourceEvents, r.EventsShrank = "", nil, false
	r.Input, r.Result = rules.Input{}, rules.Result{}
	r.lv, r.pushing = nil, nil
	x.columns, x.merged, x.backlog, x.cursors = nil, 0, 0, nil
	x.scored, x.cutShort = false, false
	x.oldRanked, x.tierLogs = nil, nil
}

// prepare is the run-start work recorded in the model: the hosted config
// version, skipped runs, the first run's time, a lease takeover, and each
// lane's sink.
func (x *exec) prepare() {
	r, m := x.run, x.run.Model
	if v := x.s.getenv(configVersionVar); v != "" && m.StateValue("config_version") != v {
		m.SetState("config_version", v)
	}
	if last := m.Health[model.K("result", "last_run_at")].Value; last != "" {
		if t, err := model.ParseTime(last); err == nil && x.startAt.Sub(t) > 2*x.cfg.Schedule {
			x.problem("skipped_runs", fmt.Sprintf("the last run before this one started at %s, more than two schedule intervals ago", last),
				"check that runs are not skipped: only one install should use this store, and each run should finish within its deadline", true)
		}
	}
	if m.StateValue("first_run_at") == "" {
		m.SetState("first_run_at", model.FormatTime(x.startAt))
	}
	if x.takeover != "" {
		x.log("warn", "lease_takeover", "", "took over the expired run lease of run "+x.takeover)
	}
	for _, l := range r.Rubric.Lanes() {
		if l.Sink == "export" {
			continue // export lanes are written by the engine, not a sink
		}
		if _, ok := sinkFactory(l.Sink); !ok {
			x.problem("lane_sink_unregistered:"+l.ID,
				fmt.Sprintf("lane %s pushes to %s, but this build has no %q sink, so the lane cannot push", l.ID, l.Push, l.Sink),
				fmt.Sprintf("use a build that includes the %s sink, or change the lane's push", l.Sink), false)
		}
	}
}

// cut reports whether the deadline has passed or Stop was closed, recording
// it once.
func (x *exec) cut(where string) bool {
	if x.run.PushCtx.Err() == nil {
		return false
	}
	if !x.cutShort {
		x.cutShort = true
		if x.dlCtx.Err() != nil {
			x.problem("deadline_passed", fmt.Sprintf("the run reached its deadline (%s) %s; it saved what it had and pushed nothing more", x.cfg.Deadline, where),
				"if this repeats, raise deadline or lower ingest_chunk_rows in leadscore.yml", false)
		} else {
			x.problem("run_stopped", "the run was asked to stop "+where+"; it saved what it had and pushed nothing more",
				"nothing to do: the next run carries on", true)
		}
	}
	return true
}

// push is steps 8 and 9. A dry run calls PrePush (whose lookups S10b skips
// on a dry run) but never Push.
func (x *exec) push() {
	r := x.run
	if !x.scored || x.cut("before pushing") {
		return
	}
	if h := x.s.hooks.PrePush; h != nil {
		err := h(r, x.candidates())
		x.buildRanked() // PrePush may have re-scored: Ranked follows Run.Result
		if err != nil {
			x.hookFailed("prepush", err)
			return
		}
	}
	if r.DryRun {
		return
	}
	if h := x.s.hooks.Push; h != nil {
		if err := h(r); err != nil {
			x.hookFailed("push", err)
		}
	}
	x.cut("while pushing")
}

// reRead is Run.ReRead.
func (x *exec) reRead() ([]api.LeadID, error) {
	h := x.s.hooks.ReRead
	if h == nil {
		return nil, nil
	}
	changed, err := h(x.run)
	if err != nil {
		x.hookFailed("reread", err)
	}
	return changed, err
}

// candidates are the leads a cold or non-cold lane matched this run, not
// blocked by a rubric conflict: who the pre-push checks look at.
func (x *exec) candidates() []api.LeadID {
	r := x.run
	kind := map[string]string{}
	for _, l := range r.Rubric.Lanes() {
		kind[l.ID] = l.Kind
	}
	var out []api.LeadID
	for _, ref := range r.Input.Leads {
		if _, blocked := r.Result.Blocked[ref.ID]; blocked {
			continue
		}
		for _, lane := range r.Result.Lanes[ref.ID] {
			if kind[lane] != "export" {
				out = append(out, ref.ID)
				break
			}
		}
	}
	return out
}

// commit checks the lease, then commits. A lost lease stops every later
// write. ErrTooLarge on phase 1 is returned for the caller to halve; any
// other failure is retried once at the same size.
func (x *exec) commit(name string, writes []api.TableWrite, phase1 bool) error {
	if len(writes) == 0 {
		return nil
	}
	r := x.run
	try := func() error {
		if err := r.Lease.Check(r.Ctx); err != nil {
			if errors.Is(err, api.ErrLeaseLost) {
				x.lost = true
			}
			return err
		}
		return x.store.Commit(r.Ctx, writes)
	}
	err := try()
	if errors.Is(err, api.ErrCommittedWithProblems) {
		// Every write landed; only a people tab needs a person. Resending
		// would fail (keys already appended), so take it as committed.
		x.problem("people_tab_check", errText(err), "open the tab the message names and check its rows", true)
		err = nil
	}
	if err != nil && !x.lost && r.Ctx.Err() == nil && !(phase1 && errors.Is(err, api.ErrTooLarge)) {
		err = try()
	}
	if err != nil {
		if x.lost {
			return fmt.Errorf("%s: %w (another run took over; this run writes nothing more)", name, err)
		}
		if phase1 && errors.Is(err, api.ErrTooLarge) {
			return err
		}
		return fmt.Errorf("%s commit: %w", name, err)
	}
	r.Model.Committed(writes)
	return nil
}

// writeRanked writes Ranked after phase 2, in chunks. The tier and priority
// change lines go in the first chunk's commit, so a Ranked write that fails
// never logs a change twice.
func (x *exec) writeRanked() error {
	m := x.run.Model
	for _, e := range x.tierLogs {
		m.Put(model.TableLog, e)
	}
	logs := codec.Encode(m, model.TableLog)
	first := true
	for _, w := range codec.Encode(m, model.TableRanked) {
		for _, c := range codec.Chunk(w, rankedChunkRows) {
			writes := []api.TableWrite{c}
			if first {
				writes, first = append(writes, logs...), false
			}
			if err := x.commit("Ranked", writes, false); err != nil {
				return err
			}
		}
	}
	if first {
		return x.commit("Log", logs, false)
	}
	return nil
}

func (x *exec) hookFailed(name string, err error) {
	x.problem("step_failed:"+name, name+" failed: "+err.Error(), "see the message; the run carried on without this step where it could", false)
}

// noPush blocks pushing for the run, keeping every reason.
func (x *exec) noPush(reason string) {
	if x.run.NoPush == "" {
		x.run.NoPush = reason
	} else if !strings.Contains(x.run.NoPush, reason) {
		x.run.NoPush += "; " + reason
	}
}

// problem raises an open problem this run (Run.Problem). The first raise of a
// key wins.
func (x *exec) problem(key, message, fix string, warning bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.problems[key]; !ok {
		x.problems[key] = problem{key: key, message: message, fix: fix, warning: warning}
	}
}

func (x *exec) log(level, kind string, lead api.LeadID, msg string) {
	x.run.Model.Put(model.TableLog, model.LogEntry{At: x.run.Now(), RunID: x.run.ID, Level: level, LeadID: lead, Kind: kind,
		Message: msg, RubricVersion: x.run.Rubric.Version()})
}

// healthy is true when no problem raised this run is an error.
func (x *exec) healthy() bool {
	if x.failed {
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, p := range x.problems {
		if !p.warning {
			return false
		}
	}
	return true
}

func (x *exec) problemKeys() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]string, 0, len(x.problems))
	for k := range x.problems {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// finish turns the run's end into its result, writing Health for a run that
// failed after it took the lease, and prints the summary line.
func (x *exec) finish(err error) (api.RunResult, error) {
	if err != nil {
		x.failed = true
		x.problem("run_failed", "the run stopped: "+err.Error(), "fix the cause in the message; the next run tries again", false)
		x.saveFailure()
	}
	res := api.RunResult{Healthy: x.healthy(), Problems: x.problemKeys(), Pushed: x.run.Pushed}
	state := "healthy"
	switch {
	case err != nil:
		state = "failed: " + err.Error()
	case !res.Healthy:
		state = "unhealthy"
	}
	line := fmt.Sprintf("run %s: %s; %d lead(s) scored, %d input row(s) merged, %d left for later runs, %d pushed",
		x.run.ID, state, len(x.run.Input.Leads), x.merged, x.backlog, x.run.Pushed)
	if len(res.Problems) > 0 {
		line += "; open problems: " + strings.Join(res.Problems, ", ")
	}
	fmt.Fprintln(x.s.out, logredact.Redact(line))
	return res, err
}

// emptyStore stands in for a store that does not exist yet, on a dry run: it
// reads as empty and refuses every write.
type emptyStore struct{}

var errNoStore = errors.New("no store yet")

func (emptyStore) ReadTable(context.Context, string) ([]api.Row, error) { return nil, nil }
func (emptyStore) Lease(context.Context, string, time.Duration) (api.RunLease, error) {
	return nil, errNoStore
}
func (emptyStore) Commit(context.Context, []api.TableWrite) error     { return errNoStore }
func (emptyStore) AppendEvents(context.Context, []api.RawEvent) error { return errNoStore }
func (emptyStore) DeleteProcessed(_ context.Context, c api.Cursor, _ time.Time) (api.Cursor, error) {
	return c, nil
}
func (emptyStore) ReadEvents(_ context.Context, c api.Cursor) ([]api.RawEvent, api.Cursor, error) {
	return nil, c, nil
}
