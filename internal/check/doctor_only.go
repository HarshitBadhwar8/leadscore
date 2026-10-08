package check

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

// The checks only doctor runs: rubric-version,
// receivers, lease and pushes-enabled. None of them runs inside a run, so
// their problem keys never reach Health.
func init() {
	Register(rubricVersion{})
	// Redirects are not followed: Apollo posts to public_url itself, so an
	// address that answers with a redirect is not one Apollo can deliver to.
	Register(receivers{client: &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	Register(leaseCheck{})
	Register(pushesEnabled{})
}

// RubricFor is the rubric a check judges: the run's (Env.Rubric), else, in
// doctor, the local file compiled; nil when it does
// not compile (the rubric check says why).
func RubricFor(env Env) *rules.Rubric {
	if env.Rubric != nil {
		return env.Rubric
	}
	if env.Config == nil {
		return nil
	}
	text, err := env.Config.Rubric()
	if err != nil {
		return nil
	}
	r, err := rules.Compile(text)
	if err != nil {
		return nil
	}
	return r
}

// rubricVersion is the `rubric-version` check (a warning): the rubric the
// last run scored with (Health's rubric_version) is not the local file, so
// the dry run a person reviewed is not what runs.
type rubricVersion struct{}

func (rubricVersion) Name() string { return "rubric-version" }
func (rubricVersion) InRun() bool  { return false }

func (rubricVersion) Run(_ context.Context, env Env) []Problem {
	if env.Model == nil {
		return nil
	}
	stored := env.Model.Health[model.K("result", "rubric_version")].Value
	r := RubricFor(env)
	if stored == "" || r == nil || stored == r.Version() {
		return nil
	}
	fix := "the next run reads the local file; review `leadscore run --dry-run` before it does"
	if env.Config.Hosted() {
		fix = "`leadscore config push` uploads the local rubric, or put back the file the run used"
	}
	return []Problem{{Key: "rubric-version:differs", Warning: true,
		Message: fmt.Sprintf("the last run scored with rubric %s, but the local rubric is %s", stored, r.Version()),
		Fix:     fix}}
}

// receivers is the `receivers` check: when the install expects webhooks, the
// receiver's public address must answer on /healthz, as Apollo would reach
// it. Without receiver.public_url it warns, since then nothing outside can be
// probed and receiver-silence is off.
type receivers struct{ client *http.Client }

func (receivers) Name() string { return "receivers" }
func (receivers) InRun() bool  { return false }

func (c receivers) Run(ctx context.Context, env Env) []Problem {
	if env.Config == nil || !ReceiverConfigured(env.Config) {
		return nil
	}
	fix := "re-deploy (Google Cloud: `setup/gcp.sh deploy`; a server: `docker compose --profile caddy up -d`); " +
		"on a laptop, use `replies: polling` or a Cloudflare Tunnel"
	raw := strings.TrimSpace(env.Config.Receiver.PublicURL)
	if raw == "" {
		return []Problem{{Key: "receivers:no_public_url", Warning: true,
			Message: "receiver.public_url is not set, so doctor cannot check that Apollo reaches the receiver, and receiver-silence is off",
			Fix: "set receiver.public_url to the address the Apollo workflows post to (Google Cloud: `setup/gcp.sh deploy` prints it); " +
				"a CSV-only install can ignore this"}}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return []Problem{{Key: "receivers:public_url",
			Message: fmt.Sprintf("receiver.public_url %q is not an http or https address", raw),
			Fix:     "set receiver.public_url to the receiver's address, like https://leads.example.com"}}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/healthz"
	u.RawQuery, u.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return []Problem{{Key: "receivers:unreachable", Message: err.Error(), Fix: fix}}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return []Problem{{Key: "receivers:unreachable",
			Message: fmt.Sprintf("%s did not answer: %v", u.Redacted(), err), Fix: fix}}
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusServiceUnavailable:
		// Reachable: /healthz itself reports the runs as unhealthy.
		return []Problem{{Key: "receivers:unhealthy", Warning: true,
			Message: u.Redacted() + " answers, but reports unhealthy: the last run failed or none succeeded in three intervals",
			Fix:     "`leadscore status` shows why"}}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Apollo does not follow redirects; the usual cause is http that
		// redirects to https.
		return []Problem{{Key: "receivers:unreachable",
			Message: fmt.Sprintf("%s answered %d, a redirect to %q, which Apollo would not follow", u.Redacted(), resp.StatusCode, resp.Header.Get("Location")),
			Fix:     "set receiver.public_url to the final https address (where the redirect points), and use it in the Apollo workflows"}}
	}
	return []Problem{{Key: "receivers:unreachable",
		Message: fmt.Sprintf("%s answered %d, not leadscore's /healthz", u.Redacted(), resp.StatusCode), Fix: fix}}
}

// leaseCheck is the `lease` check: on Sheets, the lease bucket must exist and
// be writable by this account. On any store that can show its lease, a held
// lease is shown with its owner and expiry, as a warning. It never takes the
// lease.
type leaseCheck struct{}

func (leaseCheck) Name() string { return "lease" }
func (leaseCheck) InRun() bool  { return false }

func (leaseCheck) Run(ctx context.Context, env Env) []Problem {
	var out []Problem
	if s, ok := env.Store.(*sheets.Store); ok {
		fix := "run `setup/gcp.sh bucket` (on Docker, create the bucket and give the service account Storage Object Admin on it)"
		exists, writable, err := s.LeaseBucketAccess(ctx)
		switch {
		case err != nil:
			out = append(out, Problem{Key: "lease:bucket", Message: err.Error(), Fix: fix})
		case !exists:
			out = append(out, Problem{Key: "lease:bucket",
				Message: "the lease bucket gs://" + s.LeaseBucket() + " does not exist, so no run can take the lease", Fix: fix})
		case !writable:
			out = append(out, Problem{Key: "lease:bucket",
				Message: "this account cannot write the lease file in gs://" + s.LeaseBucket() + ", so no run can take the lease", Fix: fix})
		}
		if len(out) > 0 {
			return out
		}
	}
	li, ok := env.Store.(api.LeaseInspector)
	if !ok {
		return out
	}
	owner, expires, err := li.LeaseInfo(ctx)
	if err != nil || owner == "" || !expires.After(env.Clock()) {
		return out
	}
	return append(out, Problem{Key: "lease:held", Warning: true,
		Message: fmt.Sprintf("run %s holds the run lease until %s (a run is in progress, or one stopped without releasing it)",
			owner, model.FormatTime(expires)),
		Fix: "nothing: it clears itself at expiry"})
}

// pushesEnabled is the `pushes-enabled` check (a warning while pushes are
// off). It also names every cold lane whose sink has no sinks.<type> block: a
// cold lane claims the leads it matches even before its sink is set up, so
// they are do_not_contact on every export list.
type pushesEnabled struct{}

func (pushesEnabled) Name() string { return "pushes-enabled" }
func (pushesEnabled) InRun() bool  { return false }

func (pushesEnabled) Run(_ context.Context, env Env) []Problem {
	c := env.Config
	if c == nil {
		return nil
	}
	var out []Problem
	if !c.PushesEnabled {
		fix := "review `leadscore run --dry-run`, then set pushes_enabled: true in leadscore.yml"
		if c.Hosted() {
			fix += " and `leadscore config push`"
		}
		if len(c.Sinks) == 0 {
			// A CSV-only install has nothing to push: no advice to turn it on.
			fix = "nothing to do while no sinks block is set up (a CSV-only install); once one is, " + fix
		}
		out = append(out, Problem{Key: "pushes-enabled:off", Warning: true,
			Message: "pushes_enabled is false: runs score leads and keep the export lists, and push nothing",
			Fix:     fix})
	}
	if r := RubricFor(env); r != nil {
		for _, l := range r.Lanes() {
			if l.Kind != "cold" {
				continue
			}
			if _, ok := c.Sinks[l.Sink]; ok {
				continue
			}
			out = append(out, Problem{Key: "cold_lane_no_sink:" + l.ID, Warning: true,
				Message: fmt.Sprintf("cold lane %s pushes to %s, which has no sinks.%s block, yet it already claims every lead it matches: "+
					"they show do_not_contact yes on the export lists", l.ID, l.Sink, l.Sink),
				Fix: fmt.Sprintf("set up sinks.%s, or remove the lane (a CSV-only team lists leads through export lanes only)", l.Sink)})
		}
	}
	return out
}
