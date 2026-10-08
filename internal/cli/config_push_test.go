// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcp"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// fakeGCP points the commands' Google Cloud client at a fake.
func fakeGCP(t *testing.T) *gcp.Server {
	t.Helper()
	f := gcp.New()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := gcpConnector
	gcpConnector = func(ctx context.Context) (*hosting.Client, error) {
		return hosting.Connect(ctx, api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	}
	t.Cleanup(func() { gcpConnector = old })
	return f
}

const pushRubric = `version: 1
score:
  contact:
    - { when: { field: title, present: true }, points: 1 }
lanes:
  - { id: everyone, kind: export, priority: 1, when: { field: title, present: true }, push: "export:everyone" }
`

const pushConfig = `version: 1
# the team's settings
store: { type: sheets, spreadsheet: "0123", lease_bucket: b }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]
hosting: { project: p, region: asia-south1, run_account: leadscore-run }
`

func pushInstall(t *testing.T, cfg, rubric string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"leadscore.yml": cfg, "rubric.yml": rubric} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "leadscore.yml")
}

// config push uploads both files as one new version, which loads back as the
// same pair: the run reads exactly what was reviewed.
func TestConfigPush(t *testing.T) {
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.ConfigSecret)
	f.CreateSecret("p", hosting.ConfigVersionSecret)
	path := pushInstall(t, pushConfig, pushRubric)
	code, stdout, stderr := run("config", "push", "--config", path)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "as leadscore-config version 1") {
		t.Errorf("stdout %q", stdout)
	}
	versions := f.Versions("p", hosting.ConfigSecret)
	if len(versions) != 1 {
		t.Fatalf("%d versions", len(versions))
	}
	bundle := filepath.Join(t.TempDir(), "bundle.yaml")
	_ = os.WriteFile(bundle, versions[0], 0o600)
	c, err := config.Load(config.Options{ConfigPath: bundle})
	if err != nil {
		t.Fatal(err)
	}
	rubric, _ := c.Rubric()
	if !c.Bundle || string(rubric) != pushRubric || c.Store.Spreadsheet != "0123" {
		t.Errorf("bundle loads as bundle=%v spreadsheet=%q rubric=%q", c.Bundle, c.Store.Spreadsheet, rubric)
	}
	if !strings.Contains(string(versions[0]), "# the team's settings") {
		t.Error("the file must be uploaded as written, comments included")
	}

	// --rubric picks the rubric pushed; a second push is version 2.
	other := filepath.Join(t.TempDir(), "other.yml")
	_ = os.WriteFile(other, []byte(pushRubric+"# v2\n"), 0o600)
	if code, stdout, stderr := run("config", "push", "--config", path, "--rubric", other); code != exitOK || !strings.Contains(stdout, "version 2") {
		t.Fatalf("second push: exit %d %q %q", code, stdout, stderr)
	}
	if v := f.Versions("p", hosting.ConfigSecret); !strings.Contains(string(v[1]), "# v2") {
		t.Error("--rubric was not the rubric pushed")
	}
	// Each push records the bundle's version number, which the job reads.
	if got := f.Versions("p", hosting.ConfigVersionSecret); len(got) != 2 || string(got[0]) != "1" || string(got[1]) != "2" {
		t.Errorf("%s holds %q, want 1 then 2", hosting.ConfigVersionSecret, got)
	}
}

// When the version number cannot be recorded, the push fails and says to run
// it again, since runs would record the wrong version.
func TestConfigPushVersionWriteFails(t *testing.T) {
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.ConfigSecret)
	f.CreateSecret("p", hosting.ConfigVersionSecret)
	f.DenyWrites("p", hosting.ConfigVersionSecret)
	code, _, stderr := run("config", "push", "--config", pushInstall(t, pushConfig, pushRubric))
	if code != exitFail || !strings.Contains(stderr, "run `leadscore config push` again") || !strings.Contains(stderr, "version 1") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// No key ever goes into the bundle: a token pattern, the value of a key
// variable, or an adapter key named like a credential.
func TestConfigPushRefusesKeys(t *testing.T) {
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.ConfigSecret)
	f.CreateSecret("p", hosting.ConfigVersionSecret)
	f.CreateSecret("p", "apollo-api-key")
	f.AddVersion("p", "apollo-api-key", []byte("apollo-stored-key-55555\n"))
	t.Setenv("APOLLO_API_KEY", "")
	t.Setenv("HUBSPOT_TOKEN", "hubspot-env-token-98765")
	for _, tt := range []struct{ name, cfg, rubric, want string }{
		{"a token pattern in the rubric", pushConfig, pushRubric + "# pat-na1-12345678-1234-1234-1234-123456789012\n", "rubric.yml line 7 holds what looks like a key"},
		{"a key variable's value", pushConfig + "# hubspot-env-token-98765\n", pushRubric, "leadscore.yml line 6 holds the value of HUBSPOT_TOKEN"},
		{"a key stored in Secret Manager, not in the environment", pushConfig, pushRubric + "# apollo-stored-key-55555\n", "rubric.yml line 7 holds the key stored in secret apollo-api-key"},
		{"an adapter key named like a credential", pushConfig + "sinks: { apollo: { mailbox_id: m, api_key: x } }\n", pushRubric, `leadscore.yml line 6: sinks.apollo has a key named "api_key"`},
		{"a nested one", pushConfig + "enrich:\n  type: apollo\n  auth:\n    password: x\n", pushRubric, `leadscore.yml line 9: enrich has a key named "password"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := run("config", "push", "--config", pushInstall(t, tt.cfg, tt.rubric))
			if code != exitFail || !strings.Contains(stderr, tt.want) || strings.Contains(stderr, "98765") || strings.Contains(stderr, "55555") {
				t.Errorf("exit %d, stderr %q; want %q", code, stderr, tt.want)
			}
		})
	}
	if n := len(f.Versions("p", hosting.ConfigSecret)); n != 0 {
		t.Errorf("%d versions uploaded", n)
	}
	// Ordinary words that merely resemble a token format pass.
	ok := pushRubric + "# a bearer bond buyer; lane ids like sk-leads\n"
	if code, _, stderr := run("config", "push", "--config", pushInstall(t, pushConfig, ok)); code != exitOK {
		t.Errorf("an ordinary rubric was refused: %s", stderr)
	}
}

// Nothing a hosted run would refuse is uploaded.
func TestConfigPushRefusals(t *testing.T) {
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.ConfigSecret)
	for _, tt := range []struct {
		name, cfg, rubric, want string
	}{
		{"not hosted", strings.Replace(pushConfig, "project: p, ", "", 1), pushRubric, "hosting.project is not set"},
		{"sqlite", strings.Replace(pushConfig, `{ type: sheets, spreadsheet: "0123", lease_bucket: b }`, "{ type: sqlite }", 1), pushRubric, "SQLite"},
		{"csv path", strings.Replace(pushConfig, "sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]",
			"sources: [ { id: leads, type: sheetsource, tabs: [Leads] }, { id: more, type: csv, path: more.csv } ]", 1), pushRubric, "CSV path"},
		{"key file", strings.Replace(pushConfig, "lease_bucket: b", "lease_bucket: b, credentials: key.json", 1), pushRubric, "store.credentials"},
		{"schedule with no cron form", pushConfig + "schedule: 7m\n", pushRubric, "cannot run on Cloud Scheduler"},
		{"runs would overlap", pushConfig + "deadline: 14m\n", pushRubric, "runs would overlap"},
		{"rubric does not compile", pushConfig, "fields: [\n", "does not compile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := run("config", "push", "--config", pushInstall(t, tt.cfg, tt.rubric))
			if code != exitFail || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want %q", code, stderr, tt.want)
			}
		})
	}
	if n := len(f.Versions("p", hosting.ConfigSecret)); n != 0 {
		t.Errorf("%d versions uploaded by refused pushes", n)
	}
}

// Without the version secret, nothing is uploaded: the bundle could not have
// its number recorded.
func TestConfigPushNeedsTheVersionSecretFirst(t *testing.T) {
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.ConfigSecret)
	code, _, stderr := run("config", "push", "--config", pushInstall(t, pushConfig, pushRubric))
	if code != exitFail || !strings.Contains(stderr, "leadscore-config-version") || !strings.Contains(stderr, "setup/gcp.sh secrets") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if n := len(f.Versions("p", hosting.ConfigSecret)); n != 0 {
		t.Errorf("the bundle was uploaded (%d versions) though its number could not be recorded", n)
	}
}

func TestConfigPushNeedsTheSecret(t *testing.T) {
	fakeGCP(t)
	code, _, stderr := run("config", "push", "--config", pushInstall(t, pushConfig, pushRubric))
	if code != exitFail || !strings.Contains(stderr, "setup/gcp.sh secrets") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// On a hosted install with no HUBSPOT_TOKEN set, setup hubspot reads the
// token from Secret Manager, and only that key.
func TestSetupHubSpotReadsTheTokenFromSecretManager(t *testing.T) {
	t.Cleanup(func() { logredact.MaskEnvSecrets(func(string) string { return "" }) })
	_, url := fakeHubSpot(t)
	t.Setenv("HUBSPOT_TOKEN", "")
	t.Setenv("APOLLO_API_KEY", "")
	f := fakeGCP(t)
	f.CreateSecret("p", "hubspot-token")
	f.AddVersion("p", "hubspot-token", []byte(fakehub.Token))
	f.CreateSecret("p", "apollo-api-key")
	f.AddVersion("p", "apollo-api-key", []byte("apollo-key-not-needed"))
	path := writeConfig(t, fmt.Sprintf("version: 1\nstore: { type: sqlite, path: x.db }\nenrich: { type: apollo }\n"+
		"hosting: { project: p }\nsinks:\n  hubspot: { base_url: %q, pipeline: %q, stage: %q }\n",
		url, fakehub.PipelineLabel, fakehub.StageOpenName))
	code, _, stderr := run("setup", "hubspot", "--config", path)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if os.Getenv("APOLLO_API_KEY") != "" {
		t.Error("setup hubspot read the Apollo key, which it does not use")
	}
}
