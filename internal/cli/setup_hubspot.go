package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// testHubSpotClient is the HTTP client tests give the HubSpot adapter when
// leadscore.yml points sinks.hubspot.base_url at a fake; nil outside tests,
// so a base_url in a real file is refused.
var testHubSpotClient *http.Client

// runSetupHubSpot is `leadscore setup hubspot` (RFC 6.11): it creates the
// custom properties and their group, and resolves sinks.hubspot.pipeline and
// stage by name. It reads HUBSPOT_TOKEN and is safe to run again.
func runSetupHubSpot(inv *invocation) int {
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	src, ok := c.Sinks["hubspot"]
	if !ok {
		return inv.fail(errors.New("leadscore.yml has no sinks.hubspot block; add one, for example " +
			"sinks: { hubspot: { pipeline: Sales Pipeline, stage: Appointment scheduled } }"))
	}
	block := api.Config{}
	for k, v := range src {
		block[k] = v
	}
	if base, _ := block["base_url"].(string); base != "" && testHubSpotClient != nil {
		block["_http_client"] = testHubSpotClient
	}
	if err := hubspot.Setup(context.Background(), block, inv.stdout); err != nil {
		return inv.fail(err)
	}
	fmt.Fprintln(inv.stdout, "HubSpot is set up for leadscore")
	return exitOK
}
