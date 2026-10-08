// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package vendorhttp holds the HTTP rules the vendor clients share, so Apollo's
// and HubSpot's cannot drift apart: the test keys, no redirects, a per-call
// timeout, a size cap on the reply, transport errors without the URL, and the
// error classes. Auth headers, retries, pacing and parsing stay in each
// adapter.
package vendorhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// ErrBaseURLNeedsClient refuses a block with `base_url` but no test client.
var ErrBaseURLNeedsClient = errors.New("`base_url` is for tests only and needs a test HTTP client; remove it from leadscore.yml")

// Overrides reads a block's two test keys (base_url and _http_client). base is
// base_url without a trailing "/", or "" when unset; hc is _http_client, or nil
// when unset (a typed nil counts as unset). base_url without a client is
// refused: only tests set it, and one written into leadscore.yml would send the
// key to whatever address it names. A base_url that is not non-empty text, or
// an _http_client that is not an *http.Client, is refused too.
func Overrides(cfg api.Config) (base string, hc *http.Client, err error) {
	if v, ok := cfg["_http_client"]; ok && v != nil {
		if hc, ok = v.(*http.Client); !ok {
			return "", nil, errors.New("`_http_client` must be an *http.Client")
		}
	}
	if v, ok := cfg["base_url"]; ok && v != nil {
		s, _ := v.(string)
		if base = strings.TrimRight(strings.TrimSpace(s), "/"); base == "" {
			return "", nil, errors.New("`base_url` must be a URL")
		}
		if hc == nil {
			return "", nil, ErrBaseURLNeedsClient
		}
	}
	return base, hc, nil
}

// NewClient returns a copy of hc (a new default client when hc is nil) that
// never follows a redirect: a 3xx is the reply, so a key header never leaves
// for another host. The caller's client is left alone. Do bounds each call.
func NewClient(hc *http.Client) *http.Client {
	var c http.Client
	if hc != nil {
		c = *hc
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// Reply is a whole reply, read into memory.
type Reply struct {
	Status int
	Header http.Header
	Body   []byte // never log it: it may echo a person's email
}

// ErrTooLarge is a reply body over Do's cap.
var ErrTooLarge = errors.New("the reply is over the size cap")

// Do sends req bounded by timeout on its own context, whatever client is
// given, and reads the whole reply (at most maxBytes) while that context is
// live. An error means no whole reply (a timeout, a network failure, a body
// over the cap), transient by Class's rule; but when the status line came,
// the Reply still carries the status, header and the body read so far (at
// most maxBytes), so a caller still reads a 429, 401 or 403 as a rate limit.
// A transport error names only its operation and cause, never the URL, whose
// query may hold an email.
func Do(hc *http.Client, req *http.Request, timeout time.Duration, maxBytes int64) (Reply, error) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	resp, err := hc.Do(req.WithContext(ctx)) //nolint:gosec // the URL is the configured base plus the caller's path
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("%s: %w", ue.Op, ue.Err)
		}
		return Reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	reply := Reply{Status: resp.StatusCode, Header: resp.Header, Body: raw[:min(int64(len(raw)), maxBytes)]}
	switch {
	case err != nil:
		return reply, fmt.Errorf("reading the reply: %w", err)
	case int64(len(raw)) > maxBytes:
		return reply, fmt.Errorf("%w (%d bytes)", ErrTooLarge, maxBytes)
	}
	return reply, nil
}

// KeyRefused reports a status saying the key or token itself is refused.
func KeyRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// Class is the public API's error class for a reply's status, the same for
// every vendor: a 429 is api.ErrRateLimited, and so are 401 and 403 (no lead is
// at fault, so the sink stops for the run with no attempt counted); a 5xx is
// api.ErrTransient, as is any error from Do. Nil means the adapter decides (a
// refusal it recognises, else one attempt).
func Class(status int) error {
	switch {
	case status == http.StatusTooManyRequests, KeyRefused(status):
		return api.ErrRateLimited
	case status >= 500:
		return api.ErrTransient
	}
	return nil
}
