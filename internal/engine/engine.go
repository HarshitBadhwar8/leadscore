// Package engine is the run (RFC 6.9, contracts section 12.6): one execution
// of the loop, from reading leadscore.yml to writing Ranked. S10a owns the
// loop, the lease, the two-phase writes, chunked intake, the deadline and the
// Health writer; the other steps plug in as Hooks, which each owning slice sets
// in DefaultHooks.
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
	SourceEvents []api.Event                                  // step 3 source events, keys already normalized, set by S10a before Intake
	NoPush       string                                       // non-empty: score and save, but push nothing (the reason)
	EventsShrank bool                                         // Intake returned ErrEventsShrank: no processed event is deleted this run
	Problem      func(key, message, fix string, warning bool) // raise an open problem this run
	// ReRead calls Hooks.ReRead (S15) for S10b's push loop before each batch;
	// with no hook it returns nothing. Its error also raises step_failed:reread.
	ReRead func() (changed []api.LeadID, err error)
	Input  rules.Input  // step 6's evaluator input; a re-score (S10b) updates it
	Result rules.Result // step 6's result; PrePush may update it, and Ranked and the dry-run report follow it
	Pushed int          // pushes made this run (S10b); RunResult.Pushed

	lv      *view    // the lane view (S10b), built on first use; nil after invalidate
	pushing *pushRun // what PrePush decided for Push (S10b); nil before PrePush
	scored  bool     // step 6 finished, so Result holds this run's lanes (set before Export)
}

// Hooks are the run's plug-in steps. A nil hook is skipped. Their errors are
// handled per contracts section 12.6 ("Hook errors").
type Hooks struct {
	Intake    func(*Run) error                             // step 3 after sources (S9)
	Enrich    func(*Run) error                             // step 4 (S8); skipped on dry-run
	Fold      func(*Run) error                             // step 5; default gives every live lead with no stored status new (S10b)
	Detect    func(*Run) (rules.DetectorResults, error)    // step 6, before Evaluate (S9)
	PrePush   func(*Run, []api.LeadID) error               // step 8 (S10b)
	Push      func(*Run) error                             // step 9 (S10b)
	ReRead    func(*Run) (changed []api.LeadID, err error) // before each pushing batch (S15)
	Export    func(*Run) error                             // after Push, before phase 2, every run (S13)
	AfterSave func(*Run) error                             // after phase 2 committed: CSV rewrite (S13), view (S16)
}

// DefaultHooks is the production set. Each hook slice sets its field here in
// its own PR; slices sharing AfterSave each add one function to its Chain
// (S9's deletion of processed events, S13's CSV rewrite, S16's view).
func DefaultHooks() Hooks {
	return Hooks{
		Intake:    intake,
		Fold:      foldHook,
		Detect:    detectHook,
		PrePush:   prePushHook,
		Push:      pushHook,
		Export:    exportHook,
		AfterSave: Chain(deleteProcessed, writeExportCSVs),
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

// Fixed values (contracts section 11). Variables so tests can shrink them.
var (
	// saveBudget is how long the run may save after the deadline.
	saveBudget = 90 * time.Second
	// leaseMargin is how long before the lease expires the run is stopped.
	leaseMargin = 30 * time.Second
	// rankedChunkRows bounds one Ranked write after phase 2.
	rankedChunkRows = 5000
)

// RunWith is the test entry point: it adds _http_client to every vendor and
// store block (section 3) when client is set, and uses the given clock. A test
// pointing a block at a fake writes that block's base_url in its own YAML.
// The dry-run report goes to stdout.
func RunWith(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client) (api.RunResult, error) {
	return execute(ctx, opts, settings{hooks: hooks, now: now, client: client, out: os.Stdout, getenv: os.Getenv})
}

// RunTo is one production run with DefaultHooks, writing its summary line
// (and, on a dry run, the report) to out. `leadscore run` and leadscore.Run
// use it.
func RunTo(ctx context.Context, opts api.RunOptions, out io.Writer) (api.RunResult, error) {
	return execute(ctx, opts, settings{hooks: DefaultHooks(), now: time.Now, out: out, getenv: os.Getenv})
}

// settings is how one run is wired: the hooks, clock, HTTP client, output and
// environment.
type settings struct {
	hooks  Hooks
	now    func() time.Time
	client *http.Client
	out    io.Writer
	getenv func(string) string
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
