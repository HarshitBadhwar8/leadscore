package check

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

type named string

func (n named) Name() string                     { return string(n) }
func (named) InRun() bool                        { return false }
func (named) Run(context.Context, Env) []Problem { return nil }

func TestRegisterRejectsDuplicatesAndBlanks(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    Check
		want string
	}{
		{"duplicate", named("secrets"), "registered twice"},
		{"empty name", named(""), "empty name"},
		{"nil", nil, "nil"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if s, _ := r.(string); !strings.Contains(s, tt.want) {
					t.Fatalf("panic = %v, want %q", r, tt.want)
				}
			}()
			Register(tt.c)
		})
	}
}

func TestSecretsIsRegisteredInRun(t *testing.T) {
	var found Check
	for _, c := range InRun() {
		if c.Name() == "secrets" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("the secrets check must be registered and run inside every run")
	}
}

func load(t *testing.T, yml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(yml), "/team", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestSecrets(t *testing.T) {
	full := load(t, `
version: 1
store: { type: sheets, spreadsheet: s }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]
enrich: { type: apollo }
sinks: { apollo: { mailbox_id: m }, hubspot: { pipeline: Sales, stage: New } }
`)
	tests := []struct {
		name string
		cfg  *config.Config
		vars map[string]string
		want []Problem
	}{
		{"nothing configured needs a key", load(t, "version: 1\nstore: { type: sqlite }\n"), nil, nil},
		{"every key present", full, map[string]string{"APOLLO_API_KEY": "k", "HUBSPOT_TOKEN": "t"}, nil},
		{"both missing", full, nil, []Problem{
			{Key: "secret_missing:APOLLO_API_KEY", Message: "APOLLO_API_KEY is not set; enrich, sinks.apollo need it",
				Fix: "add APOLLO_API_KEY to Secret Manager or .env"},
			{Key: "secret_missing:HUBSPOT_TOKEN", Message: "HUBSPOT_TOKEN is not set; sinks.hubspot needs it",
				Fix: "add HUBSPOT_TOKEN to Secret Manager or .env"},
		}},
		{"blank counts as missing", full, map[string]string{"APOLLO_API_KEY": "  ", "HUBSPOT_TOKEN": "t"}, []Problem{
			{Key: "secret_missing:APOLLO_API_KEY", Message: "APOLLO_API_KEY is not set; enrich, sinks.apollo need it",
				Fix: "add APOLLO_API_KEY to Secret Manager or .env"},
		}},
		{"enrich only", load(t, "version: 1\nstore: { type: sqlite }\nenrich: { type: apollo }\n"), nil, []Problem{
			{Key: "secret_missing:APOLLO_API_KEY", Message: "APOLLO_API_KEY is not set; enrich needs it",
				Fix: "add APOLLO_API_KEY to Secret Manager or .env"},
		}},
		{"plug-in types need no built-in key", load(t, "version: 1\nstore: { type: sqlite }\nsinks: { mysink: {} }\n"), nil, nil},
		{"no config", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := secrets{getenv: env(tt.vars)}.Run(context.Background(), Env{Config: tt.cfg})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("problems = %#v\nwant %#v", got, tt.want)
			}
			for _, p := range got {
				if p.Warning {
					t.Errorf("%s: a missing key fails the check; it is not a warning", p.Key)
				}
			}
		})
	}
}
