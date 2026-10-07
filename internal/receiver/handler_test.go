package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

const (
	testSecret     = "current-receiver-secret"
	previousSecret = "previous-receiver-secret"
	goldenDir      = "../../testdata/events"
)

// fastQueue shrinks the batch window so tests do not wait two seconds.
func fastQueue(t *testing.T, window time.Duration) {
	t.Helper()
	oldW, oldC := batchWindow, holdCap
	batchWindow = window
	t.Cleanup(func() { batchWindow, holdCap = oldW, oldC })
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

type testStore struct {
	name   string
	store  api.Backend
	events api.EventLog
	sheets *fakesheets.Server // nil on SQLite
}

func sqliteStore(t *testing.T) testStore {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "leadscore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return testStore{name: "sqlite", store: s, events: s}
}

func sheetsStore(t *testing.T) testStore {
	t.Helper()
	fs, fg := fakesheets.New(), gcs.New()
	srv := httptest.NewServer(gcs.Route(fg, fs))
	t.Cleanup(srv.Close)
	fg.CreateBucket("lease")
	s, err := sheets.Open(t.Context(), api.Config{"type": "sheets", "spreadsheet": fs.NewSpreadsheet("leadscore"),
		"lease_bucket": "lease", "base_url": srv.URL, "_http_client": srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return testStore{name: "sheets", store: s, events: s, sheets: fs}
}

func bothStores(t *testing.T) []testStore {
	return []testStore{sqliteStore(t), sheetsStore(t)}
}

var fixedNow = time.Date(2026, 9, 14, 8, 30, 0, 0, time.UTC)

func newTestHandler(t *testing.T, ts testStore, vars map[string]string) *Handler {
	t.Helper()
	if vars == nil {
		vars = map[string]string{SecretVar: testSecret}
	}
	h := NewHandler(Options{Store: ts.store, Events: ts.events, Now: func() time.Time { return fixedNow }, Getenv: env(vars)})
	t.Cleanup(h.Close)
	return h
}

func post(h http.Handler, path string, body []byte, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if header != "" {
		req.Header.Set(SecretHeader, header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readAll(t *testing.T, ev api.EventLog) []api.RawEvent {
	t.Helper()
	got, _, err := ev.ReadEvents(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// routeOf is the route a golden file's name says stores it.
func routeOf(name string) (path, kind string) {
	if strings.HasPrefix(name, "apollo_visit_") {
		return "/apollo/visit", apollo.KindVisit
	}
	return "/apollo/reply", apollo.KindReply
}

type parseResult struct {
	Events []api.Event
	Rows   []api.InputRow
	Err    string
}

func parseResultOf(raw api.RawEvent) parseResult {
	evs, rows, err := apollo.ParseRaw(raw)
	r := parseResult{Events: evs, Rows: rows}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

// The service contract (RFC 8.2): every golden body, posted with the secret
// in the header or in the body, is stored with the secret stripped and parsed
// exactly as the body itself parses, on both stores.
func TestGoldenBodiesStoredAndParsedTheSameOnBothStores(t *testing.T) {
	fastQueue(t, 10*time.Millisecond)
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden bodies in %s: %v", goldenDir, err)
	}
	byStore := map[string][]parseResult{}
	for _, ts := range bothStores(t) {
		h := newTestHandler(t, ts, nil)
		var want []parseResult
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			path, kind := routeOf(filepath.Base(f))
			expect := parseResultOf(api.RawEvent{Kind: kind, ReceivedAt: fixedNow, Body: body})
			if rec := post(h, path, body, testSecret); rec.Code != http.StatusOK {
				t.Fatalf("%s %s (header): %d %s", ts.name, filepath.Base(f), rec.Code, rec.Body)
			}
			// The same body with the secret in a top-level field instead.
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			m[secretField] = testSecret
			withSecret, _ := json.Marshal(m)
			if rec := post(h, path, withSecret, ""); rec.Code != http.StatusOK {
				t.Fatalf("%s %s (body field): %d %s", ts.name, filepath.Base(f), rec.Code, rec.Body)
			}
			want = append(want, expect, expect)
		}
		stored := readAll(t, ts.events)
		if len(stored) != len(want) {
			t.Fatalf("%s: %d events stored, want %d", ts.name, len(stored), len(want))
		}
		for i, raw := range stored {
			if bytes.Contains(raw.Body, []byte(testSecret)) || bytes.Contains(raw.Body, []byte(secretField)) {
				t.Errorf("%s: event %d stored the secret: %s", ts.name, i, raw.Body)
			}
			if !raw.ReceivedAt.Equal(fixedNow) {
				t.Errorf("%s: event %d received at %v, want the handler's clock", ts.name, i, raw.ReceivedAt)
			}
			got := parseResultOf(raw)
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("%s: event %d (%s) parses differently once stored:\n got %+v\nwant %+v", ts.name, i, files[i/2], got, want[i])
			}
			byStore[ts.name] = append(byStore[ts.name], got)
		}
	}
	if !reflect.DeepEqual(byStore["sqlite"], byStore["sheets"]) {
		t.Error("the two stores give different parses of the same requests")
	}
}

// A wrong, missing or unset secret is refused with 401 and nothing is stored.
// While a rotation is under way, the previous secret is accepted too.
func TestTheSecretIsCheckedWithRotation(t *testing.T) {
	fastQueue(t, 5*time.Millisecond)
	body := []byte(`{"event":"email_sent","contact_email":"a@example.com"}`)
	cases := []struct {
		name   string
		vars   map[string]string
		header string
		field  string
		want   int
	}{
		{"current in header", map[string]string{SecretVar: testSecret}, testSecret, "", 200},
		{"current in body", map[string]string{SecretVar: testSecret}, "", testSecret, 200},
		{"wrong in header", map[string]string{SecretVar: testSecret}, "nope", "", 401},
		{"wrong in body", map[string]string{SecretVar: testSecret}, "", "nope", 401},
		{"wrong header, right body", map[string]string{SecretVar: testSecret}, "nope", testSecret, 401},
		{"missing", map[string]string{SecretVar: testSecret}, "", "", 401},
		{"previous during rotation", map[string]string{SecretVar: testSecret, PreviousSecretVar: previousSecret}, previousSecret, "", 200},
		{"previous after rotation", map[string]string{SecretVar: testSecret}, previousSecret, "", 401},
		{"no secret configured", map[string]string{}, "", "", 401},
		{"no secret configured, any proof", map[string]string{}, "anything", "", 401},
		{"only a previous secret", map[string]string{PreviousSecretVar: previousSecret}, previousSecret, "", 200},
	}
	for _, c := range cases {
		ts := sqliteStore(t)
		h := newTestHandler(t, ts, c.vars)
		b := body
		if c.field != "" {
			b = []byte(`{"event":"email_sent","contact_email":"a@example.com","leadscore_secret":"` + c.field + `"}`)
		}
		rec := post(h, "/apollo/reply", b, c.header)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
		n := len(readAll(t, ts.events))
		if c.want == 401 && n != 0 {
			t.Errorf("%s: a refused request was stored", c.name)
		}
		if c.want == 200 && n != 1 {
			t.Errorf("%s: %d events stored, want 1", c.name, n)
		}
		if c.want == 401 && strings.Contains(rec.Body.String(), "match") {
			t.Errorf("%s: the refusal says why: %q", c.name, rec.Body)
		}
	}
}

func TestRoutesAndMethods(t *testing.T) {
	fastQueue(t, 5*time.Millisecond)
	h := newTestHandler(t, sqliteStore(t), nil)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/apollo/reply", 405}, {"PUT", "/apollo/visit", 405}, {"POST", "/healthz", 405},
		{"POST", "/apollo/other", 404}, {"GET", "/", 404}, {"GET", "/healthz", 200}, {"HEAD", "/healthz", 200},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	big := bytes.Repeat([]byte("x"), maxRequestBytes+1)
	if rec := post(h, "/apollo/reply", big, testSecret); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the read limit: %d, want 413", rec.Code)
	}
}

// Oversized bodies are cut down through the handler too, and fit the Sheets
// store's cell.
func TestOversizedBodiesAreCutDownOnBothStores(t *testing.T) {
	fastQueue(t, 5*time.Millisecond)
	body, _ := json.Marshal(map[string]any{
		"event": "email_unsubscribed", "contact_email": "sam@example.org",
		"past_conversations": strings.Repeat("x", 300<<10), "other": strings.Repeat("y", 15<<10),
		"more": strings.Repeat("z", 15<<10), "and_more": strings.Repeat("w", 15<<10),
	})
	for _, ts := range bothStores(t) {
		h := newTestHandler(t, ts, nil)
		if rec := post(h, "/apollo/reply", body, testSecret); rec.Code != 200 {
			t.Fatalf("%s: %d %s", ts.name, rec.Code, rec.Body)
		}
		got := readAll(t, ts.events)
		if len(got) != 1 {
			t.Fatalf("%s: %d stored", ts.name, len(got))
		}
		evs, _, err := apollo.ParseRaw(got[0])
		if err != nil || len(evs) != 1 || evs[0].Kind != "unsubscribed" || evs[0].Email != "sam@example.org" {
			t.Errorf("%s: the cut-down opt-out parses as %+v, %v", ts.name, evs, err)
		}
	}
}

// slowLog is an EventLog whose appends wait for release, counting calls.
type slowLog struct {
	release chan struct{}
	calls   atomic.Int32
	sizes   []int
	mu      sync.Mutex
	err     error
	stored  []api.RawEvent
}

func (s *slowLog) AppendEvents(ctx context.Context, evs []api.RawEvent) error {
	s.calls.Add(1)
	s.mu.Lock()
	s.sizes = append(s.sizes, len(evs))
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.stored = append(s.stored, evs...)
	s.mu.Unlock()
	return nil
}
func (s *slowLog) ReadEvents(context.Context, api.Cursor) ([]api.RawEvent, api.Cursor, error) {
	return nil, "", nil
}
func (s *slowLog) DeleteProcessed(_ context.Context, c api.Cursor, _ time.Time) (api.Cursor, error) {
	return c, nil
}

func handlerOn(t *testing.T, ev api.EventLog) *Handler {
	h := NewHandler(Options{Events: ev, Getenv: env(map[string]string{SecretVar: testSecret})})
	t.Cleanup(h.Close)
	return h
}

// The safety rule: no request is answered before its event is durable, and
// requests arriving within the window share one append.
func TestNoAnswerBeforeTheEventIsStored(t *testing.T) {
	fastQueue(t, 50*time.Millisecond)
	log := &slowLog{release: make(chan struct{})}
	h := handlerOn(t, log)
	var wg sync.WaitGroup
	codes := make([]int, 5)
	answered := atomic.Int32{}
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = post(h, "/apollo/reply", []byte(`{"event":"email_sent","contact_email":"a@example.com"}`), testSecret).Code
			answered.Add(1)
		}()
	}
	time.Sleep(300 * time.Millisecond) // well past the window: the append is waiting
	if n := answered.Load(); n != 0 {
		t.Fatalf("%d requests were answered before the store returned", n)
	}
	close(log.release)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d: %d after the store returned", i, c)
		}
	}
	if n := log.calls.Load(); n != 1 {
		t.Errorf("%d appends for one window's requests, want 1 (sizes %v)", n, log.sizes)
	}
}

// When the append fails, every request in the batch gets 5xx, and /healthz
// without a timer turns 503 until an append succeeds.
func TestAFailedAppendAnswers5xxToTheWholeBatch(t *testing.T) {
	fastQueue(t, 20*time.Millisecond)
	log := &slowLog{release: make(chan struct{}), err: errors.New("store down")}
	close(log.release)
	h := handlerOn(t, log)
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = post(h, "/apollo/visit", []byte(`{"event":"website_visited_x","contact":{"email":"a@example.com"}}`), testSecret).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c < 500 {
			t.Errorf("request %d: %d, want 5xx", i, c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Errorf("/healthz after a failed append: %d, want 503", rec.Code)
	}
	log.err = nil
	if c := post(h, "/apollo/reply", []byte(`{"event":"email_sent","contact_email":"a@example.com"}`), testSecret).Code; c != 200 {
		t.Fatalf("after recovery: %d", c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("/healthz after a good append: %d, want 200", rec.Code)
	}
}

// An append still running at the hold cap gets its context ended, and the
// requests get 5xx so Apollo can retry; none is answered 2xx.
func TestTheHoldCapAnswers5xx(t *testing.T) {
	fastQueue(t, 10*time.Millisecond)
	holdCap = 200 * time.Millisecond
	log := &slowLog{release: make(chan struct{})} // never released
	h := handlerOn(t, log)
	start := time.Now()
	code := post(h, "/apollo/reply", []byte(`{"event":"email_sent","contact_email":"a@example.com"}`), testSecret).Code
	if code != http.StatusServiceUnavailable {
		t.Errorf("past the hold cap: %d, want 503", code)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("answered after %v, want close to the cap", d)
	}
}

// The Sheets store's "slow down" answers are retried inside the hold cap; the
// request is answered 2xx once the append lands.
func TestSheetsSlowDownIsRetriedWithinTheCap(t *testing.T) {
	fastQueue(t, 10*time.Millisecond)
	ts := sheetsStore(t)
	h := newTestHandler(t, ts, nil)
	ts.sheets.SlowDown(2)
	rec := post(h, "/apollo/reply", []byte(`{"event":"email_sent","contact_email":"a@example.com"}`), testSecret)
	if rec.Code != 200 {
		t.Fatalf("after slow-down answers: %d %s", rec.Code, rec.Body)
	}
	if n := len(readAll(t, ts.events)); n != 1 {
		t.Errorf("%d events stored, want 1", n)
	}
}

// Close stores what was accepted and refuses later requests with 503.
func TestCloseDrainsTheQueue(t *testing.T) {
	fastQueue(t, time.Hour) // only Close sends the batch
	ts := sqliteStore(t)
	h := NewHandler(Options{Store: ts.store, Events: ts.events, Getenv: env(map[string]string{SecretVar: testSecret})})
	done := make(chan int)
	go func() {
		done <- post(h, "/apollo/reply", []byte(`{"event":"email_sent","contact_email":"a@example.com"}`), testSecret).Code
	}()
	waitFor(t, func() bool { h.q.mu.Lock(); defer h.q.mu.Unlock(); return h.q.pending != nil })
	h.Close()
	if c := <-done; c != 200 {
		t.Errorf("an accepted request at shutdown: %d, want 200", c)
	}
	if n := len(readAll(t, ts.events)); n != 1 {
		t.Errorf("%d stored after the drain, want 1", n)
	}
	if c := post(h, "/apollo/reply", []byte(`{}`), testSecret).Code; c != 503 {
		t.Errorf("a request after Close: %d, want 503", c)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// putHealth writes Health result rows as a run would.
func putHealth(t *testing.T, b api.Backend, results map[string]string) {
	t.Helper()
	var rows []api.Row
	for k, v := range results {
		rows = append(rows, api.Row{"kind": "result", "key": k, "value": v, "first_seen_at": "", "updated_at": ""})
	}
	if err := b.Commit(context.Background(), []api.TableWrite{{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"}, Rows: rows}}); err != nil {
		t.Fatal(err)
	}
}

func healthCode(h *Handler) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

// /healthz with a timer (contracts section 5.1).
func TestHealthzWithATimer(t *testing.T) {
	every := 15 * time.Minute
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		results map[string]string
		at      time.Duration // after serve started
		want    int
	}{
		{"no run yet, none due", nil, 10 * time.Minute, 200},
		{"no success in three intervals since start", nil, 46 * time.Minute, 503},
		{"last run healthy and recent", map[string]string{"last_result": "healthy", "last_success_at": model.FormatTime(start.Add(-20 * time.Minute))}, 10 * time.Minute, 200},
		{"last run failed", map[string]string{"last_result": "unhealthy", "last_success_at": model.FormatTime(start)}, time.Minute, 503},
		{"last success too old", map[string]string{"last_result": "healthy", "last_success_at": model.FormatTime(start.Add(-time.Hour))}, time.Minute, 503},
		{"healthy runs that never scored, three intervals on", map[string]string{"last_result": "healthy"}, 50 * time.Minute, 503},
	}
	for _, c := range cases {
		ts := sqliteStore(t)
		if c.results != nil {
			putHealth(t, ts.store, c.results)
		}
		now := start.Add(c.at)
		h := NewHandler(Options{Store: ts.store, Events: ts.events, Now: func() time.Time { return now }, Every: every, Started: start})
		if code, body := healthCode(h); code != c.want {
			t.Errorf("%s: %d %q, want %d", c.name, code, body, c.want)
		}
		h.Close()
	}
}

// The verdict is cached for 60 seconds; a run finishing drops the cache, and
// a run that failed (an error or a panic) is 503 whatever the store says.
func TestHealthzCacheAndRunReports(t *testing.T) {
	start := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	now := start
	ts := sqliteStore(t)
	h := NewHandler(Options{Store: ts.store, Events: ts.events, Now: func() time.Time { return now }, Every: time.Minute, Started: start})
	t.Cleanup(h.Close)
	if code, _ := healthCode(h); code != 200 {
		t.Fatalf("fresh: %d", code)
	}
	putHealth(t, ts.store, map[string]string{"last_result": "unhealthy"})
	now = start.Add(30 * time.Second)
	if code, _ := healthCode(h); code != 200 {
		t.Errorf("within the cache: %d, want the cached 200", code)
	}
	now = start.Add(61 * time.Second)
	if code, _ := healthCode(h); code != 503 {
		t.Errorf("after the cache: %d, want 503", code)
	}
	putHealth(t, ts.store, map[string]string{"last_result": "healthy", "last_success_at": model.FormatTime(now)})
	h.RunFinished(false)
	if code, _ := healthCode(h); code != 200 {
		t.Errorf("after a run finished: %d, want a fresh read", code)
	}
	h.RunFinished(true)
	if code, _ := healthCode(h); code != 503 {
		t.Errorf("after a failed run: %d, want 503", code)
	}
}
