package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// DoctorOrder is the doctor checks table, in its order: doctor prints
// its checks in this order, then any other registered check (a plug-in's).
var DoctorOrder = []string{
	"secrets", "receiver-secret", "hosting", "rubric-version", "sheet-access", "sheets", "hubspot",
	"apollo-key", "apollo-sequences", "receivers", "receiver-silence", "lease", "pushes", "store",
	"rubric", "overrides", "pushes-enabled", "duplicates",
}

// modelChecks read only the loaded model: without it doctor prints them as
// skipped rather than as passing.
var modelChecks = map[string]bool{
	"rubric-version": true, "receiver-silence": true, "pushes": true, "overrides": true, "duplicates": true,
}

// storeChecks need the open store: without it doctor prints them as skipped.
var storeChecks = map[string]bool{"lease": true}

// runDoctor is `leadscore doctor`: one line per check,
// each problem with its suggested fix. It exits 0 when no check fails
// (warnings allowed) and 1 otherwise. It never writes the store and never
// takes the lease: the store is opened and its model loaded read-only, and a
// SQLite file that does not exist yet is not created.
func runDoctor(inv *invocation) int {
	ctx := context.Background()
	w := inv.stdout
	c, err := config.Load(inv.configOptions())
	if err != nil {
		fmt.Fprintf(w, "FAIL  config: %s\n      fix: correct leadscore.yml (every key is in docs/reference.md)\n", doctorText(err.Error()))
		fmt.Fprintln(w, "doctor: 1 failed; the other checks need a configuration that loads")
		return exitFail
	}
	// A hosted install's keys live in Secret Manager: read the ones the
	// configured adapters need, so their checks run as a run's would. A key
	// that cannot be read stays empty, and `secrets` says so.
	if err := hosting.LoadKeys(ctx, c, os.Getenv, os.Setenv, gcpConnector); err != nil {
		fmt.Fprintf(w, "note  keys from Secret Manager: %s\n", doctorText(err.Error()))
	}
	addTestClients(c)

	env := check.Env{Config: c, Doctor: true}
	extra := map[string][]check.Problem{} // problems doctor itself finds, shown under a check
	skipped := ""                         // why the model checks did not run
	if c.Store.Type == "sqlite" && !fileExists(c.Store.Path) {
		skipped = "no SQLite file yet at " + c.Store.Path
		extra["store"] = append(extra["store"], check.Problem{Key: "store:not_created", Warning: true,
			Message: "there is no SQLite file at " + c.Store.Path + " yet: no run has saved, so the checks that read the store were skipped",
			Fix:     "wait for the first run (Docker runs one when the container starts), or run `leadscore run`"})
	} else if open, ok := api.BackendFactory(c.Store.Type); !ok {
		skipped = "the store type is not in this build"
		extra["store"] = append(extra["store"], check.Problem{Key: "store:unregistered",
			Message: fmt.Sprintf("store type %q is not registered in this build", c.Store.Type),
			Fix:     "use sqlite or sheets, or a build that registers the store"})
	} else if b, ev, err := openReadOnly(c, open); err != nil {
		skipped = "the store did not open"
		extra["store"] = append(extra["store"], check.Problem{Key: "store:open",
			Message: "the store cannot be opened: " + err.Error(), Fix: "check the store block in leadscore.yml and its access"})
	} else {
		if cl, ok := b.(io.Closer); ok {
			defer cl.Close()
		}
		env.Store, env.Events = b, ev
		// Load only reads; doctor never encodes or commits what it holds.
		m, err := codec.Load(ctx, b)
		switch {
		case errors.Is(err, codec.ErrNewerSchema), errors.Is(err, codec.ErrBadVersion):
			skipped = "the store did not load" // the store check names the version
		case err != nil:
			skipped = "the store did not load"
			extra["store"] = append(extra["store"], check.Problem{Key: "store:load",
				Message: "the store cannot be loaded: " + err.Error(), Fix: "the message names the table, row and column to fix"})
		default:
			env.Model = m
		}
	}

	failed, warned, passed := 0, 0, 0
	for _, ck := range doctorChecks() {
		name := ck.Name()
		probs := extra[name]
		if (env.Model == nil && modelChecks[name]) || (env.Store == nil && storeChecks[name]) {
			fmt.Fprintf(w, "skip  %s (%s)\n", name, skipped)
			continue
		}
		probs = append(probs, ck.Run(ctx, env)...)
		if len(probs) == 0 {
			fmt.Fprintf(w, "ok    %s\n", name)
			passed++
			continue
		}
		for _, p := range probs {
			tag := "FAIL"
			if p.Warning {
				tag = "warn"
				warned++
			} else {
				failed++
			}
			fmt.Fprintf(w, "%s  %s: %s: %s\n", tag, name, doctorText(p.Key), doctorText(p.Message))
			if p.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", doctorText(p.Fix))
			}
		}
	}
	fmt.Fprintf(w, "doctor: %d failed, %d warnings, %d checks ok\n", failed, warned, passed)
	if failed > 0 {
		return exitFail
	}
	return exitOK
}

// openReadOnly opens the store for doctor: the SQLite file in SQLite's
// read-only mode (no chmod, no journal change, no WAL folded in), any other
// store through its factory, whose opening reads only.
func openReadOnly(c *config.Config, open func(api.Config) (api.Backend, api.EventLog, error)) (api.Backend, api.EventLog, error) {
	if c.Store.Type == "sqlite" {
		s, err := sqlite.OpenReadOnly(c.Store.Path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}
	return open(c.Store.Block)
}

// doctorChecks are every registered check, DoctorOrder's in its order first.
func doctorChecks() []check.Check {
	byName := map[string]check.Check{}
	var rest []check.Check
	known := map[string]bool{}
	for _, n := range DoctorOrder {
		known[n] = true
	}
	for _, ck := range check.All() {
		byName[ck.Name()] = ck
		if !known[ck.Name()] {
			rest = append(rest, ck)
		}
	}
	var out []check.Check
	for _, n := range DoctorOrder {
		if ck, ok := byName[n]; ok {
			out = append(out, ck)
		}
	}
	return append(out, rest...)
}

// doctorText makes stored or vendor text safe to print: no control
// characters, and redacted inside Cloud Run, where stdout is Cloud Logging.
func doctorText(s string) string {
	if inCloudRun() {
		s = logredact.Redact(s)
	}
	return printable(strings.TrimSuffix(s, "."))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// addTestClients gives the Google and HubSpot blocks the test HTTP clients
// when a test points them at a fake with base_url; outside tests the clients
// are nil, so a base_url in a real file is still refused.
func addTestClients(c *config.Config) {
	set := func(b api.Config, client *http.Client) {
		if base, _ := b["base_url"].(string); base != "" && client != nil {
			b["_http_client"] = client
		}
	}
	set(c.Store.Block, testGoogleClient)
	for _, s := range c.Sources {
		set(s.Block, testGoogleClient)
	}
	if b, ok := c.Sinks["hubspot"]; ok && b != nil {
		set(b, testHubSpotClient)
	}
}
