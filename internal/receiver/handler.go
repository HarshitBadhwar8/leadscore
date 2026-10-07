// Package receiver is `leadscore serve` (contracts section 5.1): the HTTP
// receiver for Apollo workflow requests, /healthz, and on Docker the run
// timer. The receiver only appends events to the store; every run reads them.
//
// Its one safety rule: a request is answered 2xx only after the store said
// the event is durable.
package receiver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/receiver/auth"
)

// Secret variables (RFC 6.13).
const (
	SecretVar         = check.ReceiverSecretVar
	PreviousSecretVar = check.ReceiverSecretPreviousVar
)

// SecretHeader carries the secret (contracts section 5.1).
//
// S0 confirms: that Apollo's workflow webhook action can send a custom
// header. The body field leadscore_secret is the fallback when it cannot.
const SecretHeader = "X-Leadscore-Secret"

// Routes (contracts section 5.1).
var routes = map[string]string{
	"/apollo/visit": apollo.KindVisit,
	"/apollo/reply": apollo.KindReply,
}

// healthCache is how long /healthz reuses a verdict read from the store.
var healthCache = 60 * time.Second

// Options configure a Handler.
type Options struct {
	Store  api.Backend  // read for /healthz with a timer
	Events api.EventLog // where requests are appended
	// Now is the clock for received times and /healthz; nil is time.Now.
	// Batching and the hold cap use the real clock.
	Now func() time.Time
	// Getenv reads the secret variables once, here; nil is os.Getenv.
	Getenv func(string) string
	// Every is the timer's interval; zero means no timer (Cloud Run), and
	// /healthz then reports only whether the last append failed.
	Every time.Duration
	// Started is when serve started: with a timer and no success yet, the
	// three intervals are measured from it. Zero means Now() at construction.
	Started time.Time
	Log     io.Writer // nil discards
}

// Handler is the receiver's HTTP handler. S17 builds one in process with its
// own clock; serve wraps it in an http.Server. Close drains it.
type Handler struct {
	o       Options
	secrets []string
	q       *queue
	logMu   sync.Mutex

	healthMu  sync.Mutex
	healthAt  time.Time // when the cached verdict was read (Options.Now)
	healthOK  bool
	healthMsg string
	// runFailed is set by the timer when the last run returned an error or
	// panicked, which it may not have written to Health.
	runFailed atomic.Bool
}

// NewHandler returns a receiver handler. It reads the secret variables now:
// rotating them means restarting serve (contracts section 5.1).
func NewHandler(o Options) *Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Started.IsZero() {
		o.Started = o.Now()
	}
	h := &Handler{o: o, secrets: []string{
		strings.TrimSpace(o.Getenv(SecretVar)),
		strings.TrimSpace(o.Getenv(PreviousSecretVar)),
	}}
	h.q = newQueue(o.Events, func(err error) {
		h.logf("storing a batch of webhook events failed; each request in it got 503 so Apollo can retry: %v", err)
	})
	return h
}

// Close stores every request already accepted, refuses new ones with 503,
// and returns once the write queue is empty.
func (h *Handler) Close() { h.q.close() }

func (h *Handler) logf(format string, args ...any) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	fmt.Fprintln(h.o.Log, "leadscore serve: "+logredact.Redact(fmt.Sprintf(format, args...)))
}

// ServeHTTP routes one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveHealth(w, r)
		return
	}
	kind, ok := routes[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.receive(w, r, kind)
}

// receive authenticates one Apollo request, stores it, and answers only once
// it is stored.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request, kind string) {
	received := h.o.Now().UTC()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}
	in := readBody(raw)
	presented := strings.TrimSpace(r.Header.Get(SecretHeader))
	if presented == "" {
		presented = strings.TrimSpace(in.secret)
	}
	if err := auth.VerifyAny(h.secrets, presented); err != nil {
		// One reason for every refusal, so a caller cannot probe which part
		// was wrong; the log says only which route refused.
		h.logf("refused a request to %s: wrong or missing secret", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	b, err := h.q.submit(api.RawEvent{Kind: kind, ReceivedAt: received, Body: storedBody(in)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The batch's append ends at the hold cap; the extra second only covers
	// a store slow to return after its context ended. Either way the answer
	// below is 2xx only when the append succeeded.
	t := time.NewTimer(time.Until(b.oldest.Add(holdCap + time.Second)))
	defer t.Stop()
	select {
	case <-b.done:
		if b.err != nil {
			http.Error(w, "the event could not be stored; retry", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "stored\n")
	case <-t.C:
		http.Error(w, "the event could not be stored in time; retry", http.StatusServiceUnavailable)
	}
}

// RunFinished is the timer's report of a run: failed when the run returned
// an error or panicked. /healthz is 503 after a failed run whatever the
// store holds, and the cached store verdict is dropped either way.
func (h *Handler) RunFinished(failed bool) {
	h.runFailed.Store(failed)
	h.healthMu.Lock()
	h.healthAt = time.Time{}
	h.healthMu.Unlock()
}

// serveHealth answers /healthz (contracts section 5.1).
func (h *Handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.health(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if ok {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	io.WriteString(w, msg+"\n")
}

func (h *Handler) health(r *http.Request) (bool, string) {
	if h.o.Every <= 0 {
		// No timer (Cloud Run): runs are the job's; the service is healthy
		// unless it cannot store.
		if h.q.appendFailed.Load() {
			return false, "unhealthy: the last attempt to store events failed"
		}
		return true, "ok"
	}
	if h.runFailed.Load() {
		return false, "unhealthy: the last run failed; see the serve log and `leadscore status`"
	}
	now := h.o.Now()
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	if !h.healthAt.IsZero() && now.Sub(h.healthAt) < healthCache && now.Sub(h.healthAt) >= 0 {
		return h.healthOK, h.healthMsg
	}
	ok, msg, err := h.readHealth(r, now)
	if err != nil {
		// Not cached: the next probe tries the store again.
		return false, "unhealthy: " + logredact.Redact(err.Error())
	}
	h.healthAt, h.healthOK, h.healthMsg = now, ok, msg
	return ok, msg
}

// readHealth judges the timer's runs from the Health table: 200 when the
// last run succeeded or none is due yet; 503 when the last run failed or none
// succeeded in three intervals (from serve's start when none ever did).
func (h *Handler) readHealth(r *http.Request, now time.Time) (bool, string, error) {
	rows, err := h.o.Store.ReadTable(r.Context(), model.TableHealth)
	if err != nil {
		return false, "", fmt.Errorf("reading Health: %w", err)
	}
	results := map[string]string{}
	for _, row := range rows {
		if row["kind"] == "result" {
			results[row["key"]] = row["value"]
		}
	}
	if results["last_result"] == "unhealthy" {
		return false, "unhealthy: the last run was unhealthy; see `leadscore status`", nil
	}
	since, what := h.o.Started, "serve started"
	if s := results["last_success_at"]; s != "" {
		t, err := model.ParseTime(s)
		if err != nil {
			return false, "", fmt.Errorf("Health's last_success_at is not a time")
		}
		since, what = t, "the last successful run"
	}
	if limit := 3 * h.o.Every; now.Sub(since) > limit {
		return false, fmt.Sprintf("unhealthy: no successful run in three intervals (%s since %s)", now.Sub(since).Round(time.Second), what), nil
	}
	return true, "ok", nil
}
