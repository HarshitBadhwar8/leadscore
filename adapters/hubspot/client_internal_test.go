// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hubspot

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

// A redirect is not followed, so the token never reaches another host; the
// test client the caller passed is not changed.
func TestCallDoesNotFollowRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect, with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	hc := srv.Client()
	c := newClient(srv.URL, "secret", hc)
	if err := c.call(context.Background(), http.MethodGet, "/crm/v3/pipelines/deals", nil, &struct{}{}); err == nil {
		t.Error("a redirect answer was read as success")
	}
	if hc.CheckRedirect != nil {
		t.Error("the caller's client was changed")
	}
}

// An answer over the size cap is a failed read (ErrTransient).
func TestCallCapsTheAnswer(t *testing.T) {
	old := maxBody
	maxBody = 64
	defer func() { maxBody = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":"` + strings.Repeat("x", 100) + `"}`))
	}))
	defer srv.Close()
	err := newClient(srv.URL, "t", srv.Client()).call(context.Background(), http.MethodGet, "/x", nil, &struct{}{})
	if !errors.Is(err, api.ErrTransient) {
		t.Errorf("err %v", err)
	}
}

// Each call is bounded by callTimeout on its own context, whatever client
// the caller passed (a test client has no timeout).
func TestCallTimesOut(t *testing.T) {
	old := callTimeout
	callTimeout = 50 * time.Millisecond
	defer func() { callTimeout = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	err := newClient(srv.URL, "t", srv.Client()).call(context.Background(), http.MethodGet, "/x", nil, nil)
	if !errors.Is(err, api.ErrTransient) || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
}

// The pacer spaces calls and stops waiting when the context ends.
func TestPacer(t *testing.T) {
	p := &pacer{every: 40 * time.Millisecond}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 75*time.Millisecond {
		t.Errorf("three calls took %s, want at least two gaps of 40ms", d)
	}
	slow := &pacer{every: time.Hour}
	_ = slow.wait(context.Background()) // the first call goes at once
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := slow.wait(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
}

// A 401 or 429 whose body is cut short is still read by its status, so the
// sink stops for the run instead of calling again.
func TestCallCutShortKeepsItsStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"status":`)) // then the connection closes early
		}))
		err := newClient(srv.URL, "t", srv.Client()).call(context.Background(), http.MethodGet, "/x", nil, nil)
		if !errors.Is(err, api.ErrRateLimited) || statusOf(err) != status {
			t.Errorf("%d: %v", status, err)
		}
		srv.Close()
	}
}
