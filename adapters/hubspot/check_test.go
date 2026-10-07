package hubspot_test

import (
	"context"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func hubspotCheck(t *testing.T) check.Check {
	t.Helper()
	for _, c := range check.All() {
		if c.Name() == "hubspot" {
			if !c.InRun() {
				t.Error("the hubspot check must run in every run")
			}
			return c
		}
	}
	t.Fatal("the hubspot check is not registered")
	return nil
}

const dealsRubric = `version: 1
lanes:
  - { id: warm, kind: non-cold, when: { field: status, eq: replied_positive }, push: "hubspot:deals" }
`

func keysOf(ps []check.Problem) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Key)
	}
	return strings.Join(out, ",")
}

// The hubspot check (contracts section 10): the token's scopes, the custom
// properties, and the pipeline and stage.
func TestHubSpotCheck(t *testing.T) {
	c := hubspotCheck(t)
	rb, err := rules.Compile([]byte(dealsRubric))
	if err != nil {
		t.Fatal(err)
	}
	run := func(f *fakehub.Server, cfg api.Config) []check.Problem {
		return c.Run(context.Background(), check.Env{Config: &config.Config{Sinks: map[string]api.Config{"hubspot": cfg}}, Rubric: rb})
	}

	f, cfg := portal(t)
	if got := run(f, cfg); len(got) != 0 {
		t.Errorf("a set-up portal: %+v", got)
	}

	f.SetScopes("crm.objects.contacts.read")
	if got := run(f, cfg); keysOf(got) != "hubspot:scopes" || !strings.Contains(got[0].Message, "crm.objects.deals.write") {
		t.Errorf("missing scopes: %+v", got)
	}

	bare := fakehub.NewBare()
	srv := bare.Serve(t)
	bcfg := api.Config{"base_url": srv.URL, "_http_client": srv.Client(), "pipeline": fakehub.PipelineLabel, "stage": "Nope"}
	got := run(bare, bcfg)
	if keysOf(got) != "hubspot:properties,hubspot:pipeline" || !strings.Contains(got[0].Message, "leadscore_lead_id is missing") ||
		!strings.Contains(got[0].Fix, "setup hubspot") {
		t.Errorf("a bare portal with a bad stage: %+v", got)
	}

	delete(cfg, "pipeline")
	delete(cfg, "stage")
	f.SetScopes(fakehub.AllScopes...)
	if got := run(f, cfg); keysOf(got) != "hubspot:pipeline" || !strings.Contains(got[0].Message, "lane warm") {
		t.Errorf("a deals lane with no pipeline: %+v", got)
	}

	t.Setenv(hubspot.TokenVariable, "")
	if got := run(f, cfg); len(got) != 0 {
		t.Errorf("with no token (the secrets check's problem): %+v", got)
	}
	if got := c.Run(context.Background(), check.Env{Config: &config.Config{}}); len(got) != 0 {
		t.Errorf("with no sinks.hubspot: %+v", got)
	}
}
