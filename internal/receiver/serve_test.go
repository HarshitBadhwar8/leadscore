// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package receiver

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// install writes leadscore.yml (SQLite store, the sample CSV) and returns its
// path and the store file.
func install(t *testing.T, extra string) (cfgPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	leads, err := os.ReadFile("../../examples/leads.csv")
	if err != nil {
		t.Fatal(err)
	}
	rubric, _ := filepath.Abs("../../testdata/compose/rubric.yml")
	if err := os.WriteFile(filepath.Join(dir, "leads.csv"), leads, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "version: 1\nrubric: " + rubric + "\nstore: { type: sqlite, path: leadscore.db }\nexport: { dir: out }\n" +
		"sources:\n  - { id: leads, type: csv, path: leads.csv }\nreplies: receiver\n" + extra
	cfgPath = filepath.Join(dir, "leadscore.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, filepath.Join(dir, "leadscore.db")
}

// startServe runs serve in the background on a local port and returns its
// base URL, a stop function (SIGTERM) and the channel serve's error lands on.
func startServe(t *testing.T, o ServeOptions, d deps) (url string, sigterm func(), done <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.listener = ln
	if d.getenv == nil {
		d.getenv = env(map[string]string{SecretVar: testSecret})
	}
	started := make(chan string, 1)
	d.started = started
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	fin := make(chan struct{})
	go func() { errc <- serve(ctx, o, d); close(fin) }()
	select {
	case addr := <-started:
		url = "http://" + addr
	case err := <-errc:
		t.Fatalf("serve did not start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-fin:
		case <-time.After(30 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return url, cancel, errc
}

func postHTTP(t *testing.T, url, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set(SecretHeader, testSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("POST %s: %v", path, err)
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func getHealthz(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The timer: the first run starts with serve, each next one a full interval
// after the previous ended, and two runs never overlap.
func TestTimerNeverOverlaps(t *testing.T) {
	cfg, _ := install(t, "")
	every := 30 * time.Millisecond
	var (
		mu         sync.Mutex
		starts     []time.Time
		ends       []time.Time
		active     atomic.Int32
		overlapped atomic.Bool
	)
	t0 := time.Now()
	run := func(_ context.Context, _ api.RunOptions) (api.RunResult, error) {
		if active.Add(1) > 1 {
			overlapped.Store(true)
		}
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // longer than the interval
		mu.Lock()
		ends = append(ends, time.Now())
		mu.Unlock()
		active.Add(-1)
		return api.RunResult{Healthy: true}, nil
	}
	_, sigterm, done := startServe(t, ServeOptions{ConfigPath: cfg, Timer: true, Every: every}, deps{run: run})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(ends) >= 4 })
	sigterm()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if overlapped.Load() {
		t.Error("two runs overlapped")
	}
	mu.Lock()
	defer mu.Unlock()
	if d := starts[0].Sub(t0); d > time.Second {
		t.Errorf("the first run started %v after serve, want at once", d)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(ends[i-1]); gap < every {
			t.Errorf("run %d started %v after run %d ended, want at least %v", i, gap, i-1, every)
		}
	}
}

// A run that panics is recovered: the receiver keeps serving, serve writes
// the failure to Health itself, /healthz turns 503, and the timer goes on.
func TestAPanickingRunLeavesTheReceiverUp(t *testing.T) {
	cfg, db := install(t, "")
	var calls atomic.Int32
	run := func(_ context.Context, _ api.RunOptions) (api.RunResult, error) {
		if calls.Add(1) == 1 {
			panic("boom in a hook")
		}
		return api.RunResult{Healthy: true}, nil
	}
	url, sigterm, done := startServe(t, ServeOptions{ConfigPath: cfg, Timer: true, Every: 200 * time.Millisecond}, deps{run: run, window: testWindow})
	waitFor(t, func() bool { return calls.Load() >= 1 })
	if code := postHTTP(t, url, "/apollo/reply", `{"event":"email_sent","contact_email":"a@example.com"}`); code != 200 {
		t.Errorf("POST after a panicking run: %d, want 200", code)
	}
	waitFor(t, func() bool { return getHealthz(t, url) == 503 })
	waitFor(t, func() bool { return calls.Load() >= 2 }) // the timer carried on
	sigterm()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.ReadTable(context.Background(), model.TableHealth)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r["kind"]+":"+r["key"]] = r["value"]
	}
	if got["result:last_result"] != "unhealthy" || got["result:last_run_at"] == "" {
		t.Errorf("Health after a panic = %v, want last_result unhealthy and last_run_at", got)
	}
	if !strings.Contains(got["problem:run_failed"], "boom in a hook") {
		t.Errorf("run_failed = %q, want the panic", got["problem:run_failed"])
	}
	evs, _, _ := s.ReadEvents(context.Background(), "")
	if len(evs) != 1 {
		t.Errorf("%d events stored, want 1", len(evs))
	}
}

// SIGTERM during a real run: serve closes the run's Stop, keeps storing
// webhooks until the run returns, and the run saves what it merged before
// serve exits.
func TestSIGTERMDuringARunSavesBeforeExit(t *testing.T) {
	cfg, db := install(t, "")
	var url string
	inRun := make(chan struct{})
	var stoppedPost atomic.Int32
	hooks := engine.DefaultHooks()
	intake := hooks.Intake
	hooks.Intake = func(r *engine.Run) error {
		close(inRun)
		<-r.PushCtx.Done() // the stop serve sends on SIGTERM
		// The run is still going: the receiver must still store webhooks.
		stoppedPost.Store(int32(postHTTP(t, url, "/apollo/reply", `{"event":"email_unsubscribed","contact_email":"anna.weber@kranlogistik.example"}`)))
		return intake(r)
	}
	var runReturned atomic.Bool
	run := func(ctx context.Context, opts api.RunOptions) (api.RunResult, error) {
		res, err := engine.RunWith(ctx, opts, hooks, time.Now, nil)
		runReturned.Store(true)
		return res, err
	}
	u, sigterm, done := startServe(t, ServeOptions{ConfigPath: cfg, Timer: true, Every: time.Hour}, deps{run: run, window: testWindow})
	url = u
	<-inRun
	sigterm()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	if !runReturned.Load() {
		t.Fatal("serve exited before the run returned")
	}
	if c := stoppedPost.Load(); c != 200 {
		t.Errorf("a webhook during the stopping run: %d, want 200", c)
	}
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	people, err := s.ReadTable(context.Background(), model.TablePeople)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) == 0 {
		t.Error("the stopped run saved no merged rows")
	}
	health, _ := s.ReadTable(context.Background(), model.TableHealth)
	var stopped, ranAt bool
	for _, r := range health {
		stopped = stopped || (r["kind"] == "problem" && r["key"] == "run_stopped")
		ranAt = ranAt || (r["key"] == "last_run_at" && r["value"] != "")
	}
	if !stopped || !ranAt {
		t.Errorf("Health after the stop = %v, want run_stopped and last_run_at", health)
	}
	evs, _, _ := s.ReadEvents(context.Background(), "")
	if len(evs) != 1 {
		t.Errorf("%d events stored, want the one posted while the run stopped", len(evs))
	}
}

// Without --every there is no timer, and /healthz reports only the store.
func TestServeWithoutATimer(t *testing.T) {
	cfg, _ := install(t, "")
	var calls atomic.Int32
	run := func(context.Context, api.RunOptions) (api.RunResult, error) {
		calls.Add(1)
		return api.RunResult{}, nil
	}
	url, sigterm, done := startServe(t, ServeOptions{ConfigPath: cfg}, deps{run: run, window: testWindow})
	if code := getHealthz(t, url); code != 200 {
		t.Errorf("/healthz: %d", code)
	}
	if code := postHTTP(t, url, "/apollo/visit", `{"event":"website_visited_pricing","contact":{"email":"a@example.com"}}`); code != 200 {
		t.Errorf("POST: %d", code)
	}
	sigterm()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Error("a run started without --every")
	}
}

// serve marks a SQLite file with the container that opened it, and clears
// the mark outside one; and it refuses a SQLite store inside Cloud Run, where
// stored events would be lost.
func TestServeMarksTheSQLiteStoreAndRefusesCloudRun(t *testing.T) {
	cfg, db := install(t, "")
	_, sigterm, done := startServe(t, ServeOptions{ConfigPath: cfg}, deps{})
	sigterm()
	<-done
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ReadTable(context.Background(), model.TableState)
	_ = s.Close()
	found := false
	for _, r := range rows {
		if r["key"] == "opened_by" {
			found = true
			if !sqlite.InContainer() && r["value"] != "" {
				t.Errorf("opened_by = %q outside a container, want empty", r["value"])
			}
		}
	}
	if !found {
		t.Error("serve did not record opened_by")
	}

	err = serve(context.Background(), ServeOptions{ConfigPath: cfg}, deps{getenv: env(map[string]string{"K_SERVICE": "leadscore-receiver"})})
	if err == nil || !strings.Contains(err.Error(), "Cloud Run") {
		t.Errorf("serve with SQLite on Cloud Run: %v, want a refusal", err)
	}
}

// A missing secret is logged at start, and serve still serves /healthz.
func TestServeWithoutASecretStillStarts(t *testing.T) {
	cfg, _ := install(t, "")
	var log bytes.Buffer
	var mu sync.Mutex
	url, _, _ := startServe(t, ServeOptions{ConfigPath: cfg, Log: lockedWriter{&mu, &log}}, deps{getenv: env(nil)})
	if code := getHealthz(t, url); code != 200 {
		t.Errorf("/healthz: %d", code)
	}
	if code := postHTTP(t, url, "/apollo/reply", `{"event":"email_sent","contact_email":"a@example.com"}`); code != 401 {
		t.Errorf("POST with no secret configured: %d, want 401", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(log.String(), "LEADSCORE_RECEIVER_SECRET is not set") {
		t.Errorf("no warning logged: %q", log.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
