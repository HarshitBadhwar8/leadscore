package check

import (
	"context"
	"os"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

func init() { Register(apolloKey{getenv: os.Getenv}) }

// apolloKey is the `apollo-key` check (contracts section 10). It signs in
// with Apollo's free auth-health call, never an enrichment call, so running
// it every run spends no credits. A missing key is the secrets check's to
// report. It also warns a team that sends only through Apollo when Apollo's
// contacts have no opt-out flag to look up (apollo.ContactOptOutFlag).
type apolloKey struct{ getenv func(string) string }

func (apolloKey) Name() string { return "apollo-key" }
func (apolloKey) InRun() bool  { return true }

func (a apolloKey) Run(ctx context.Context, env Env) []Problem {
	block := apolloBlock(env.Config)
	if block == nil {
		return nil
	}
	var out []Problem
	if key := a.getenv(apollo.KeyVariable); strings.TrimSpace(key) != "" {
		c, err := apollo.NewClientWithKey(block, key)
		if err == nil {
			err = c.AuthHealth(ctx)
		}
		if err != nil {
			out = append(out, Problem{Key: "apollo-key:auth",
				Message: "the Apollo key fails Apollo's auth-health call: " + err.Error(),
				Fix:     "check the key in " + apollo.KeyVariable + " (Secret Manager or .env)"})
		}
	}
	_, hubspot := env.Config.Sinks["hubspot"]
	if _, sends := env.Config.Sinks["apollo"]; sends && !hubspot && !apollo.ContactOptOutFlag {
		out = append(out, Problem{Key: "apollo-key:no_optout_flag", Warning: true,
			Message: "Apollo's contacts carry no opt-out flag leadscore can look up, and no HubSpot sink is set, " +
				"so a person who clicked an unsubscribe link without replying is not seen before a push",
			Fix: "add an Apollo workflow that sends unsubscribes to the receiver, or mark such people unsubscribed in Overrides"})
	}
	return out
}

// apolloBlock is the leadscore.yml block an Apollo client is built from: the
// enrich block when it uses Apollo, else sinks.apollo; nil when the install
// does not use Apollo.
func apolloBlock(c *config.Config) api.Config {
	if c == nil {
		return nil
	}
	if c.Enrich != nil && c.Enrich.Type == "apollo" {
		return c.Enrich.Block
	}
	if b, ok := c.Sinks["apollo"]; ok {
		if b == nil {
			b = api.Config{}
		}
		return b
	}
	return nil
}
