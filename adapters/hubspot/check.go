package hubspot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/check"
)

// requiredScopes are the private-app scopes the sink and lookup use (S0
// confirms the list); dealsWriteScope is needed only when a lane pushes to
// hubspot:deals. Setup also needs crm.schemas.contacts.write and
// crm.schemas.deals.write, which a run does not.
var requiredScopes = []string{
	"crm.objects.companies.read",
	"crm.objects.contacts.read",
	"crm.objects.contacts.write",
	"crm.objects.deals.read",
	"crm.schemas.contacts.read",
	"crm.schemas.deals.read",
}

const dealsWriteScope = "crm.objects.deals.write"

// hubspotCheck is the `hubspot` check (contracts section 10): the token has
// the scopes, the custom properties exist and fit, and the pipeline and stage
// resolve. It runs only when sinks.hubspot is configured and the token is
// set (the secrets check reports a missing token).
type hubspotCheck struct{}

func (hubspotCheck) Name() string { return "hubspot" }
func (hubspotCheck) InRun() bool  { return true }

const (
	fixApp   = "re-create the private app with the scopes leadscore needs, and update HUBSPOT_TOKEN"
	fixSetup = "run `leadscore setup hubspot`"
)

func (hubspotCheck) Run(ctx context.Context, env check.Env) []check.Problem {
	if env.Config == nil {
		return nil
	}
	block, ok := env.Config.Sinks["hubspot"]
	if !ok || strings.TrimSpace(os.Getenv(TokenVariable)) == "" {
		return nil
	}
	s, err := parse(block)
	if err != nil {
		return []check.Problem{{Key: "hubspot:config", Message: err.Error(), Fix: "fix sinks.hubspot in leadscore.yml"}}
	}
	var out []check.Problem
	apiProblem := func(err error) []check.Problem {
		return append(out, check.Problem{Key: "hubspot:api", Message: "HubSpot could not be read: " + err.Error(),
			Fix: "check HUBSPOT_TOKEN and HubSpot's status"})
	}

	dealsLane := ""
	if env.Rubric != nil {
		for _, l := range env.Rubric.Lanes() {
			if l.Sink == "hubspot" && l.Dest == destDeals {
				dealsLane = l.ID
				break
			}
		}
	}
	// S0 confirms this call and its answer for private-app tokens.
	var info struct {
		Scopes []string `json:"scopes"`
	}
	if err := s.c.call(ctx, http.MethodPost, "/oauth/v2/private-apps/get/access-token-info",
		map[string]string{"tokenKey": s.c.token}, &info); err != nil {
		return apiProblem(err)
	}
	have := map[string]bool{}
	for _, sc := range info.Scopes {
		have[sc] = true
	}
	var missing []string
	need := requiredScopes
	if dealsLane != "" {
		need = append(append([]string(nil), need...), dealsWriteScope)
	}
	sort.Strings(need)
	for _, sc := range need {
		if !have[sc] {
			missing = append(missing, sc)
		}
	}
	if len(missing) > 0 {
		out = append(out, check.Problem{Key: "hubspot:scopes", Message: "the HubSpot token is missing scopes: " + strings.Join(missing, ", "), Fix: fixApp})
	}

	var gaps []string
	for _, object := range []string{"contacts", "deals"} {
		props, err := listProperties(ctx, s.c, object)
		if err != nil {
			return apiProblem(err)
		}
		for _, p := range properties() {
			if p.object != object {
				continue
			}
			name := s.prop(p.name)
			e, ok := props[name]
			switch {
			case !ok:
				gaps = append(gaps, fmt.Sprintf("%s property %s is missing", strings.TrimSuffix(object, "s"), name))
			case p.mismatch(e) != "":
				gaps = append(gaps, fmt.Sprintf("%s property %s %s", strings.TrimSuffix(object, "s"), name, p.mismatch(e)))
			}
		}
	}
	if len(gaps) > 0 {
		sort.Strings(gaps)
		out = append(out, check.Problem{Key: "hubspot:properties", Message: strings.Join(gaps, "; "), Fix: fixSetup})
	}

	switch {
	case s.pipeline == "" && dealsLane != "":
		out = append(out, check.Problem{Key: "hubspot:pipeline",
			Message: "lane " + dealsLane + " pushes to hubspot:deals, and sinks.hubspot.pipeline and stage are not set",
			Fix:     "set sinks.hubspot.pipeline and stage to the names in HubSpot, then " + fixSetup})
	case s.pipeline != "":
		if _, err := readPipelines(ctx, s); err != nil {
			if !errors.Is(err, errConfig) {
				return apiProblem(err)
			}
			out = append(out, check.Problem{Key: "hubspot:pipeline", Message: err.Error(),
				Fix: "set sinks.hubspot.pipeline and stage to the names in HubSpot, then " + fixSetup})
		}
	}
	return out
}
