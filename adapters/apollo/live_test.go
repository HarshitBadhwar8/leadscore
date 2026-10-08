package apollo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// TestLiveApolloReadOnly is the read-only half of S0's Apollo check
// (LEADSCORE_LIVE_APOLLO names an env file holding APOLLO_API_KEY and
// APOLLO_MAILBOX_ID). It runs the real Client against Apollo: auth health, a
// refused key, the mailbox list, the sequence search, the reply search (its
// date filter and paging) and one error shape, and checks what the adapter
// relies on.
//
// It never writes: a guard transport sends only the GETs and searches listed
// in liveAllowed, at most liveMaxCalls calls, one a second. Raw replies hold
// real people's data, so they are saved only under ~/leadscore-live/captures
// (directory 0700, files 0600) and the log shows only field names, types,
// counts, status codes and rate-limit headers. The key is read from the file
// into this process alone and sent only in the X-Api-Key header.
func TestLiveApolloReadOnly(t *testing.T) {
	envFile := os.Getenv("LEADSCORE_LIVE_APOLLO")
	if envFile == "" {
		t.Skip("set LEADSCORE_LIVE_APOLLO to an env file with APOLLO_API_KEY and APOLLO_MAILBOX_ID to run read-only calls against real Apollo")
	}
	env, err := readEnvFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if env[KeyVariable] == "" || env["APOLLO_MAILBOX_ID"] == "" {
		t.Fatalf("%s must set %s and APOLLO_MAILBOX_ID", envFile, KeyVariable)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	g, err := newLiveGuard(filepath.Join(home, "leadscore-live", "captures"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := api.Config{"base_url": DefaultBaseURL, "_http_client": &http.Client{Transport: g}}
	c, err := NewClientWithKey(cfg, env[KeyVariable])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer func() { t.Logf("calls made: %d; raw replies in %s", g.calls, g.dir) }()

	// 1. Auth health through the Client, on our path and on /api/v1/.
	t.Run("auth_health", func(t *testing.T) {
		if err := c.AuthHealth(ctx); err != nil {
			t.Fatalf("AuthHealth: %v", err)
		}
		var m map[string]any
		err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/api/v1/auth/health"}, &m)
		t.Logf("GET /api/v1/auth/health: err=%v shape=%s", err, shapeOf(m))
	})

	// 2. A refused key: the status and body shape, and that KeyRefused reads it.
	t.Run("bad_key", func(t *testing.T) {
		bad, err := NewClientWithKey(cfg, "leadscore-live-check-not-a-key")
		if err != nil {
			t.Fatal(err)
		}
		err = bad.AuthHealth(ctx)
		t.Logf("auth health with a bad key: %s", errShape(err))
		if !KeyRefused(err) {
			t.Errorf("auth health with a bad key: KeyRefused is false for %v", err)
		}
		err = bad.Do(ctx, Request{Method: http.MethodGet, Path: emailAccountsPath}, nil)
		t.Logf("email accounts with a bad key: %s", errShape(err))
		if !KeyRefused(err) {
			t.Errorf("email accounts with a bad key: KeyRefused is false for %v", err)
		}
	})

	// 3. The mailbox list: APOLLO_MAILBOX_ID (an id or the mailbox's
	// address, as sinks.apollo.mailbox_id) resolves to a listed mailbox id.
	mailbox := env["APOLLO_MAILBOX_ID"]
	mailboxEmail := ""
	if strings.Contains(mailbox, "@") {
		mailboxEmail = mailbox
	}
	t.Run("email_accounts", func(t *testing.T) {
		accounts, err := c.emailAccounts(ctx)
		if err != nil {
			t.Fatalf("the mailbox list: %v", err)
		}
		id, err := c.ResolveMailbox(ctx, mailbox) // an id: no call; an address: one more list call
		listed := false
		for _, a := range accounts {
			if a.ID == id {
				listed = true
				if mailboxEmail == "" {
					mailboxEmail = a.Email
				}
			}
		}
		t.Logf("%d mailboxes; APOLLO_MAILBOX_ID is an address: %v; resolves to a listed id: %v (err %v)",
			len(accounts), strings.Contains(mailbox, "@"), listed, err)
		if err != nil || !listed {
			t.Errorf("APOLLO_MAILBOX_ID does not resolve to a listed mailbox: %v", err)
		}
		t.Logf("shape: %s", shapeOf(rawJSON(g.last())))
	})

	// 4. The sequence search: shape, paging, whether q_name filters, and
	// ResolveSequence on a real name.
	t.Run("sequences", func(t *testing.T) {
		var first map[string]any
		err := c.Do(ctx, Request{Method: http.MethodPost, Path: sequencesSearchPath, Body: map[string]any{"page": 1, "per_page": 5}}, &first)
		if err != nil {
			t.Fatalf("sequence search: %v", err)
		}
		t.Logf("unfiltered (per_page 5): pagination=%v shape=%s", first["pagination"], shapeOf(first))
		var none map[string]any
		err = c.Do(ctx, Request{Method: http.MethodPost, Path: sequencesSearchPath, Body: map[string]any{"q_name": "zz leadscore no such sequence 7f3c", "page": 1, "per_page": 5}}, &none)
		t.Logf("q_name with no match: err=%v pagination=%v", err, none["pagination"])
		seqs, _ := first["emailer_campaigns"].([]any)
		if len(seqs) == 0 {
			t.Skip("no sequences to resolve")
		}
		name, _ := seqs[0].(map[string]any)["name"].(string)
		if pg, _ := none["pagination"].(map[string]any); pg == nil || pg["total_entries"] != float64(0) {
			t.Skip("q_name did not filter, so ResolveSequence would read every page; not run")
		}
		id, err := c.ResolveSequence(ctx, name)
		t.Logf("ResolveSequence(first sequence's name): found=%v err=%v", id != "", err)
	})

	// 5. The reply search: fields, paging, and what the date filter does.
	now := time.Now().UTC()
	search := func(t *testing.T, label string, extra map[string]any) map[string]any {
		body := map[string]any{"emailer_message_stats": []string{"replied"}, "page": 1, "per_page": 5}
		for k, v := range extra {
			body[k] = v
		}
		var m map[string]any
		err := c.Do(ctx, Request{Method: http.MethodPost, Path: messagesSearchPath, Body: body}, &m)
		n := -1
		if ms, ok := m["emailer_messages"].([]any); ok {
			n = len(ms)
		}
		t.Logf("%s: err=%v records=%d pagination=%v range=%s", label, errShape(err), n, m["pagination"], completedRange(m))
		return m
	}
	// The first live run showed emailerMessageDateRange is ignored: a min of
	// tomorrow still returned old replies, while emailer_message_date_range
	// (what Poll now sends) with that min returned none.
	since := func(after string) map[string]any {
		return map[string]any{"emailer_message_date_range_mode": "completed_at",
			"emailer_message_date_range": map[string]string{"min": after}}
	}
	t.Run("reply_date_filter", func(t *testing.T) {
		search(t, "replied, snake_case min a year ago", since(now.Add(-365*24*time.Hour).Format(time.DateOnly)))
		search(t, "replied, snake_case min 30 days ago", since(now.Add(-30*24*time.Hour).Format(time.DateOnly)))
		search(t, "replied, snake_case min as a timestamp 2 days ago", since(now.Add(-48*time.Hour).Format(time.RFC3339)))
	})
	t.Run("reply_search", func(t *testing.T) {
		p1 := search(t, "replied, no date, page 1", nil)
		t.Logf("message shape: %s", shapeOf(p1))
		p2 := search(t, "replied, no date, page 2", map[string]any{"page": 2})
		t.Logf("page 2 shares ids with page 1: %v", sharesIDs(p1, p2))
	})
	// The real Poller: a 7-day window holds no more than a 400-day one, and
	// every event before the window's day is filtered out by Apollo.
	t.Run("poller", func(t *testing.T) {
		p := &Poller{c: c, now: time.Now}
		poll := func(days int) int {
			since := now.Add(-time.Duration(days) * 24 * time.Hour)
			evs, err := p.Poll(ctx, since)
			if err != nil {
				t.Fatalf("Poll(%d days): %v", days, err)
			}
			labelled, untimed, noContact, early := 0, 0, 0, 0
			for _, e := range evs {
				if e.Attrs[AttrLabel] != "" {
					labelled++
				}
				if e.Attrs[AttrNoReplyTime] != "" {
					untimed++
				}
				if e.Attrs[AttrContactID] == "" {
					noContact++
				}
				if e.At.Before(since.Truncate(24 * time.Hour)) {
					early++
				}
			}
			t.Logf("Poll(%d days): %d events, %d labelled, %d without a time, %d without a contact id, %d sent before the window",
				days, len(evs), labelled, untimed, noContact, early)
			if early > 0 {
				t.Errorf("Poll(%d days): %d events sent before the window", days, early)
			}
			return len(evs)
		}
		if week, long := poll(7), poll(400); week > long {
			t.Errorf("7 days gave %d events, 400 days %d", week, long)
		}
	})

	// 6. A contact search by an address that is ours: the mailbox's own.
	t.Run("contacts_search", func(t *testing.T) {
		if mailboxEmail == "" {
			t.Skip("no mailbox address to search by")
		}
		found, err := c.contactsByEmail(ctx, mailboxEmail)
		var m map[string]any
		_ = json.Unmarshal(g.last(), &m)
		t.Logf("contactsByEmail(our mailbox address): %d found, err=%v; pagination=%v shape=%s", len(found), errShape(err), m["pagination"], shapeOf(m))
	})

	// 7. Error shapes: a contact id that does not exist, and a bad filter value.
	t.Run("errors", func(t *testing.T) {
		err := c.Do(ctx, Request{Method: http.MethodGet, Path: contactsPath + "/000000000000000000000000"}, nil)
		t.Logf("GET an unknown contact id: %s", errShape(err))
		err = c.Do(ctx, Request{Method: http.MethodPost, Path: messagesSearchPath, Body: map[string]any{"page": "not-a-number", "per_page": -5}}, nil)
		t.Logf("reply search with a bad page value: %s", errShape(err))
	})

	t.Logf("response header names seen: %s", strings.Join(g.headerNames(), ", "))
	t.Logf("rate-limit headers on the last reply: %s", g.rateHeaders())
}

// liveMaxCalls bounds the live check; liveGap spaces its calls.
const (
	liveMaxCalls = 30
	liveGap      = time.Second
)

// liveAllowed is every call the live check may send: reads only.
func liveAllowed(method, path string) bool {
	switch method {
	case http.MethodGet:
		return path == authHealthPath || path == "/api/v1/auth/health" || path == emailAccountsPath ||
			strings.HasPrefix(path, contactsPath+"/") && !strings.Contains(strings.TrimPrefix(path, contactsPath+"/"), "/")
	case http.MethodPost:
		return path == sequencesSearchPath || path == messagesSearchPath || path == contactsSearchPath
	}
	return false
}

// liveGuard is the live check's transport: it refuses any call not in
// liveAllowed and any past liveMaxCalls, spaces calls by liveGap, and saves
// each raw exchange (header names only on the request) under dir.
type liveGuard struct {
	mu      sync.Mutex
	dir     string
	calls   int
	lastAt  time.Time
	lastRaw []byte
	lastHdr http.Header
	names   map[string]bool
}

func newLiveGuard(dir string) (*liveGuard, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &liveGuard{dir: dir, names: map[string]bool{}}, nil
}

func (g *liveGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.URL.Host != "api.apollo.io" || !liveAllowed(req.Method, req.URL.Path) {
		return nil, fmt.Errorf("live guard: %s %s is not an allowed read", req.Method, req.URL.Path)
	}
	if g.calls >= liveMaxCalls {
		return nil, fmt.Errorf("live guard: over the budget of %d calls", liveMaxCalls)
	}
	if wait := liveGap - time.Since(g.lastAt); wait > 0 {
		time.Sleep(wait)
	}
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	g.calls++
	g.lastAt = time.Now()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	g.lastRaw, g.lastHdr = body, resp.Header // in memory only, for rateHeaders
	for k := range resp.Header {
		g.names[k] = true
	}
	var reqHeaders []string
	for k := range req.Header {
		reqHeaders = append(reqHeaders, k)
	}
	sort.Strings(reqHeaders)
	// Cookies are session state, not shape: never saved.
	respHeaders := resp.Header.Clone()
	respHeaders.Del("Set-Cookie")
	capture := map[string]any{
		"method": req.Method, "path": req.URL.Path, "query": req.URL.Query(),
		"request_headers": reqHeaders, "request_body": rawJSON(reqBody),
		"status": resp.StatusCode, "response_headers": respHeaders, "response_body": rawJSON(body),
	}
	out, _ := json.MarshalIndent(capture, "", "  ")
	name := fmt.Sprintf("apollo-%s-%02d.json", g.lastAt.UTC().Format("20060102T150405"), g.calls)
	if err := os.WriteFile(filepath.Join(g.dir, name), out, 0o600); err != nil {
		return nil, err
	}
	return resp, nil
}

// last is the body of the latest reply.
func (g *liveGuard) last() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastRaw
}

func (g *liveGuard) headerNames() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for k := range g.names {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rateHeaders is the latest reply's headers whose names mention a rate or
// limit, with their values (counts, not data).
func (g *liveGuard) rateHeaders() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for k, v := range g.lastHdr {
		l := strings.ToLower(k)
		if strings.Contains(l, "rate") || strings.Contains(l, "limit") || l == "retry-after" {
			out = append(out, k+"="+strings.Join(v, ","))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// readEnvFile reads KEY=VALUE lines (optionally "export ", optionally quoted).
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}

// rawJSON is b as JSON when it parses, else as text (nil when empty).
func rawJSON(b []byte) any {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	return string(b)
}

// errShape is an error's status and the field names of its body, never the
// values (a body may echo data back), except a short machine error code.
func errShape(err error) string {
	if err == nil {
		return "ok"
	}
	se, ok := err.(*StatusError)
	if !ok {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("status %d, body %s", se.Status, shapeOf(rawJSON(se.Body())))
}

// idLike matches keys that are record ids, so a shape never shows one.
var idLike = regexp.MustCompile(`^[0-9a-f]{24}$`)

// shapeOf is v's field names and JSON types, arrays shown by the union of
// their elements' shapes; no values.
func shapeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		merged := map[string]any{}
		for _, e := range x {
			m, ok := e.(map[string]any)
			if !ok {
				return "[" + shapeOf(e) + "]"
			}
			for k, ev := range m {
				if prev, seen := merged[k]; !seen || prev == nil {
					merged[k] = ev
				}
			}
		}
		return "[" + shapeOf(merged) + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			name := k
			if idLike.MatchString(k) {
				name = "<id>"
			}
			parts = append(parts, name+":"+shapeOf(x[k]))
		}
		return "{" + strings.Join(parts, " ") + "}"
	}
	return fmt.Sprintf("%T", v)
}

// completedRange is the earliest and latest completed_at and replied_at day
// among a reply search's messages (dates only).
func completedRange(m map[string]any) string {
	msgs, _ := m["emailer_messages"].([]any)
	var out []string
	for _, f := range []string{"completed_at", "replied_at"} {
		var days []string
		for _, e := range msgs {
			if s, _ := e.(map[string]any)[f].(string); len(s) >= 10 {
				days = append(days, s[:10])
			}
		}
		sort.Strings(days)
		if len(days) > 0 {
			out = append(out, fmt.Sprintf("%s %s..%s (%d)", f, days[0], days[len(days)-1], len(days)))
		}
	}
	return strings.Join(out, "; ")
}

// sharesIDs reports whether two reply search pages have a message id in common.
func sharesIDs(a, b map[string]any) bool {
	ids := map[any]bool{}
	as, _ := a["emailer_messages"].([]any)
	for _, e := range as {
		if m, ok := e.(map[string]any); ok {
			ids[m["id"]] = true
		}
	}
	bs, _ := b["emailer_messages"].([]any)
	for _, e := range bs {
		if m, ok := e.(map[string]any); ok && ids[m["id"]] {
			return true
		}
	}
	return false
}
