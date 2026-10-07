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

// How long /healthz reuses a verdict read from the store, and a failure to
// read it.
var (
	healthCache    = 60 * time.Second
	healthErrCache = 10 * time.Second
)

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
	unauth  chan struct{} // a slot per unauthenticated body being read

	refusalMu sync.Mutex
	refusals  int       // refusals not logged yet
	refusalAt time.Time // when a refusal was last logged (real clock)

	healthMu  sync.Mutex
	healthAt  time.Time // when the cached verdict was read (Options.Now)
	healthOK  bool
	healthMsg string
	healthTTL time.Duration
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
	}, unauth: make(chan struct{}, unauthReads)}
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

// Limits on requests not yet authenticated (a body-field secret is only
// known once the body is read). Variables so tests can shrink them.
var (
	// unauthReads is how many such bodies may be read at once.
	unauthReads = 16
	// unauthReadTime is how long one such body may take to arrive.
	unauthReadTime = 3 * time.Second
	// unauthWait is how long a request waits for a free read before 503.
	unauthWait = 5 * time.Second
	// refusalLogEvery is the most often a refusal is logged; the refusals in
	// between are counted into the next line.
	refusalLogEvery = time.Minute
)

// receive authenticates one Apollo request, stores it, and answers only once
// it is stored. Nothing is decoded before the secret is checked: a header
// secret is checked before the body is read at all, and a body secret is
// found by scanning only the body's first level.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request, kind string) {
	received := h.o.Now().UTC()
	// With no current secret every request is refused, even one carrying a
	// previous secret (contracts section 5.1).
	if h.secrets[0] == "" {
		h.refuse(w, r)
		return
	}
	var raw []byte
	if presented := strings.TrimSpace(r.Header.Get(SecretHeader)); presented != "" {
		if auth.VerifyAny(h.secrets, presented) != nil {
			h.refuse(w, r)
			return
		}
		var ok bool
		if raw, ok = h.read(w, r); !ok {
			return
		}
	} else {
		t := time.NewTimer(unauthWait)
		select {
		case h.unauth <- struct{}{}:
			t.Stop()
		case <-t.C:
			http.Error(w, "too many requests at once; retry", http.StatusServiceUnavailable)
			return
		case <-r.Context().Done():
			t.Stop()
			return
		}
		// A slow sender must not hold a slot for the server's whole read
		// timeout: an unauthenticated body gets a short deadline of its own.
		// (Not supported by a test recorder; the slot cap still holds.)
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(unauthReadTime))
		var ok bool
		raw, ok = h.read(w, r)
		var presented []string
		if ok {
			presented = scanSecret(raw)
		}
		<-h.unauth
		if !ok {
			return
		}
		accepted := false
		for _, p := range presented {
			if auth.VerifyAny(h.secrets, strings.TrimSpace(p)) == nil {
				accepted = true
			}
		}
		if !accepted {
			h.refuse(w, r)
			return
		}
	}
	b, i, err := h.q.submit(api.RawEvent{Kind: kind, ReceivedAt: received, Body: storedBody(readBody(raw, h.secrets))})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The batch's append ends at the hold cap; the extra second only covers
	// a store slow to return after its context ended. Either way the answer
	// below is 2xx only when this event was stored.
	t := time.NewTimer(time.Until(b.oldest.Add(holdCap + time.Second)))
	defer t.Stop()
	select {
	case <-b.done:
		if err := b.errs[i]; err != nil {
			if errors.Is(err, errTooLarge) {
				h.logf("could not store a request to %s: %v", r.URL.Path, err)
			}
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

// read reads the request body, at most maxRequestBytes; a bigger one is
// answered 413 and logged.
func (h *Handler) read(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.logf("refused a request to %s: its body is over %d bytes (Content-Length %d)", r.URL.Path, maxRequestBytes, r.ContentLength)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return nil, false
	}
	return raw, true
}

// refuse answers 401 with one reason for every refusal, so a caller cannot
// probe which part was wrong. Refusals are logged at most once a minute,
// with how many there were, so a flood cannot flood the log.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request) {
	h.refusalMu.Lock()
	h.refusals++
	now := time.Now()
	if h.refusalAt.IsZero() || now.Sub(h.refusalAt) >= refusalLogEvery {
		n := h.refusals
		h.refusals, h.refusalAt = 0, now
		h.refusalMu.Unlock()
		h.logf("refused %d request(s) with a wrong or missing secret (the last to %s)", n, r.URL.Path)
	} else {
		h.refusalMu.Unlock()
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	if !h.healthAt.IsZero() && now.Sub(h.healthAt) < h.healthTTL && now.Sub(h.healthAt) >= 0 {
		return h.healthOK, h.healthMsg
	}
	ok, msg, err := h.readHealth(r, now)
	ttl := healthCache
	if err != nil {
		// The detail goes to the log only; a failure is kept for a shorter
		// time, so the store is not asked on every probe while it is down.
		h.logf("/healthz: %v", err)
		ok, msg, ttl = false, "unhealthy: cannot read the store", healthErrCache
	}
	h.healthAt, h.healthOK, h.healthMsg, h.healthTTL = now, ok, msg, ttl
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
