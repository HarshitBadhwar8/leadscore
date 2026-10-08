// Package apollo is a fake of the Apollo API calls leadscore makes, served
// from the fixtures in testdata/vendors/apollo (provisional until real captured
// calls replace them). This file covers what enrichment and the
// apollo-key check call: auth health and the organization lookup.
// outreach.go covers the sink, the contact lookup, the reply poller and the
// apollo-sequences check.
//
// The fake checks the X-Api-Key header against its key and answers the
// fixture's bad_key case when it does not match. It records every call, so a
// test can count credits spent.
package apollo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Fixture is one saved call and case (testdata/vendors/<vendor>/<call>/<case>.json).
type Fixture struct {
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Query           map[string]string `json:"query"`
	RequestHeaders  []string          `json:"request_headers"`
	RequestBody     json.RawMessage   `json:"request_body"`
	Status          int               `json:"status"`
	ResponseHeaders map[string]string `json:"response_headers"`
	ResponseBody    json.RawMessage   `json:"response_body"`
	Provisional     bool              `json:"provisional"`
	Documented      bool              `json:"documented"`
	Note            string            `json:"note"`           // what is still unconfirmed about this file
	ConfirmedFrom   string            `json:"confirmed_from"` // which record confirms which parts, when one does
	// RecordedFrom names the real call this file's shape was recorded from
	// (scrubbed: real field names, types, status and headers; made-up
	// values). A recorded file is not provisional.
	RecordedFrom string `json:"recorded_from"`
}

// Call is one request the fake answered.
type Call struct {
	Method, Path string
	Domain       string     // the enrich call's domain query
	Query        url.Values // the request's query
	Body         []byte     // the request's body
	Status       int
}

// Org is a company the fake knows, rendered in the found fixture's shape.
// Empty fields (nil Employees) are left out of the reply, as Apollo leaves
// out what it does not know.
type Org struct {
	Name         string
	Employees    *int
	FundingStage string
	FundingDate  string
	Country      string
}

// Server is the fake. Create it with New and serve it with httptest.
type Server struct {
	mu         sync.Mutex
	key        string
	fixtures   map[string]Fixture // "<call>/<case>"
	orgs       map[string]Org
	cases      map[string]string // domain -> organizations_enrich case served as saved
	limitSkip  int               // enrich calls answered normally before rateLimit applies
	rateLimit  int               // then this many enrich calls are answered 429
	retryAfter *string           // overrides the rate_limited fixture's Retry-After
	authCase   string            // the auth_health case a good key gets; "ok" by default
	calls      []Call
	out        *outreach // the sink, lookup and poller calls (outreach.go)
}

// New returns a fake that accepts key, knowing no companies.
func New(key string) *Server {
	return &Server{key: key, fixtures: loadFixtures(), orgs: map[string]Org{}, cases: map[string]string{}, authCase: "ok",
		out: newOutreach()}
}

// Dir is the folder holding the Apollo fixtures.
func Dir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("fakes/apollo: cannot find the source folder")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "vendors", "apollo")
}

// Load reads one fixture, "<call>/<case>".
func Load(name string) (Fixture, error) {
	var f Fixture
	b, err := os.ReadFile(filepath.Join(Dir(), filepath.FromSlash(name)+".json"))
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("fixture %s: %w", name, err)
	}
	return f, nil
}

func loadFixtures() map[string]Fixture {
	out := map[string]Fixture{}
	for _, name := range []string{
		"auth_health/ok", "auth_health/not_logged_in", "auth_health/bad_key",
		"organizations_enrich/found", "organizations_enrich/found_sparse",
		"organizations_enrich/not_found", "organizations_enrich/not_found_null",
		"organizations_enrich/status_404", "organizations_enrich/server_error",
		"organizations_enrich/rate_limited", "organizations_enrich/bad_key",
	} {
		out[name] = mustLoad(name)
	}
	for _, name := range outreachFixtures {
		out[name] = mustLoad(name)
	}
	return out
}

func mustLoad(name string) Fixture {
	f, err := Load(name)
	if err != nil {
		panic("fakes/apollo: " + err.Error())
	}
	return f
}

// AddOrg makes the fake know a company: the enrich call for domain answers
// the found fixture with these facts in place of its own.
func (s *Server) AddOrg(domain string, o Org) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := strings.ToLower(domain)
	s.orgs[d] = o
	delete(s.cases, d)
}

// Serve makes the enrich call for domain answer an organizations_enrich case
// exactly as saved, for example "found_sparse", "not_found_null" or
// "server_error". A domain the fake does not know answers "not_found".
func (s *Server) Serve(domain, enrichCase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.fixtures["organizations_enrich/"+enrichCase]; !ok {
		panic("fakes/apollo: no organizations_enrich case " + enrichCase)
	}
	d := strings.ToLower(domain)
	s.cases[d] = enrichCase
	delete(s.orgs, d)
}

// ServeAuth makes auth health answer a case even for the right key, for
// example "not_logged_in".
func (s *Server) ServeAuth(authCase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.fixtures["auth_health/"+authCase]; !ok {
		panic("fakes/apollo: no auth_health case " + authCase)
	}
	s.authCase = authCase
}

// RateLimitNext makes the next n enrich calls answer the rate_limited case.
func (s *Server) RateLimitNext(n int) { s.RateLimitAfter(0, n) }

// RateLimitAfter answers the next `after` enrich calls normally, then the n
// after them with the rate_limited case.
func (s *Server) RateLimitAfter(after, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limitSkip, s.rateLimit = after, n
}

// SetRetryAfter replaces the Retry-After the rate_limited case sends ("0"
// keeps a test from waiting).
func (s *Server) SetRetryAfter(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryAfter = &v
}

// Calls returns every call answered, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// EnrichCalls returns the domain of every enrich call answered, in order,
// rate-limited ones included.
func (s *Server) EnrichCalls() []string {
	var out []string
	for _, c := range s.Calls() {
		if c.Path == s.fixtures["organizations_enrich/found"].Path {
			out = append(out, c.Domain)
		}
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, f, body := s.answer(r)
	call.Status = f.Status
	s.calls = append(s.calls, call)
	for k, v := range f.ResponseHeaders {
		w.Header().Set(k, v)
	}
	if f.Status == http.StatusTooManyRequests && s.retryAfter != nil {
		w.Header().Set("Retry-After", *s.retryAfter)
	}
	w.WriteHeader(f.Status)
	_, _ = w.Write(body)
}

// answer picks the fixture for a request, and the body to send.
func (s *Server) answer(r *http.Request) (Call, Fixture, []byte) {
	call := Call{Method: r.Method, Path: r.URL.Path}
	var callName string
	switch {
	case r.Method == http.MethodGet && r.URL.Path == s.fixtures["auth_health/ok"].Path:
		callName = "auth_health"
	case r.Method == http.MethodGet && r.URL.Path == s.fixtures["organizations_enrich/found"].Path:
		callName = "organizations_enrich"
		call.Domain = r.URL.Query().Get("domain")
	default:
		if callName = outreachCall(r); callName == "" {
			return call, Fixture{Status: http.StatusNotFound}, []byte(`{"error":"not faked"}`)
		}
		call.Query = r.URL.Query()
		call.Body, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Api-Key") != s.key {
			f := s.fixtures["email_accounts/bad_key"]
			return call, f, f.ResponseBody
		}
		f, body := s.answerOutreach(callName, r, call.Body)
		return call, f, body
	}
	if r.Header.Get("X-Api-Key") != s.key {
		f := s.fixtures[callName+"/bad_key"]
		return call, f, f.ResponseBody
	}
	if callName == "auth_health" {
		f := s.fixtures["auth_health/"+s.authCase]
		return call, f, f.ResponseBody
	}
	if s.limitSkip > 0 {
		s.limitSkip--
	} else if s.rateLimit > 0 {
		s.rateLimit--
		f := s.fixtures["organizations_enrich/rate_limited"]
		return call, f, f.ResponseBody
	}
	d := strings.ToLower(call.Domain)
	if c, ok := s.cases[d]; ok {
		f := s.fixtures["organizations_enrich/"+c]
		return call, f, f.ResponseBody
	}
	o, ok := s.orgs[d]
	if !ok {
		f := s.fixtures["organizations_enrich/not_found"]
		return call, f, f.ResponseBody
	}
	f := s.fixtures["organizations_enrich/found"]
	return call, f, renderOrg(f.ResponseBody, d, o)
}

// renderOrg is the found fixture's body with the company's own facts.
func renderOrg(saved json.RawMessage, domain string, o Org) []byte {
	var body map[string]map[string]any
	if err := json.Unmarshal(saved, &body); err != nil {
		panic("fakes/apollo: the found fixture is not an organization reply: " + err.Error())
	}
	org := body["organization"]
	org["primary_domain"] = domain
	org["website_url"] = "http://www." + domain
	set := func(k string, v any, has bool) {
		if has {
			org[k] = v
		} else {
			delete(org, k)
		}
	}
	set("name", o.Name, o.Name != "")
	set("estimated_num_employees", o.Employees, o.Employees != nil)
	set("latest_funding_stage", o.FundingStage, o.FundingStage != "")
	set("latest_funding_round_date", o.FundingDate, o.FundingDate != "")
	set("country", o.Country, o.Country != "")
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return b
}
