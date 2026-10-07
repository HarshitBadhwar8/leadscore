package cli

import (
	"fmt"
	"strings"
	"testing"

	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
)

func fakeHubSpot(t *testing.T) (*fakehub.Server, string) {
	t.Helper()
	t.Setenv("HUBSPOT_TOKEN", fakehub.Token)
	f := fakehub.NewBare()
	srv := f.Serve(t)
	old := testHubSpotClient
	testHubSpotClient = srv.Client()
	t.Cleanup(func() { testHubSpotClient = old })
	return f, srv.URL
}

// setup hubspot creates the group and every custom property (lead id
// unique, score a number), resolves the pipeline and stage, and is safe to
// run again.
func TestSetupHubSpot(t *testing.T) {
	f, url := fakeHubSpot(t)
	path := writeConfig(t, fmt.Sprintf("version: 1\nstore: { type: sqlite, path: x.db }\nsinks:\n  hubspot: { base_url: %q, pipeline: %q, stage: %q }\n",
		url, fakehub.PipelineLabel, fakehub.StageOpenName))
	code, stdout, stderr := run("setup", "hubspot", "--config", path)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	contacts, deals := f.Properties("contacts"), f.Properties("deals")
	for _, n := range []string{"leadscore_lead_id", "leadscore_lane", "leadscore_tier", "leadscore_priority", "leadscore_reasons", "leadscore_score"} {
		if p, ok := contacts[n]; !ok || p.GroupName != "leadscore" {
			t.Errorf("contact property %s: %+v", n, p)
		}
	}
	if !contacts["leadscore_lead_id"].HasUniqueValue || contacts["leadscore_score"].Type != "number" {
		t.Errorf("lead id %+v, score %+v", contacts["leadscore_lead_id"], contacts["leadscore_score"])
	}
	for _, n := range []string{"leadscore_company_domain", "leadscore_lane"} {
		if p, ok := deals[n]; !ok || p.HasUniqueValue {
			t.Errorf("deal property %s: %+v (the domain is not unique)", n, p)
		}
	}
	if !strings.Contains(stdout, "id "+fakehub.PipelineID) || !strings.Contains(stdout, "id "+fakehub.StageOpen) {
		t.Errorf("stdout %q", stdout)
	}
	code, stdout, stderr = run("setup", "hubspot", "--config", path)
	if code != exitOK || !strings.Contains(stdout, "kept contact property leadscore_lead_id") {
		t.Errorf("second run: exit %d %q %q", code, stdout, stderr)
	}
}

// A stage the pipeline does not have, a property of the wrong type, and a
// missing sinks.hubspot block each fail with the fix.
func TestSetupHubSpotRefusals(t *testing.T) {
	f, url := fakeHubSpot(t)
	path := writeConfig(t, fmt.Sprintf("version: 1\nstore: { type: sqlite, path: x.db }\nsinks:\n  hubspot: { base_url: %q, pipeline: %q, stage: Nope }\n",
		url, fakehub.PipelineLabel))
	if code, _, stderr := run("setup", "hubspot", "--config", path); code != exitFail || !strings.Contains(stderr, `no stage named "Nope"`) {
		t.Errorf("unknown stage: %d %q", code, stderr)
	}
	f.AddProperty("contacts", fakehub.Property{Name: "leadscore_score", Type: "string", FieldType: "text"})
	path = writeConfig(t, fmt.Sprintf("version: 1\nstore: { type: sqlite, path: x.db }\nsinks:\n  hubspot: { base_url: %q }\n", url))
	if code, _, stderr := run("setup", "hubspot", "--config", path); code != exitFail ||
		!strings.Contains(stderr, "leadscore_score is of type string, not number") || !strings.Contains(stderr, "property_prefix") {
		t.Errorf("wrong type: %d %q", code, stderr)
	}
	path = writeConfig(t, "version: 1\nstore: { type: sqlite, path: x.db }\n")
	if code, _, stderr := run("setup", "hubspot", "--config", path); code != exitFail || !strings.Contains(stderr, "no sinks.hubspot") {
		t.Errorf("no block: %d %q", code, stderr)
	}
}
