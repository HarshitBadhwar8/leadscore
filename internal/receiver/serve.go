package receiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// ServeOptions are `leadscore serve`'s command line.
type ServeOptions struct {
	ConfigPath, RubricPath string
	// Timer is true with --every; Every is its value, zero meaning the
	// config's `schedule` (read once, here).
	Timer bool
	Every time.Duration
	// Out gets each run's summary line, Log the receiver's own lines.
	Out, Log io.Writer
}

// RunFunc is one run of the loop; closing stop is the graceful stop.
type RunFunc func(ctx context.Context, opts api.RunOptions) (api.RunResult, error)

// deps are what tests replace.
type deps struct {
	run      RunFunc
	listener net.Listener // nil: listen on receiver.port
	now      func() time.Time
	getenv   func(string) string
	started  chan<- string // gets the listen address once serving (tests)
}

// Serve runs `leadscore serve` until ctx is done (SIGTERM), then shuts down
// in the contracts section 5.1 order: stop the timer, close the running
// run's Stop and keep storing events until it returns, drain the write
// queue, and return.
func Serve(ctx context.Context, o ServeOptions) error {
	return serve(ctx, o, deps{})
}

func serve(ctx context.Context, o ServeOptions, d deps) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.getenv == nil {
		d.getenv = os.Getenv
	}
	if d.run == nil {
		d.run = func(ctx context.Context, opts api.RunOptions) (api.RunResult, error) {
			return engine.RunTo(ctx, opts, o.Out)
		}
	}
	logf := func(format string, args ...any) {
		fmt.Fprintln(o.Log, "leadscore serve: "+logredact.Redact(fmt.Sprintf(format, args...)))
	}

	cfg, err := config.Load(config.Options{ConfigPath: o.ConfigPath, RubricPath: o.RubricPath, Getenv: d.getenv})
	if err != nil {
		return err
	}
	// A receiver writing to a file Cloud Run throws away would lose events.
	if ps := check.CloudRunRefusal(cfg, d.getenv); len(ps) > 0 {
		return fmt.Errorf("%s (%s)", ps[0].Message, ps[0].Fix)
	}
	for _, p := range check.ReceiverSecretProblems(cfg, d.getenv) {
		level := "error"
		if p.Warning {
			level = "warning"
		}
		logf("%s: %s. Fix: %s.", level, p.Message, p.Fix)
	}
	if !secretSet(d.getenv) {
		logf("warning: %s is not set, so every webhook gets 401; /healthz and the timer still work", SecretVar)
	}

	open, ok := api.BackendFactory(cfg.Store.Type)
	if !ok {
		return fmt.Errorf("store type %q is not registered in this build", cfg.Store.Type)
	}
	store, events, err := open(cfg.Store.Block)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	if c, ok := store.(io.Closer); ok {
		defer c.Close()
	}
	// Records which container uses a SQLite file, for the store check; other
	// stores ignore it.
	if err := sqlite.MarkOpenedBy(ctx, store); err != nil {
		return fmt.Errorf("recording the container that opened the store: %w", err)
	}

	every := time.Duration(0)
	if o.Timer {
		every = o.Every
		if every <= 0 {
			every = cfg.Schedule
		}
	}
	started := d.now()
	h := NewHandler(Options{Store: store, Events: events, Now: d.now, Getenv: d.getenv, Every: every, Started: started, Log: o.Log})

	ln := d.listener
	if ln == nil {
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(cfg.Receiver.Port))
		if err != nil {
			h.Close()
			return fmt.Errorf("listening on port %d: %w", cfg.Receiver.Port, err)
		}
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	if every > 0 {
		logf("listening on %s; running every %s", ln.Addr(), every)
	} else {
		logf("listening on %s; no timer (runs are started elsewhere)", ln.Addr())
	}
	if d.started != nil {
		d.started <- ln.Addr().String()
	}

	// The timer runs until shutdown; timerCtx ending closes the running
	// run's Stop.
	timerCtx, stopTimer := context.WithCancel(context.Background())
	defer stopTimer()
	timerDone := make(chan struct{})
	if every > 0 {
		t := &timer{
			every: every, now: d.now, store: store, log: logf, h: h,
			run: func(stop <-chan struct{}) (api.RunResult, error) {
				return d.run(context.Background(), api.RunOptions{ConfigPath: o.ConfigPath, RubricPath: o.RubricPath, Stop: stop})
			},
		}
		go func() { defer close(timerDone); t.loop(timerCtx) }()
	} else {
		close(timerDone)
	}

	var serveErr error
	select {
	case <-ctx.Done():
		logf("shutting down: stopping the timer and letting a running run save")
	case serveErr = <-srvErr:
		logf("the HTTP server stopped: %v", serveErr)
	}
	stopTimer()
	<-timerDone // the receiver keeps storing events until the run returns
	shutCtx, cancel := context.WithTimeout(context.Background(), holdCap+5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logf("closing open connections: %v", err)
	}
	h.Close() // drain the write queue
	logf("stopped")
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

func secretSet(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(SecretVar)) != ""
}

// timer runs the loop on Docker (RFC 6.9): the first run when serve starts,
// each next one `every` after the previous ended, so runs never overlap and
// a missed tick is never queued.
type timer struct {
	every time.Duration
	now   func() time.Time
	store api.Backend
	log   func(format string, args ...any)
	h     *Handler
	run   func(stop <-chan struct{}) (api.RunResult, error)
}

func (t *timer) loop(ctx context.Context) {
	for {
		t.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.every):
		}
	}
}

// once runs the loop one time. ctx ending closes the run's Stop; once
// returns only when the run has.
func (t *timer) once(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	stop := make(chan struct{})
	defer context.AfterFunc(ctx, func() { close(stop) })()
	startAt := t.now().UTC()
	res, err := t.safeRun(stop)
	var p *panicked
	switch {
	case errors.As(err, &p):
		t.log("the run panicked: %v", p.value)
		t.writePanic(startAt, p)
	case err != nil:
		// A run that failed after taking the lease wrote Health itself.
		t.log("the run failed: %v", err)
	case !res.Healthy:
		t.log("the run finished unhealthy; `leadscore status` lists its problems")
	}
	t.h.RunFinished(err != nil)
}

type panicked struct{ value any }

func (p *panicked) Error() string { return fmt.Sprintf("panic: %v", p.value) }

// safeRun recovers a panic that escapes the run, so the receiver stays up.
func (t *timer) safeRun(stop <-chan struct{}) (res api.RunResult, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &panicked{value: v}
		}
	}()
	return t.run(stop)
}

// writePanic records a recovered panic in Health, as the run could not:
// last_result unhealthy, last_run_at, and the run_failed problem.
func (t *timer) writePanic(startAt time.Time, p *panicked) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first := map[string]string{}
	if rows, err := t.store.ReadTable(ctx, model.TableHealth); err == nil {
		for _, r := range rows {
			first[r["kind"]+"\x00"+r["key"]] = r["first_seen_at"]
		}
	}
	now := model.FormatTime(t.now().UTC())
	row := func(kind, key, value string) api.Row {
		fs := first[kind+"\x00"+key]
		if fs == "" {
			fs = now
		}
		return api.Row{"kind": kind, "key": key, "value": value, "first_seen_at": fs, "updated_at": now}
	}
	def, _ := model.Def(model.TableHealth)
	err := t.store.Commit(ctx, []api.TableWrite{{
		Table: model.TableHealth, Op: api.OpUpsert, Key: def.Key,
		Rows: []api.Row{
			row("result", "last_result", "unhealthy"),
			row("result", "last_run_at", model.FormatTime(startAt)),
			row("problem", "run_failed", logredact.Redact("the run stopped: "+p.Error())+". Fix: fix the cause in the message; the next run tries again."),
		},
	}})
	if err != nil {
		t.log("writing the panicked run to Health: %v", err)
	}
}
