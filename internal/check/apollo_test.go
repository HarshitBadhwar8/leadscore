package check

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
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
		// S0 confirms whether Apollo has an opt-out flag; until then the
		// safe default warns teams that send only through Apollo.
		{"Apollo-only team", apolloSink(false), "good-key", map[string]bool{"apollo-key:no_optout_flag": true}},
		{"Apollo with HubSpot", apolloSink(true), "good-key", map[string]bool{}},
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
			got := keys(c.Run(context.Background(), Env{Config: tc.cfg}))
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
