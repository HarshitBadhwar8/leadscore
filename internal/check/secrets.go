package check

import (
	"context"
	"os"
	"sort"
	"strings"
)

func init() { Register(secrets{getenv: os.Getenv}) }

// adapterKeyVariables names the key variable each built-in vendor adapter
// reads (RFC 6.13). The receiver's secret is the receiver-secret check's, not
// this one's. Stores sign in through Google's standard credentials and need none.
var adapterKeyVariables = map[string]string{
	"apollo":  "APOLLO_API_KEY",
	"hubspot": "HUBSPOT_TOKEN",
}

// secrets fails when a configured adapter's key variable is missing (section 10).
type secrets struct{ getenv func(string) string }

func (secrets) Name() string { return "secrets" }
func (secrets) InRun() bool  { return true }

func (s secrets) Run(_ context.Context, env Env) []Problem {
	if env.Config == nil {
		return nil
	}
	// Variable -> the config places that need it, so one problem names every user.
	needs := map[string][]string{}
	need := func(typ, where string) {
		if v, ok := adapterKeyVariables[typ]; ok {
			needs[v] = append(needs[v], where)
		}
	}
	c := env.Config
	if c.Enrich != nil {
		need(c.Enrich.Type, "enrich")
	}
	for _, src := range c.Sources {
		need(src.Type, "sources."+src.ID)
	}
	for typ := range c.Sinks {
		need(typ, "sinks."+typ)
	}

	vars := make([]string, 0, len(needs))
	for v := range needs {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	var out []Problem
	for _, v := range vars {
		if strings.TrimSpace(s.getenv(v)) != "" {
			continue
		}
		users := needs[v]
		sort.Strings(users)
		out = append(out, Problem{
			Key:     "secret_missing:" + v,
			Message: v + " is not set; " + strings.Join(users, ", ") + " need" + plural(len(users)) + " it",
			Fix:     "add " + v + " to Secret Manager or .env",
		})
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}
