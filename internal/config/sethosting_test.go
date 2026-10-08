// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commented = `# Team config: keep this comment.
version: 1
store:
  type: sheets # the shared spreadsheet
  spreadsheet: "123"
# hosting is written by setup/gcp.sh
hosting:
  project: old-project # set by setup
pushes_enabled: false
`

func TestSetHostingKeepsCommentsAndOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadscore.yml")
	write(t, path, commented)

	if err := SetHosting(path, []string{"project=new-project", "region=asia-south1", "image=reg/img:abc"}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	text := string(out)
	for _, keep := range []string{
		"# Team config: keep this comment.", "# the shared spreadsheet",
		"# hosting is written by setup/gcp.sh", "# set by setup",
	} {
		if !strings.Contains(text, keep) {
			t.Errorf("comment %q lost:\n%s", keep, text)
		}
	}
	c, err := Parse(out, filepath.Dir(path), noEnv)
	if err != nil {
		t.Fatalf("result no longer loads: %v\n%s", err, text)
	}
	if c.Hosting.Project != "new-project" || c.Hosting.Region != "asia-south1" || c.Hosting.Image != "reg/img:abc" {
		t.Errorf("hosting = %+v", *c.Hosting)
	}
	// A numeric-looking id must stay text, and other keys must be untouched.
	if c.Store.Spreadsheet != "123" || c.Store.Type != "sheets" || c.PushesEnabled {
		t.Errorf("other keys changed: %+v", c.Store)
	}
}

func TestSetHostingCreatesTheBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadscore.yml")
	write(t, path, minimal)
	if err := SetHosting(path, []string{"project=p1", "run_account=run@p1.iam.gserviceaccount.com"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(Options{ConfigPath: path, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("hosting.project"); got != "p1" {
		t.Errorf("hosting.project = %q", got)
	}
	if got, _ := c.Get("hosting.run_account"); got != "run@p1.iam.gserviceaccount.com" {
		t.Errorf("hosting.run_account = %q", got)
	}
}

func TestSetHostingQuotesAmbiguousValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadscore.yml")
	write(t, path, minimal+"hosting:\n")
	if err := SetHosting(path, []string{"project=12345", "region=true"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(Options{ConfigPath: path, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if c.Hosting.Project != "12345" || c.Hosting.Region != "true" {
		t.Errorf("hosting = %+v", *c.Hosting)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), `"12345"`) {
		t.Errorf("a numeric value must be written quoted so it stays text:\n%s", out)
	}
}

func TestSetHostingRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leadscore.yml")
	write(t, path, minimal)
	for _, args := range [][]string{nil, {"zone=x"}, {"project"}, {"=x"}} {
		if err := SetHosting(path, args); err == nil {
			t.Errorf("SetHosting(%q) succeeded, want an error", args)
		}
	}
	if out, _ := os.ReadFile(path); string(out) != minimal {
		t.Errorf("a refused call changed the file:\n%s", out)
	}

	bundle := filepath.Join(dir, "bundle.yaml")
	write(t, bundle, "config: |\n  version: 1\n  store: { type: sqlite }\nrubric: |\n  version: 1\n")
	if err := SetHosting(bundle, []string{"project=p"}); err == nil || !strings.Contains(err.Error(), "bundle") {
		t.Errorf("writing a bundle: err = %v, want a refusal naming the bundle", err)
	}

	if err := SetHosting(filepath.Join(dir, "missing.yml"), []string{"project=p"}); err == nil {
		t.Error("a missing file must fail")
	}
}
