package apollo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// The client (contracts sections 6 and 12.1). Everything this package sends
// to Apollo goes through a Client: the enricher here, and the sinks, the
// contact Lookup and the reply Poller. It has two call modes:
//
//   - Do is single-shot: a 429 returns ErrRateLimited at once, with no sleep.
//     Sinks, lookups and the poller use it, since a push step that waits
//     would hold up the batch, and the ledger retries it next run.
//   - DoRetrying retries a 429 (and only a 429) up to three attempts, waiting
//     for Retry-After or a doubling backoff. Enrichment uses it.
//
// Every attempt has a 30-second timeout, whatever HTTP client the block gives.

// KeyVariable is the environment variable holding the Apollo API key.
const KeyVariable = "APOLLO_API_KEY"

// DefaultBaseURL is Apollo's API; a block's base_url replaces it in tests.
const DefaultBaseURL = "https://api.apollo.io"

// CallTimeout bounds one attempt of one call (contracts section 11).
const CallTimeout = 30 * time.Second

// Paths of the calls this file makes. S0 confirms: the auth-health path and
// that it costs no credits.
const authHealthPath = "/v1/auth/health"

// maxReplyBytes caps a reply body read into memory. Apollo's largest replies
// (an organization record, a page of contacts) are well under it.
const maxReplyBytes = 4 << 20

// Retry settings for DoRetrying. Apollo's documented ceiling is about 600
// calls an hour, so a 429 does not clear in milliseconds; three attempts keep
// a run moving rather than parked on one call. Variables so tests can shrink
// the waits.
var (
	retryAttempts = 3
	retryBaseWait = 2 * time.Second
	retryMaxWait  = 30 * time.Second
)

// Client calls Apollo's REST API with one key.
type Client struct {
	key     string
	baseURL string
	http    *http.Client
}

// NewClient builds a client from an adapter block (enrich, or sinks.apollo).
// The key comes from APOLLO_API_KEY; the block's test keys base_url and
// _http_client (contracts section 3) replace the API address and the HTTP
// client. A missing key is an error, so no call goes out unauthenticated, and
// base_url without _http_client is refused: base_url is for tests only, and
// one written into leadscore.yml would send the key to any address.
func NewClient(cfg api.Config) (*Client, error) {
	return NewClientWithKey(cfg, os.Getenv(KeyVariable))
}

// NewClientWithKey is NewClient with the key given, for a caller that reads
// it elsewhere (Secret Manager on a hosted install).
func NewClientWithKey(cfg api.Config, key string) (*Client, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("apollo: %s is not set", KeyVariable)
	}
	hc := &http.Client{Timeout: CallTimeout}
	testClient := false
	if v, ok := cfg["_http_client"]; ok && v != nil {
		given, isClient := v.(*http.Client)
		if !isClient {
			return nil, errors.New("apollo: `_http_client` must be an *http.Client")
		}
		if given != nil { // a typed nil is no client: the default is kept
			copied := *given // a copy, so the redirect rule below leaves the caller's client alone
			hc, testClient = &copied, true
		}
	}
	// The key header must never follow a redirect to another host: a 3xx is
	// returned as the reply, which is then a StatusError.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{key: key, baseURL: DefaultBaseURL, http: hc}
	if v, ok := cfg["base_url"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || s == "" {
			return nil, errors.New("apollo: `base_url` must be a URL")
		}
		if !testClient {
			return nil, errors.New("apollo: `base_url` is for tests only and needs a test HTTP client; remove it from leadscore.yml")
		}
		c.baseURL = strings.TrimRight(s, "/")
	}
	return c, nil
}

// Request is one call: a path under the base URL, an optional query, and an
// optional body sent as JSON.
type Request struct {
	Method string
	Path   string // for example /api/v1/organizations/enrich
	Query  url.Values
	Body   any // JSON-encoded when non-nil
}

// StatusError is a reply outside 2xx other than 429. Detail holds only the
// vendor's machine-issued diagnostic fields (logredact.VendorErrorDetail),
// never the body, which may echo a person's email back.
type StatusError struct {
	Status int
	Detail string
	body   []byte
}

// Body is the reply's raw body (at most 4 MB), for telling refusal reasons
// apart. Never log it: it may hold a person's email.
func (e *StatusError) Body() []byte { return e.body }

func (e *StatusError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("apollo returned %d", e.Status)
	}
	return fmt.Sprintf("apollo returned %d %s", e.Status, e.Detail)
}

// IsStatus reports whether err is a StatusError with one of the codes.
func IsStatus(err error, codes ...int) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Status == c {
			return true
		}
	}
	return false
}

// Do sends the request once. A 2xx reply's JSON body is decoded into out
// (when out is not nil). A 429 returns an error wrapping api.ErrRateLimited,
// with no wait; any other non-2xx reply returns a *StatusError; a transport
// failure or timeout is returned wrapped, so errors.Is finds
// context.DeadlineExceeded.
func (c *Client) Do(ctx context.Context, req Request, out any) error {
	resp, err := c.attempt(ctx, req)
	if err != nil {
		return err
	}
	return c.finish(resp, out)
}

// DoRetrying is Do, but a 429 is retried: up to three attempts in all,
// waiting the reply's Retry-After (seconds, capped at 30) or a backoff of 2
// seconds doubling per attempt. A 429 on the last attempt returns an error
// wrapping api.ErrRateLimited. Only a 429 is retried: another 4xx fails the
// same way every time, and a 5xx during a bulk run is more likely an outage
// than a blip. The wait ends early when ctx is done.
func (c *Client) DoRetrying(ctx context.Context, req Request, out any) error {
	for n := 1; ; n++ {
		resp, err := c.attempt(ctx, req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusTooManyRequests || n >= retryAttempts {
			return c.finish(resp, out)
		}
		wait := retryWait(resp.Header.Get("Retry-After"), n-1)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("apollo: waiting to retry a rate-limited call: %w", ctx.Err())
		case <-waitStop(ctx):
			t.Stop()
			return ErrWaitStopped
		case <-t.C:
		}
	}
}

// attempt sends one request under the per-call timeout. The body is read
// before the timeout's context is cancelled, so the reply is complete.
func (c *Client) attempt(ctx context.Context, req Request) (*http.Response, error) {
	var body io.Reader
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("apollo: encoding the request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	u := c.baseURL + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("apollo: building the request: %w", err)
	}
	hr.Header.Set("X-Api-Key", c.key) // S0 confirms: the key goes in this header
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Cache-Control", "no-cache")
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(hr) //nolint:gosec // the URL is the configured base plus a fixed path
	if err != nil {
		// A *url.Error's text holds the whole URL, query included, and a
		// query may carry an email; report only its operation and cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("%s: %w", ue.Op, ue.Err)
		}
		return nil, fmt.Errorf("apollo: %s %s: %w", req.Method, req.Path, err)
	}
	// Read the whole reply now, while the attempt's context is live.
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("apollo: %s %s: reading the reply: %w", req.Method, req.Path, rerr)
	}
	if len(raw) > maxReplyBytes {
		return nil, fmt.Errorf("apollo: %s %s: the reply is over %d bytes", req.Method, req.Path, maxReplyBytes)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

// finish turns a reply into the call's result.
func (c *Client) finish(resp *http.Response, out any) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body) // already in memory
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("apollo: %w (429)", api.ErrRateLimited)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &StatusError{Status: resp.StatusCode, Detail: logredact.VendorErrorDetail(raw), body: raw}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("apollo: decoding the reply: %w", err)
	}
	return nil
}

// retryWait is how long to wait before the next attempt: the reply's
// Retry-After in seconds (0 means at once), capped at retryMaxWait; else
// retryBaseWait doubled per attempt already made, with the same cap. An HTTP
// date in Retry-After is not parsed and falls back to the backoff.
func retryWait(retryAfter string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs >= 0 {
		if secs >= int(retryMaxWait/time.Second) {
			return retryMaxWait // capped before multiplying, so a huge value cannot overflow
		}
		return time.Duration(secs) * time.Second
	}
	return min(retryBaseWait<<attempt, retryMaxWait)
}

// ErrKeyRefused is AuthHealth's answer when the call worked but the key does
// not sign in (is_logged_in false).
var ErrKeyRefused = errors.New("apollo: the key does not sign in (is_logged_in is false)")

// KeyRefused reports whether err says the key itself is bad: a 401 or 403,
// or ErrKeyRefused. S0 confirms which of 401 and 403 Apollo uses for a bad
// key; both count.
func KeyRefused(err error) bool {
	return errors.Is(err, ErrKeyRefused) || IsStatus(err, http.StatusUnauthorized, http.StatusForbidden)
}

// AuthHealth calls Apollo's free auth-health endpoint and reports whether the
// key signs in. It never spends a credit (S0 confirms), which is why the
// apollo-key check uses it rather than an enrichment call. S0 confirms the
// reply shape: a bad key may come back as 401, or as 200 with is_logged_in
// false (ErrKeyRefused); both read as not signed in.
func (c *Client) AuthHealth(ctx context.Context) error {
	var reply struct {
		IsLoggedIn *bool `json:"is_logged_in"`
	}
	err := c.Do(ctx, Request{Method: http.MethodGet, Path: authHealthPath}, &reply)
	if err != nil {
		return err
	}
	if reply.IsLoggedIn != nil && !*reply.IsLoggedIn {
		return ErrKeyRefused
	}
	return nil
}

// ContactOptOutFlag says whether Apollo's contact record carries an opt-out
// flag that a lookup by email can read without spending credits (contracts
// section 6). S0 confirms: until it does, this is false, the safe default:
// the apollo-key check warns teams that send only through Apollo that a
// person who clicked an unsubscribe link without replying is not seen.
const ContactOptOutFlag = false

// ErrWaitStopped is DoRetrying's answer when the WithWaitStop context ended
// during a retry wait: the run stopped new calls (Stop or the deadline). It is
// not a rate limit and not a failure of the call.
var ErrWaitStopped = errors.New("apollo: the run stopped new calls while a rate-limited call waited to retry")

type waitStopKey struct{}

// WithWaitStop returns ctx carrying stop: DoRetrying gives up waiting for a
// retry when stop is done, returning ErrWaitStopped, while a call already in
// flight still runs under ctx. The engine passes the run's push context
// (done at Stop or the deadline), which ends new calls but not open ones.
func WithWaitStop(ctx, stop context.Context) context.Context {
	return context.WithValue(ctx, waitStopKey{}, stop)
}

// waitStop is the stop channel set by WithWaitStop, or nil (never ready).
func waitStop(ctx context.Context) <-chan struct{} {
	if stop, ok := ctx.Value(waitStopKey{}).(context.Context); ok {
		return stop.Done()
	}
	return nil
}
