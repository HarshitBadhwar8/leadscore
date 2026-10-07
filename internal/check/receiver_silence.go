package check

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func init() { Register(receiverSilence{}) }

// State keys the check reads (contracts section 4); Intake and the run write
// them.
const (
	lastReceivedPrefix = "last_received:"
	firstRunAtKey      = "first_run_at"
)

// receiverSilence is the receiver-silence check (contracts sections 5.3 and
// 10, RFC 6.12): reachable is not delivering, so when the receiver is
// configured and set up (receiver.public_url), every event kind it expects must have arrived within
// silence_threshold. Silence is measured from the later of the kind's newest
// received event (State last_received:<kind>) and the first run (State
// first_run_at), so a new install is not flagged before it could hear
// anything. Each silent kind raises silent:<kind>, which makes the run
// unhealthy.
type receiverSilence struct{}

func (receiverSilence) Name() string { return "receiver-silence" }
func (receiverSilence) InRun() bool  { return true }

func (receiverSilence) Run(_ context.Context, env Env) []Problem {
	// Only a receiver that was set up (it has a public address) can fall
	// silent; an install that never set one up is not flagged forever.
	if env.Config == nil || env.Model == nil || !ReceiverConfigured(env.Config) ||
		strings.TrimSpace(env.Config.Receiver.PublicURL) == "" {
		return nil
	}
	m := env.Model
	firstRun, err := model.ParseTime(m.StateValue(firstRunAtKey))
	if err != nil || firstRun.IsZero() {
		return nil // no run yet: nothing could have arrived
	}
	now := env.Clock()
	threshold := env.Config.SilenceThreshold
	var out []Problem
	for _, kind := range ExpectedKinds(env.Config) {
		since := firstRun
		last, err := model.ParseTime(m.StateValue(lastReceivedPrefix + kind))
		heard := err == nil && !last.IsZero()
		if heard && last.After(now) {
			last = now // a received time in the future (a clock that ran ahead) counts as now
		}
		if heard && last.After(since) {
			since = last
		}
		if now.Sub(since) <= threshold {
			continue
		}
		what := "no " + kind + " event has arrived since the first run, at " + model.FormatTime(firstRun)
		if heard {
			what = "the last " + kind + " event arrived at " + model.FormatTime(last)
		}
		out = append(out, Problem{
			Key:     "silent:" + kind,
			Message: fmt.Sprintf("the receiver expects %s events, but %s, more than silence_threshold (%s) ago", kind, what, threshold),
			Fix:     "check the Apollo workflow that sends " + kind + " (its URL, secret and trigger) and that the receiver is reachable",
		})
	}
	return out
}

// ExpectedKinds are the event kinds the receiver expects (contracts 5.3):
// the receiver.visit_events kinds, and `sent` when replies come from the
// receiver. Lowercased and sorted, each once.
func ExpectedKinds(c *config.Config) []string {
	set := map[string]bool{}
	if c.Replies == "receiver" {
		set["sent"] = true
	}
	for _, k := range c.Receiver.VisitEvents {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
