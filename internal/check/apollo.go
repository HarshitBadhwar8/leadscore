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
// report; a block the client refuses is apollo-key:config, a key Apollo
// refuses (401, 403, or is_logged_in false) is apollo-key:auth, and a call
// that got no answer is the warning apollo-key:unreachable. It also warns a
// team that sends only through Apollo, until an unsubscribe webhook has been
// received, when Apollo's contacts have no opt-out flag to look up
// (apollo.ContactOptOutFlag).
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
		if err != nil {
			out = append(out, Problem{Key: "apollo-key:config",
				Message: "the Apollo block in leadscore.yml cannot be used: " + err.Error(),
				Fix:     "fix the enrich or sinks.apollo block"})
		} else {
			err = c.AuthHealth(ctx)
		}
		switch {
		case c == nil:
		case apollo.KeyRefused(err):
			out = append(out, Problem{Key: "apollo-key:auth",
				Message: "Apollo refuses the key: " + err.Error(),
				Fix:     "check the key in " + apollo.KeyVariable + " (Secret Manager or .env)"})
		case err != nil:
			// A rate limit, a 5xx or a network failure says nothing about
			// the key: a warning under its own key.
			out = append(out, Problem{Key: "apollo-key:unreachable", Warning: true,
				Message: "Apollo's auth-health call did not answer, so the key was not checked: " + err.Error(),
				Fix:     "nothing if it clears on the next run; otherwise check Apollo's status and the network"})
		}
	}
	_, hubspot := env.Config.Sinks["hubspot"]
	if _, sends := env.Config.Sinks["apollo"]; sends && !hubspot && !apollo.ContactOptOutFlag && !unsubscribeReceived(env) {
		out = append(out, Problem{Key: "apollo-key:no_optout_flag", Warning: true,
			Message: "Apollo's contacts carry no opt-out flag leadscore can look up, and no HubSpot sink is set, " +
				"so until an unsubscribe webhook has been received, a person who clicked an unsubscribe link " +
				"without replying may not be seen before a push",
			Fix: "add an Apollo workflow that sends unsubscribes to the receiver, or mark such people unsubscribed in Overrides"})
	}
	return out
}

// unsubscribeReceived reports real evidence that Apollo's unsubscribe
// workflow reaches the receiver: an `unsubscribed` webhook has arrived
// (State.last_received:unsubscribed). Configuration alone proves nothing, and
// doctor without a loaded store has no evidence.
func unsubscribeReceived(env Env) bool {
	return env.Model != nil && env.Model.StateValue("last_received:unsubscribed") != ""
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
