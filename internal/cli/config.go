package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
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
	v, err := client.AddSecretVersion(ctx, c.Hosting.Project, hosting.ConfigSecret, bundle)
	if err != nil {
		return inv.fail(err)
	}
	fmt.Fprintf(inv.stdout, "pushed %s and %s as %s version %s; the next run uses them\n",
		filepath.Base(path), filepath.Base(c.RubricPath), hosting.ConfigSecret, v)
	return exitOK
}
