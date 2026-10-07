package apollo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
)

const testKey = "test-key-123"

// fastRetries shrinks the backoff for one test.
func fastRetries(t *testing.T) {
	old := retryBaseWait
	retryBaseWait = time.Millisecond
	t.Cleanup(func() { retryBaseWait = old })
}

// fakeClient is a client against a fresh fake.
func fakeClient(t *testing.T) (*Client, *fakeapollo.Server) {
	t.Helper()
	fake := fakeapollo.New(testKey)
	fake.SetRetryAfter("0")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := NewClientWithKey(api.Config{"base_url": srv.URL, "_http_client": srv.Client()}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

// handlerClient is a client against a test handler.
func handlerClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClientWithKey(api.Config{"base_url": srv.URL + "/", "_http_client": srv.Client()}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClient(t *testing.T) {
	t.Setenv(KeyVariable, "")
	if _, err := NewClient(api.Config{}); err == nil || !strings.Contains(err.Error(), KeyVariable) {
		t.Errorf("no key: err = %v, want it to name %s", err, KeyVariable)
	}
	t.Setenv(KeyVariable, " k ")
	c, err := NewClient(api.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if c.key != "k" || c.baseURL != DefaultBaseURL || c.http.Timeout != CallTimeout {
		t.Errorf("defaults: key %q base %q timeout %v", c.key, c.baseURL, c.http.Timeout)
	}
	if _, err := NewClient(api.Config{"base_url": 5, "_http_client": http.DefaultClient}); err == nil {
		t.Error("a base_url that is not text was accepted")
	}
	// A typed-nil client is no client: the default is kept, and base_url
	// still needs a real one.
	var nilClient *http.Client
	if c, err := NewClient(api.Config{"_http_client": nilClient}); err != nil || c.http == nil {
		t.Errorf("a typed-nil _http_client: %v", err)
	}
	if _, err := NewClient(api.Config{"_http_client": nilClient, "base_url": "https://collector.example"}); err == nil {
		t.Error("base_url with a typed-nil client was accepted")
	}
	// base_url is for tests only: from leadscore.yml alone it would send the
	// key to any address.
	if _, err := NewClient(api.Config{"base_url": "https://collector.example"}); err == nil || !strings.Contains(err.Error(), "for tests only") {
		t.Errorf("base_url without a test client: err = %v", err)
	}
	if _, err := NewClient(api.Config{"_http_client": "x"}); err == nil {
		t.Error("an _http_client that is not a client was accepted")
	}
}

func TestEnrichOrganizationParsesTheFacts(t *testing.T) {
	c, fake := fakeClient(t)
	fake.Serve("acme-robotics.example", "found")
	org, err := c.EnrichOrganization(context.Background(), " ACME-Robotics.example ")
	if err != nil {
		t.Fatal(err)
	}
	if org.Name != "Acme Robotics" || org.Employees == nil || *org.Employees != 240 ||
		org.FundingStage != "Series C" || org.FundingDate != "2026-03-15" || org.Country != "United States" {
		t.Errorf("org = %+v", org)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Domain != "acme-robotics.example" {
		t.Errorf("calls %v: the domain is sent lowercased and trimmed", calls)
	}
}

// A missing headcount stays nil: zero would read as a company with no staff.
func TestEnrichOrganizationLeavesAnAbsentHeadcountNil(t *testing.T) {
	c, fake := fakeClient(t)
	fake.Serve("quiet-co.example", "found_sparse")
	org, err := c.EnrichOrganization(context.Background(), "quiet-co.example")
	if err != nil {
		t.Fatal(err)
	}
	if org.Employees != nil || org.Name != "Quiet Co" {
		t.Errorf("org = %+v", org)
	}
}

// A 200 with no organization is ErrNotFound; a 404 is an ordinary failure
// until S0 confirms Apollo uses it for an unknown domain.
func TestEnrichOrganizationNotFound(t *testing.T) {
	c, fake := fakeClient(t)
	fake.Serve("ghost-co.example", "status_404")
	if _, err := c.EnrichOrganization(context.Background(), "ghost-co.example"); errors.Is(err, ErrNotFound) || !IsStatus(err, 404) {
		t.Errorf("a 404: err = %v, want a failure, not ErrNotFound", err)
	}
	for _, cs := range []string{"not_found", "not_found_null"} {
		t.Run(cs, func(t *testing.T) {
			c, fake := fakeClient(t)
			fake.Serve("ghost-co.example", cs)
			if _, err := c.EnrichOrganization(context.Background(), "ghost-co.example"); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestEnrichOrganizationRetriesOnRateLimit(t *testing.T) {
	c, fake := fakeClient(t)
	fake.AddOrg("acme.example", fakeapollo.Org{Name: "Acme"})
	fake.RateLimitNext(1)
	org, err := c.EnrichOrganization(context.Background(), "acme.example")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Calls()); n != 2 || org.Name != "Acme" {
		t.Errorf("calls %d, org %+v: want one retry that succeeds", n, org)
	}
}

// A sustained rate limit gives up after the attempts, as ErrRateLimited.
func TestEnrichOrganizationGivesUpOnSustainedRateLimit(t *testing.T) {
	fastRetries(t)
	c, fake := fakeClient(t)
	fake.SetRetryAfter("")
	fake.RateLimitNext(100)
	_, err := c.EnrichOrganization(context.Background(), "busy.example")
	if !errors.Is(err, api.ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	if n := len(fake.Calls()); n != retryAttempts {
		t.Errorf("attempts = %d, want %d", n, retryAttempts)
	}
}

// A cancelled context ends the wait rather than sleeping through it.
func TestRetryHonoursContextDuringBackoff(t *testing.T) {
	c, fake := fakeClient(t)
	fake.SetRetryAfter("30")
	fake.RateLimitNext(100)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.EnrichOrganization(ctx, "slow.example")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's deadline", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the call took %v: the backoff ignored the context", d)
	}
}

func TestRetryWait(t *testing.T) {
	cases := []struct {
		header  string
		attempt int
		want    time.Duration
	}{
		{"5", 0, 5 * time.Second},
		{"0", 2, 0},               // Retry-After: 0 means at once
		{"3600", 0, retryMaxWait}, // an absurd wait is capped
		{"", 0, retryBaseWait},
		{"", 1, 2 * retryBaseWait},
		{"", 10, retryMaxWait},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0, retryBaseWait}, // a date falls back to the curve
		{"-1", 0, retryBaseWait},
		{"9223372036", 0, retryMaxWait}, // capped before multiplying: no overflow
	}
	for _, c := range cases {
		if got := retryWait(c.header, c.attempt); got != c.want {
			t.Errorf("retryWait(%q, %d) = %v, want %v", c.header, c.attempt, got, c.want)
		}
	}
}

// The single-shot call maps a 429 to ErrRateLimited at once: one request,
// no wait.
func TestDoIsSingleShot(t *testing.T) {
	var n atomic.Int32
	c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	start := time.Now()
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/anything"}, nil)
	if !errors.Is(err, api.ErrRateLimited) || n.Load() != 1 || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %d requests in %v", err, n.Load(), time.Since(start))
	}
}

// A retried POST still carries its body, and every request carries the key.
func TestRetriedPostCarriesItsBody(t *testing.T) {
	var bodies []string
	c := handlerClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.Header.Get("X-Api-Key") != testKey || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		if len(bodies) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"contact":{"id":"c1"}}`))
	})
	var out struct {
		Contact struct{ ID string } `json:"contact"`
	}
	err := c.DoRetrying(context.Background(), Request{Method: http.MethodPost, Path: "/v1/contacts",
		Body: map[string]string{"email": "ada@example.org"}}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] == "" || out.Contact.ID != "c1" {
		t.Errorf("bodies %q, out %+v", bodies, out)
	}
}

// A refusal comes back as a StatusError that keeps only the vendor's
// diagnostic fields, never an echoed email.
func TestStatusErrorIsRedacted(t *testing.T) {
	c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"bad","email":"ada@example.org"}`))
	})
	err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/v1/contacts", Body: map[string]string{}}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || !IsStatus(err, 422) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "ada@") {
		t.Errorf("the error echoes an email: %v", err)
	}
}

// A retry wait ends when the run stops new calls (WithWaitStop), though the
// call's own context is still live.
func TestRetryWaitEndsOnWaitStop(t *testing.T) {
	c, fake := fakeClient(t)
	fake.SetRetryAfter("30")
	fake.RateLimitNext(100)
	stop, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.EnrichOrganization(WithWaitStop(context.Background(), stop), "slow.example")
	if !errors.Is(err, ErrWaitStopped) || errors.Is(err, api.ErrRateLimited) || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %v, want ErrWaitStopped (not a rate limit) at once", err, time.Since(start))
	}
}

// A transport error never carries the URL's query, which may hold an email.
func TestTransportErrorHidesTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c, err := NewClientWithKey(api.Config{"base_url": base, "_http_client": &http.Client{}}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/contacts/search",
		Query: url.Values{"q_keywords": {"ada.lovelace@example.org"}}}, nil)
	if err == nil {
		t.Fatal("a closed server answered")
	}
	for _, leak := range []string{"ada", "%40", "example.org", "q_keywords"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error %q holds %q", err, leak)
		}
	}
}

// The key never follows a redirect: a 3xx is the reply.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer other.Close()
	c := handlerClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/auth/health"}, nil)
	if !IsStatus(err, http.StatusFound) || hit.Load() {
		t.Errorf("err %v, redirect followed %v", err, hit.Load())
	}
}

// StatusError keeps the raw body for callers that tell refusals apart.
func TestStatusErrorBody(t *testing.T) {
	c := handlerClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Contact is already in an active sequence"}`))
	})
	err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/v1/x"}, nil)
	var se *StatusError
	if !errors.As(err, &se) || !strings.Contains(string(se.Body()), "active sequence") {
		t.Errorf("err %v", err)
	}
}

func TestAuthHealth(t *testing.T) {
	c, fake := fakeClient(t)
	if err := c.AuthHealth(context.Background()); err != nil {
		t.Errorf("a good key: %v", err)
	}
	bad, err := NewClientWithKey(api.Config{"base_url": c.baseURL, "_http_client": c.http}, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.AuthHealth(context.Background()); !IsStatus(err, http.StatusUnauthorized) {
		t.Errorf("a bad key: err = %v, want a 401", err)
	}
	for _, call := range fake.Calls() {
		if call.Path != "/v1/auth/health" {
			t.Errorf("the auth check called %s", call.Path)
		}
	}
	fake.ServeAuth("not_logged_in")
	if err := c.AuthHealth(context.Background()); !errors.Is(err, ErrKeyRefused) || !KeyRefused(err) {
		t.Errorf("is_logged_in false: err = %v", err)
	}
}

func TestEnricherBudgetAndNotFound(t *testing.T) {
	c, fake := fakeClient(t)
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A", Employees: ptr(12), FundingStage: "Seed", FundingDate: "2025-01-02", Country: " Germany "})
	e := &Enricher{c: c, now: time.Now}
	got, err := e.Enrich(context.Background(), []string{"a.example", "ghost.example", "c.example"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(fake.Calls()) != 2 {
		t.Fatalf("got %+v after %d calls: the budget is 2", got, len(fake.Calls()))
	}
	a := got[0]
	if a.Domain != "a.example" || a.Name != "A" || *a.Employees != 12 || a.FundingStage != "seed" ||
		a.Region != "Germany" || a.Extra[FactLatestFundingAt] != "2025-01-02" || a.NotFound {
		t.Errorf("a.example = %+v", a)
	}
	if !got[1].NotFound || got[1].Domain != "ghost.example" {
		t.Errorf("ghost.example = %+v, want NotFound", got[1])
	}
}

// Proof: a rate limit returns the facts fetched before it, with
// ErrRateLimited, and tries nothing after it.
func TestEnricherPartialFactsOnRateLimit(t *testing.T) {
	fastRetries(t)
	c, fake := fakeClient(t)
	fake.AddOrg("a.example", fakeapollo.Org{Name: "A"})
	fake.AddOrg("b.example", fakeapollo.Org{Name: "B"})
	fake.RateLimitAfter(1, 100)
	e := &Enricher{c: c, now: time.Now}
	got, err := e.Enrich(context.Background(), []string{"a.example", "b.example", "c.example"}, 10)
	if !errors.Is(err, api.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if len(got) != 1 || got[0].Name != "A" {
		t.Errorf("facts so far = %+v, want a.example's", got)
	}
	for _, d := range fake.EnrichCalls() {
		if d == "c.example" {
			t.Error("a domain after the rate limit was tried")
		}
	}
}

// A failure on one domain is skipped; a refused key stops the call.
func TestEnricherSkipsAFailureAndStopsOnABadKey(t *testing.T) {
	var n atomic.Int32
	c := handlerClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		switch r.URL.Query().Get("domain") {
		case "down.example":
			w.WriteHeader(http.StatusBadGateway)
		case "ok.example":
			_, _ = w.Write([]byte(`{"organization":{"name":"OK"}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	e := &Enricher{c: c, now: time.Now}
	got, err := e.Enrich(context.Background(), []string{"down.example", "ok.example", "locked.example", "never.example"}, 10)
	if !IsStatus(err, http.StatusUnauthorized) {
		t.Errorf("err = %v, want the 401", err)
	}
	if len(got) != 1 || got[0].Name != "OK" || n.Load() != 3 {
		t.Errorf("got %+v after %d calls", got, n.Load())
	}
}

// Three failures in a row stop the call: every later domain would likely
// fail too.
func TestEnricherStopsAfterFailuresInARow(t *testing.T) {
	c, fake := fakeClient(t)
	for _, d := range []string{"bad1.example", "bad2.example", "bad3.example"} {
		fake.Serve(d, "server_error")
	}
	fake.AddOrg("good.example", fakeapollo.Org{Name: "Good"})
	e := &Enricher{c: c, now: time.Now}
	got, err := e.Enrich(context.Background(), []string{"bad1.example", "bad2.example", "bad3.example", "good.example"}, 10)
	if err == nil || len(got) != 0 || len(fake.Calls()) != MaxFailuresInARow {
		t.Errorf("got %v, err %v after %d calls", got, err, len(fake.Calls()))
	}
}

// An unknown funding label clears the stored stage; a missing one keeps it.
func TestUnknownFundingLabelClears(t *testing.T) {
	f := CompanyFromOrganization("a.example", &Organization{FundingStage: "Angel", FundingDate: "soon"}, time.Now())
	if v, ok := f.Extra[FactFundingStage]; !ok || v != "" || f.FundingStage != "" {
		t.Errorf("unknown label: %+v", f)
	}
	if v, ok := f.Extra[FactLatestFundingAt]; !ok || v != "" {
		t.Errorf("unreadable date: %+v", f)
	}
	f = CompanyFromOrganization("a.example", &Organization{}, time.Now())
	if _, ok := f.Extra[FactFundingStage]; ok {
		t.Errorf("a missing label clears: %+v", f)
	}
}

func TestFundingStage(t *testing.T) {
	for label, want := range map[string]string{
		"Pre-Seed": "pre_seed", "pre seed": "pre_seed", " Seed ": "seed", "Series A": "series_a",
		"series b": "series_b", "Series C": "series_c", "Series D": "series_d_plus", "Series J": "series_d_plus",
		"Angel": "", "Private Equity": "", "": "",
	} {
		if got := FundingStage(label); got != want {
			t.Errorf("FundingStage(%q) = %q, want %q", label, got, want)
		}
	}
	for in, want := range map[string]string{
		"2026-03-15": "2026-03-15", "2026-03-15T23:30:00-05:00": "2026-03-16", "March 2026": "", "": "",
	} {
		if got := fundingDate(in); got != want {
			t.Errorf("fundingDate(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every fixture is in S0's shape and marked provisional until S0 replaces it.
func TestFixturesAreInS0Shape(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fakeapollo.Dir(), "*", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		rel, _ := filepath.Rel(fakeapollo.Dir(), f)
		fx, err := fakeapollo.Load(strings.TrimSuffix(filepath.ToSlash(rel), ".json"))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if !fx.Provisional || fx.Method == "" || fx.Path == "" || fx.Status == 0 || len(fx.ResponseBody) == 0 {
			t.Errorf("%s: %+v", rel, fx)
		}
		raw, _ := os.ReadFile(f)
		if strings.Contains(strings.ToLower(string(raw)), testKey) {
			t.Errorf("%s holds a key", rel)
		}
	}
}

func ptr(n int) *int { return &n }

// A context that ended stops the enricher with its own wording, not as
// failures in a row.
func TestEnricherStopsWhenCtxEnds(t *testing.T) {
	c := handlerClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := (&Enricher{c: c, now: time.Now}).Enrich(ctx, []string{"a.example", "b.example"}, 10)
	if err == nil || strings.Contains(err.Error(), "in a row") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}
