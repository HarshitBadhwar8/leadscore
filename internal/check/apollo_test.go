// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func TestApolloKey(t *testing.T) {
	fake := fakeapollo.New("good-key")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	block := func() api.Config { return api.Config{"base_url": srv.URL, "_http_client": srv.Client()} }
	enrichOnly := func() *config.Config {
		return &config.Config{Enrich: &config.Enrich{Type: "apollo", Block: block()}, Sinks: map[string]api.Config{}}
	}
	apolloSink := func(hubspot bool) *config.Config {
		c := &config.Config{Sinks: map[string]api.Config{"apollo": block()}}
		if hubspot {
			c.Sinks["hubspot"] = api.Config{}
		}
		return c
	}
	receiving := func(c *config.Config) *config.Config {
		c.Replies, c.Receiver.PublicURL = "receiver", "https://receiver.example"
		return c
	}
	keys := func(ps []Problem) map[string]bool {
		out := map[string]bool{}
		for _, p := range ps {
			out[p.Key] = p.Warning
		}
		return out
	}

	tests := []struct {
		name string
		cfg  *config.Config
		key  string
		want map[string]bool // key -> warning
	}{
		{"no Apollo in the install", &config.Config{Sinks: map[string]api.Config{}}, "good-key", map[string]bool{}},
		{"a good key", enrichOnly(), "good-key", map[string]bool{}},
		{"a bad key", enrichOnly(), "wrong", map[string]bool{"apollo-key:auth": false}},
		{"no key: the secrets check reports it", enrichOnly(), "", map[string]bool{}},
		// Whether Apollo has an opt-out flag is unconfirmed; until then the
		// safe default warns teams that send only through Apollo.
		{"Apollo-only team", apolloSink(false), "good-key", map[string]bool{"apollo-key:no_optout_flag": true}},
		{"Apollo with HubSpot", apolloSink(true), "good-key", map[string]bool{}},
		// Configuration alone is no evidence the unsubscribe workflow is wired.
		{"Apollo-only team with a receiver configured", receiving(apolloSink(false)), "good-key", map[string]bool{"apollo-key:no_optout_flag": true}},
		{"Apollo-only team whose receiver got an unsubscribe", apolloSink(false), "good-key", map[string]bool{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fake.Calls())
			c := apolloKey{getenv: func(k string) string {
				if k == "APOLLO_API_KEY" {
					return tc.key
				}
				return ""
			}}
			env := Env{Config: tc.cfg}
			if strings.HasSuffix(tc.name, "got an unsubscribe") {
				env.Model = model.New()
				env.Model.SetState("last_received:unsubscribed", "2026-10-01T09:00:00.000Z")
			}
			got := keys(c.Run(context.Background(), env))
			if len(got) != len(tc.want) {
				t.Fatalf("problems %v, want %v", got, tc.want)
			}
			for k, w := range tc.want {
				if warn, ok := got[k]; !ok || warn != w {
					t.Errorf("problems %v, want %v", got, tc.want)
				}
			}
			// Never an enrichment call: only auth health, which spends no credit.
			for _, call := range fake.Calls()[before:] {
				if call.Path != "/v1/auth/health" {
					t.Errorf("the check called %s %s", call.Method, call.Path)
				}
			}
		})
	}
	if n := len(fake.EnrichCalls()); n != 0 {
		t.Errorf("the check made %d enrichment calls", n)
	}
}

// Only a refused key is apollo-key:auth; a call that got no answer (429, 5xx,
// network) is a warning under its own key, and is_logged_in false is a
// refusal.
func TestApolloKeyTellsRefusalFromOutage(t *testing.T) {
	fake := fakeapollo.New("good-key")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfg := &config.Config{Enrich: &config.Enrich{Type: "apollo",
		Block: api.Config{"base_url": srv.URL, "_http_client": srv.Client()}}, Sinks: map[string]api.Config{}}
	c := apolloKey{getenv: func(string) string { return "good-key" }}

	fake.ServeAuth("not_logged_in")
	if ps := c.Run(context.Background(), Env{Config: cfg}); len(ps) != 1 || ps[0].Key != "apollo-key:auth" || ps[0].Warning {
		t.Errorf("is_logged_in false: %+v", ps)
	}

	srv.Close() // no answer at all
	ps := c.Run(context.Background(), Env{Config: cfg})
	if len(ps) != 1 || ps[0].Key != "apollo-key:unreachable" || !ps[0].Warning {
		t.Errorf("unreachable: %+v", ps)
	}
	five := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer five.Close()
	cfg.Enrich.Block = api.Config{"base_url": five.URL, "_http_client": five.Client()}
	if ps := c.Run(context.Background(), Env{Config: cfg}); len(ps) != 1 || ps[0].Key != "apollo-key:unreachable" {
		t.Errorf("a 502: %+v", ps)
	}
}

// A block the client refuses (base_url without a test client) is a config
// error under its own key, not an outage.
func TestApolloKeyConfigError(t *testing.T) {
	cfg := &config.Config{Enrich: &config.Enrich{Type: "apollo", Block: api.Config{"base_url": "https://collector.example"}},
		Sinks: map[string]api.Config{}}
	ps := apolloKey{getenv: func(string) string { return "k" }}.Run(context.Background(), Env{Config: cfg})
	if len(ps) != 1 || ps[0].Key != "apollo-key:config" || ps[0].Warning {
		t.Errorf("problems %+v", ps)
	}
}
