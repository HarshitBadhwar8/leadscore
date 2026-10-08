// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

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
	"github.com/HarshitBadhwar8/leadscore/internal/model"
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

// The hubspot doctor check: the token's scopes, the custom
// properties, and the pipeline and stage.
func TestHubSpotCheck(t *testing.T) {
	c := hubspotCheck(t)
	rb, err := rules.Compile([]byte(dealsRubric))
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ *fakehub.Server, cfg api.Config) []check.Problem {
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

	f.SetScopes("crm.objects.companies.read", "crm.objects.contacts.read", "crm.objects.contacts.write",
		"crm.objects.deals.read", "crm.schemas.contacts.read", "crm.schemas.deals.read")
	contactsOnly := c.Run(context.Background(), check.Env{Config: &config.Config{Sinks: map[string]api.Config{"hubspot": cfg}}})
	if len(contactsOnly) != 0 {
		t.Errorf("deals write is required with no deals lane: %+v", contactsOnly)
	}
	if got := run(f, cfg); keysOf(got) != "hubspot:scopes" || !strings.Contains(got[0].Message, "crm.objects.deals.write") {
		t.Errorf("a deals lane needs deals write: %+v", got)
	}

	f.AddProperty("deals", fakehub.Property{Name: "leadscore_company_domain", Type: "string", FieldType: "text", HasUniqueValue: true})
	f.SetScopes(fakehub.AllScopes...)
	if got := run(f, cfg); keysOf(got) != "hubspot:properties" || !strings.Contains(got[0].Message, "leadscore_company_domain requires unique values") {
		t.Errorf("a unique domain property: %+v", got)
	}
	f.AddProperty("deals", fakehub.Property{Name: "leadscore_company_domain", Type: "string", FieldType: "text"})

	t.Setenv(hubspot.TokenVariable, "wrong")
	if got := run(f, cfg); keysOf(got) != "hubspot:api" || !strings.Contains(got[0].Message, "401") {
		t.Errorf("a refused token: %+v", got)
	}
	t.Setenv(hubspot.TokenVariable, fakehub.Token)

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

// A deal step waiting on a deal at an unknown stage raises
// hubspot:unknown_stage, even with no token to call HubSpot.
func TestHubSpotCheckUnknownStage(t *testing.T) {
	c := hubspotCheck(t)
	m := model.New()
	m.Put(model.TablePushes, model.Push{LeadID: "a", LaneID: "warm", Step: "deal", Dest: "deals", State: "pending",
		LastError: "hubspot: a deal at the company is at a stage no pipeline lists, so the deal step waits: transient"})
	t.Setenv(hubspot.TokenVariable, "")
	got := c.Run(context.Background(), check.Env{Config: &config.Config{Sinks: map[string]api.Config{"hubspot": {}}}, Model: m})
	if keysOf(got) != "hubspot:unknown_stage" || !strings.Contains(got[0].Message, "1 deal step") {
		t.Errorf("%+v", got)
	}
}
