// Package engine is the run: one execution of the loop, from reading
// leadscore.yml to writing Ranked. The engine owns the loop, the lease, the
// two-phase writes, chunked intake, the deadline and the Health writer; the
// other steps plug in as Hooks, set in DefaultHooks.
package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// Run is one run as the hooks see it.
type Run struct {
	ID           string          // run id (UUIDv7), also the lease owner
	Ctx          context.Context // cancelled at the hard stop; vendor calls and the post-batch write use it
	PushCtx      context.Context // cancelled on Stop or at the deadline; stops new vendor calls only
	Config       *config.Config
	Rubric       *rules.Rubric
	Model        *model.Model
	Store        api.Backend
	Events       api.EventLog
	Lease        api.RunLease // nil on a dry run
	DryRun       bool
	Now          func() time.Time
	HTTPClient   *http.Client
	SourceEvents []api.Event                                  // step 3 source events, keys already normalized, set by the engine before Intake
	NoPush       string                                       // non-empty: score and save, but push nothing (the reason)
	EventsShrank bool                                         // Intake returned ErrEventsShrank: no processed event is deleted this run
	Problem      func(key, message, fix string, warning bool) // raise an open problem this run
	// ReRead calls Hooks.ReRead for the push loop before each batch; with no
	// hook it returns nothing. Its error also raises step_failed:reread.
	ReRead func() (changed []api.LeadID, err error)
	Input  rules.Input  // step 6's evaluator input; a re-score updates it
	Result rules.Result // step 6's result; PrePush may update it, and Ranked and the dry-run report follow it
	Pushed int          // pushes made this run; RunResult.Pushed

	lv         *view        // the lane view, built on first use; nil after invalidate
	pushing    *pushRun     // what PrePush decided for Push; nil before PrePush
	leaseUntil time.Time    // when the lease taken at start runs out (real clock); zero on a dry run
	judged     bool         // step 6 finished on full inputs (no Enrich or Detect failure), so Result can list and reopen export rows; set before Export
	enrich     *enrichMemo  // what Enrich bought this run; kept across an ErrTooLarge redo so no answer is paid for twice
	reread     *rereadState // the re-read's position in the event log this run; nil before the first
}

// Hooks are the run's plug-in steps. A nil hook is skipped. Their errors are
// handled by the run's rule for hook errors.
type Hooks struct {
	Intake    func(*Run) error                             // step 3 after sources
	Enrich    func(*Run) error                             // step 4; skipped on dry-run
	Fold      func(*Run) error                             // step 5; default gives every live lead with no stored status new
	Detect    func(*Run) (rules.DetectorResults, error)    // step 6, before Evaluate
	PrePush   func(*Run, []api.LeadID) error               // step 8
	Push      func(*Run) error                             // step 9
	ReRead    func(*Run) (changed []api.LeadID, err error) // before each pushing batch
	Export    func(*Run) error                             // after Push, before phase 2, every run
	AfterSave func(*Run) error                             // after phase 2 committed: CSV rewrite, view
}

// DefaultHooks is the production set. Steps sharing AfterSave each add one
// function to its Chain (deleting processed events, the CSV rewrite, the
// view).
func DefaultHooks() Hooks {
	return Hooks{
		Intake:    intake,
		Enrich:    enrichHook,
		Fold:      foldHook,
		Detect:    detectHook,
		PrePush:   prePushHook,
		Push:      pushHook,
		ReRead:    reReadHook,
		Export:    exportHook,
		AfterSave: Chain(deleteProcessed, writeExportCSVs, writeView),
	}
}

// Chain runs several hook functions in order, each even when an earlier one
// failed, and returns their errors joined. With none it does nothing.
func Chain(fs ...func(*Run) error) func(*Run) error {
	return func(r *Run) error {
		var errs []error
		for _, f := range fs {
			if err := f(r); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// Fixed values (the engine defaults). Variables so tests can shrink them.
var (
	// saveBudget is how long the run may save after the deadline.
	saveBudget = config.SaveBudget
	// leaseMargin is how long before the lease expires the run is stopped.
	leaseMargin = 30 * time.Second
	// rankedChunkRows bounds one Ranked write after phase 2.
	rankedChunkRows = 5000
)

// RunWith is the test entry point: it adds _http_client to every vendor and
// store block when client is set, and uses the given clock. A test
// pointing a block at a fake writes that block's base_url in its own YAML.
// The dry-run report goes to stdout.
func RunWith(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client) (api.RunResult, error) {
	return RunWithOutput(ctx, opts, hooks, now, client, os.Stdout)
}

// RunWithOutput is RunWith writing the run's summary line (and a dry run's
// report) to out instead of standard output, so a test can log or drop it.
// A nil out discards it (execute treats it as io.Discard).
func RunWithOutput(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client, out io.Writer) (api.RunResult, error) {
	return execute(ctx, opts, settings{hooks: hooks, now: now, client: client, out: out, getenv: os.Getenv})
}

// RunTo is one production run with DefaultHooks, writing its summary line
// (and, on a dry run, the report) to out. `leadscore run` and leadscore.Run
// use it.
func RunTo(ctx context.Context, opts api.RunOptions, out io.Writer) (api.RunResult, error) {
	return execute(ctx, opts, settings{hooks: DefaultHooks(), now: time.Now, out: out, getenv: os.Getenv, loadKeys: loadHostedKeys})
}

// settings is how one run is wired: the hooks, clock, HTTP client, output and
// environment.
type settings struct {
	hooks  Hooks
	now    func() time.Time
	client *http.Client
	out    io.Writer
	getenv func(string) string
	// loadKeys fills empty key variables from Secret Manager on a hosted
	// install run locally (the hosting settings); nil reads nothing. RunWith
	// leaves it nil, so tests never reach Google.
	loadKeys func(context.Context, *config.Config) error
}

// loadHostedKeys is the production loadKeys: as the run account, into the
// process environment the adapters read.
func loadHostedKeys(ctx context.Context, c *config.Config) error {
	return hosting.LoadKeys(ctx, c, os.Getenv, os.Setenv, nil)
}

// withTestClient adds _http_client to every vendor and store block.
func withTestClient(c *config.Config, client *http.Client) {
	if client == nil {
		return
	}
	set := func(b api.Config) {
		if b != nil {
			b["_http_client"] = client
		}
	}
	set(c.Store.Block)
	for _, s := range c.Sources {
		set(s.Block)
	}
	if c.Enrich != nil {
		set(c.Enrich.Block)
	}
	for _, b := range c.Sinks {
		set(b)
	}
}
