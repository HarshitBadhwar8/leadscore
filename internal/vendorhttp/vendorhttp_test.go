// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package vendorhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

func TestOverrides(t *testing.T) {
	hc := &http.Client{}
	var typedNil *http.Client
	for _, c := range []struct {
		name    string
		cfg     api.Config
		base    string
		client  bool
		wantErr string
	}{
		{"nil block", nil, "", false, ""},
		{"neither", api.Config{}, "", false, ""},
		{"client only", api.Config{"_http_client": hc}, "", true, ""},
		{"typed-nil client only is unset", api.Config{"_http_client": typedNil}, "", false, ""},
		{"both, trailing slash trimmed", api.Config{"base_url": " http://x.test/ ", "_http_client": hc}, "http://x.test", true, ""},
		{"null base_url is unset", api.Config{"base_url": nil}, "", false, ""},
		{"base_url without a client", api.Config{"base_url": "https://collector.example"}, "", false, "tests only"},
		{"base_url with a typed-nil client", api.Config{"base_url": "https://x.example", "_http_client": typedNil}, "", false, "tests only"},
		{"base_url not text", api.Config{"base_url": 5, "_http_client": hc}, "", false, "must be a URL"},
		{"base_url empty", api.Config{"base_url": " ", "_http_client": hc}, "", false, "must be a URL"},
		{"client of the wrong type", api.Config{"_http_client": "x"}, "", false, "*http.Client"},
	} {
		base, got, err := Overrides(c.cfg)
		switch {
		case c.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.wantErr)
			}
		case err != nil || base != c.base || (got != nil) != c.client:
			t.Errorf("%s: %q %v %v", c.name, base, got, err)
		}
	}
	if _, _, err := Overrides(api.Config{"base_url": "https://x.example"}); !errors.Is(err, ErrBaseURLNeedsClient) {
		t.Errorf("err %v", err)
	}
}

// The client is a copy that refuses redirects; the caller's is unchanged.
func TestNewClientRefusesRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect, with key %q", r.Header.Get("X-Key"))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	given := srv.Client()
	hc := NewClient(given)
	if given.CheckRedirect != nil || hc == given {
		t.Error("the caller's client was changed")
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	req.Header.Set("X-Key", "secret")
	reply, err := Do(hc, req, time.Minute, 1<<10)
	if err != nil || reply.Status != http.StatusFound {
		t.Errorf("reply %d, err %v", reply.Status, err)
	}
	if d := NewClient(nil); d.CheckRedirect == nil {
		t.Error("the default client follows redirects")
	}
}

func TestDoReadsTheReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"INVALID_EMAIL","message":"ada@example.com is bad"}`))
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	reply, err := Do(NewClient(srv.Client()), req, time.Minute, 1<<10)
	if err != nil || reply.Status != http.StatusBadRequest || reply.Header.Get("Retry-After") != "3" ||
		!strings.Contains(string(reply.Body), "INVALID_EMAIL") {
		t.Fatalf("reply %+v, err %v", reply, err)
	}
}

// A reply whose body is cut short, or over the cap, still carries its
// status with the error, so a 429, 401 or 403 is still read as one.
func TestDoKeepsTheStatusWhenTheBodyFails(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":`)) // then the connection closes early
		}))
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		reply, err := Do(srv.Client(), req, time.Minute, 1<<10)
		if err == nil || reply.Status != status || Class(reply.Status) != api.ErrRateLimited {
			t.Errorf("cut short %d: reply %d, err %v", status, reply.Status, err)
		}
		req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		reply, err = Do(srv.Client(), req, time.Minute, 4)
		if !errors.Is(err, ErrTooLarge) || reply.Status != status || len(reply.Body) != 4 {
			t.Errorf("over the cap %d: reply %d %q, err %v", status, reply.Status, reply.Body, err)
		}
		srv.Close()
	}
}

func TestDoCapsTheReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 65)))
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if _, err := Do(srv.Client(), req, time.Minute, 64); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err %v", err)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if reply, err := Do(srv.Client(), req, time.Minute, 65); err != nil || len(reply.Body) != 65 {
		t.Errorf("a reply at the cap: %d bytes, err %v", len(reply.Body), err)
	}
}

// The timeout bounds the call on its own context, whatever the client.
func TestDoTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := Do(srv.Client(), req, 50*time.Millisecond, 1<<10)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
}

// A transport error names its operation and cause, never the URL.
func TestDoTransportErrorHidesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens now
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/search?email=ada%40example.com", nil)
	_, err := Do(&http.Client{}, req, time.Minute, 1<<10)
	if err == nil {
		t.Fatal("a closed server answered")
	}
	for _, leak := range []string{"ada", "email=", "/search"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q holds %q", err, leak)
		}
	}
	if !strings.HasPrefix(err.Error(), "Get: ") {
		t.Errorf("error %q does not name the operation", err)
	}
}

func TestClass(t *testing.T) {
	for status, want := range map[int]error{
		200: nil, 302: nil, 400: nil, 404: nil, 409: nil,
		401: api.ErrRateLimited, 403: api.ErrRateLimited, 429: api.ErrRateLimited,
		500: api.ErrTransient, 502: api.ErrTransient, 503: api.ErrTransient,
	} {
		if got := Class(status); got != want {
			t.Errorf("Class(%d) = %v, want %v", status, got, want)
		}
		if KeyRefused(status) != (status == 401 || status == 403) {
			t.Errorf("KeyRefused(%d) = %v", status, KeyRefused(status))
		}
	}
}
