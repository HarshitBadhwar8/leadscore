package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// gcpConnector is how commands reach Google Cloud (Secret Manager); nil is
// Google's standard credentials, the run account after setup/gcp.sh accounts.
// Tests point it at a fake.
var gcpConnector hosting.Connector

// runConfigGet prints one effective configuration value (contracts section 3).
func runConfigGet(inv *invocation) int {
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	v, err := c.Get(inv.args[0])
	if err != nil {
		return inv.fail(err)
	}
	fmt.Fprintln(inv.stdout, v)
	return exitOK
}

// runConfigSetHosting writes the hosting block of leadscore.yml, keeping comments.
func runConfigSetHosting(inv *invocation) int {
	path, err := config.Locate(inv.flags["config"])
	if err != nil {
		return inv.fail(err)
	}
	if err := config.SetHosting(path, inv.args); err != nil {
		return inv.fail(err)
	}
	fmt.Fprintf(inv.stdout, "updated hosting in %s\n", path)
	return exitOK
}

// runConfigPush uploads leadscore.yml and the rubric together as one new
// version of the leadscore-config secret (contracts section 3, "Hosted
// bundle"); the next run reads it. It refuses anything a hosted run would
// refuse, so a broken pair is never uploaded.
func runConfigPush(inv *invocation) int {
	ctx := context.Background()
	path, err := config.Locate(inv.flags["config"])
	if err != nil {
		return inv.fail(err)
	}
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	if c.Bundle {
		return inv.fail(fmt.Errorf("%s is a hosted bundle; run config push in the folder with your leadscore.yml and rubric", path))
	}
	if !c.Hosted() {
		return inv.fail(errors.New("hosting.project is not set: config push is for Google Cloud installs; run `setup/gcp.sh accounts` first"))
	}
	if c.Store.Credentials != "" {
		return inv.fail(errors.New("store.credentials names a key file, which Cloud Run does not have: it signs in as the run account; remove store.credentials"))
	}
	inJob := func(k string) string {
		if k == "CLOUD_RUN_JOB" {
			return hosting.JobName
		}
		return ""
	}
	if ps := check.CloudRunRefusal(c, inJob); len(ps) > 0 {
		return inv.fail(fmt.Errorf("%s (%s)", ps[0].Message, ps[0].Fix))
	}
	if err := hosting.CheckSchedule(c); err != nil {
		return inv.fail(err)
	}
	configText, err := os.ReadFile(path)
	if err != nil {
		return inv.fail(err)
	}
	rubricText, err := c.Rubric()
	if err != nil {
		return inv.fail(err)
	}
	if _, err := rules.Compile(rubricText); err != nil {
		return inv.fail(fmt.Errorf("the rubric does not compile (leadscore rules check lists every error): %w", err))
	}
	if err := keysInBundle(c, configText, rubricText); err != nil {
		return inv.fail(err)
	}
	bundle, err := config.MakeBundle(configText, rubricText)
	if err != nil {
		return inv.fail(err)
	}
	// The bundle must load the way a run will load it.
	dir, err := os.MkdirTemp("", "leadscore-push-")
	if err != nil {
		return inv.fail(err)
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "bundle.yaml")
	if err := os.WriteFile(tmp, bundle, 0o600); err != nil {
		return inv.fail(err)
	}
	if _, err := config.Load(config.Options{ConfigPath: tmp, Getenv: func(string) string { return "" }}); err != nil {
		return inv.fail(fmt.Errorf("the bundle would not load on Google Cloud: %w", err))
	}
	client, err := gcpConnector.Open(ctx)
	if err != nil {
		return inv.fail(err)
	}
	// The bundle first, then its version number, which the run job reads
	// into LEADSCORE_CONFIG_VERSION (contracts section 3).
	v, err := client.AddSecretVersion(ctx, c.Hosting.Project, hosting.ConfigSecret, bundle)
	if err != nil {
		return inv.fail(err)
	}
	if _, err := client.AddSecretVersion(ctx, c.Hosting.Project, hosting.ConfigVersionSecret, []byte(v)); err != nil {
		return inv.fail(fmt.Errorf("uploaded %s version %s, but recording its number in %s failed, so runs would record the wrong version: "+
			"run `leadscore config push` again (%w)", hosting.ConfigSecret, v, hosting.ConfigVersionSecret, err))
	}
	fmt.Fprintf(inv.stdout, "pushed %s and %s as %s version %s; the next run uses them\n",
		filepath.Base(path), filepath.Base(c.RubricPath), hosting.ConfigSecret, v)
	return exitOK
}

// keyName is an adapter-block key that names a credential.
var keyName = regexp.MustCompile(`(?i)key|token|secret|password`)

// keysInBundle refuses a bundle that carries a key: Secret Manager's
// leadscore-config is readable by the receiver account and shown in the
// console, while keys belong in their own secrets (setup/gcp.sh secrets).
func keysInBundle(c *config.Config, texts ...[]byte) error {
	fix := "; keys go in Secret Manager with `setup/gcp.sh secrets`, never in leadscore.yml or the rubric"
	for _, t := range texts {
		for _, name := range logredact.SecretVariables {
			if v := strings.TrimSpace(os.Getenv(name)); len(v) >= 6 && strings.Contains(string(t), v) { // as logredact, a tiny value is no key
				return errors.New("leadscore.yml or the rubric holds the value of " + name + fix)
			}
		}
		if logredact.ContainsSecret(string(t)) {
			return errors.New("leadscore.yml or the rubric holds what looks like a key or token" + fix)
		}
	}
	blocks := map[string]any{"store": map[string]any(c.Store.Block)}
	for _, src := range c.Sources {
		blocks["sources."+src.ID] = map[string]any(src.Block)
	}
	if c.Enrich != nil {
		blocks["enrich"] = map[string]any(c.Enrich.Block)
	}
	for typ, b := range c.Sinks {
		blocks["sinks."+typ] = map[string]any(b)
	}
	for where, b := range blocks {
		if k := credentialKey(b); k != "" {
			return fmt.Errorf("%s has a key named %q, which looks like a credential%s", where, k, fix)
		}
	}
	return nil
}

// credentialKey returns the first key, at any depth, whose name matches keyName.
func credentialKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if keyName.MatchString(k) {
				return k
			}
			if found := credentialKey(t[k]); found != "" {
				return found
			}
		}
	case []any:
		for _, x := range t {
			if found := credentialKey(x); found != "" {
				return found
			}
		}
	}
	return ""
}
