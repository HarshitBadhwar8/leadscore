package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Engine text keys are read exactly as written: YAML would read 0123 as a
// number and reformat a long numeric id.
func TestTextKeysKeepTheirLiteral(t *testing.T) {
	c := mustParse(t, `
version: 1
store: { type: sheets, spreadsheet: 0123, lease_bucket: 123456789012345678901234567890, view_spreadsheet: 1e3 }
hosting: { project: 00042 }
`)
	if c.Store.Spreadsheet != "0123" || c.Store.LeaseBucket != "123456789012345678901234567890" || c.Store.ViewSpreadsheet != "1e3" {
		t.Errorf("store = %+v", c.Store)
	}
	if c.Store.Block["spreadsheet"] != "0123" {
		t.Errorf("the store block must carry the literal too, got %#v", c.Store.Block["spreadsheet"])
	}
	if c.Hosting.Project != "00042" {
		t.Errorf("hosting.project = %q", c.Hosting.Project)
	}
	if got, _ := c.Get("store.spreadsheet"); got != "0123" {
		t.Errorf("Get(store.spreadsheet) = %q", got)
	}
}

func TestHostingNullOrEmptyIsNotHosted(t *testing.T) {
	for _, h := range []string{"hosting:\n", "hosting: {}\n", "hosting: null\n"} {
		c := mustParse(t, minimal+h)
		if c.Hosting != nil || c.Hosted() {
			t.Errorf("%q: Hosting = %+v, want nil", h, c.Hosting)
		}
	}
	if !mustParse(t, minimal+"hosting: { project: p }\n").Hosted() {
		t.Error("hosting.project set must count as hosted")
	}
	if mustParse(t, minimal+"hosting: { region: r }\n").Hosted() {
		t.Error("hosted means hosting.project is set")
	}
}

func TestEnrichNullIsAbsent(t *testing.T) {
	c := mustParse(t, minimal+"enrich:\n")
	if c.Enrich != nil {
		t.Errorf("enrich = %+v, want nil", c.Enrich)
	}
	if _, err := c.Get("enrich"); err == nil {
		t.Error("a null enrich block must read as not set")
	}
}

func TestReviewRejects(t *testing.T) {
	tests := []struct{ name, yml, want string }{
		{"source without type", minimal + "sources: [ { id: a } ]\n", "sources[0].type"},
		{"enrich without type", minimal + "enrich: { max_age: 30d }\n", "enrich.type"},
		{"zero max_age", minimal + "enrich: { type: apollo, max_age: 0s }\n", "enrich.max_age"},
		{"zero sequence_length", minimal + "polling: { sequence_length: 0d }\n", "polling.sequence_length"},
		{"schedule under a minute", minimal + "schedule: 30s\n", "at least 1m"},
		{"over 100 years in days", minimal + "log_retention: 36501d\n", "100 years"},
		{"over 100 years mixed", minimal + "log_retention: 36500d2000000h\n", "100 years"},
		{"huge day count", minimal + "log_retention: 99999999999999999999d\n", "100 years"},
		{"version as text", "version: \"1\"\nstore: { type: sqlite }\n", "must be the number 1, got 1 (string)"},
		{"two unknown keys", minimal + "zed: 1\nalpha: 2\n", `unknown keys "alpha", "zed"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yml), "/team", noEnv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLocateReturnsStatErrorsOtherThanMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	configDir, _ := withDefaultPaths(t)
	if err := os.Chmod(configDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(configDir, 0o755) })
	if _, err := Locate(""); err == nil {
		t.Fatal("an unreadable /config must be an error, not a silent fall-through to ./leadscore.yml")
	}
}

func TestSetHostingRefusesAnchorsAndMultipleDocuments(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"anchor": minimal + "hosting: &h { project: p }\nother: *h\n",
		"alias":  minimal + "base: &b { project: p }\nhosting: *b\n",
		"inner":  minimal + "hosting: { project: &p x, region: *p }\n",
		"multi":  minimal + "---\nversion: 1\n",
	} {
		p := filepath.Join(dir, name+".yml")
		write(t, p, text)
		err := SetHosting(p, []string{"project=q"})
		if err == nil || !(strings.Contains(err.Error(), "anchor") || strings.Contains(err.Error(), "more than one")) {
			t.Errorf("%s: err = %v, want a refusal naming the anchor or the extra document", name, err)
		}
		if got, _ := os.ReadFile(p); string(got) != text {
			t.Errorf("%s: file changed on refusal", name)
		}
	}
}

func TestSetHostingKeepsCRLF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "leadscore.yml")
	write(t, p, "version: 1\r\nstore: { type: sqlite } # local\r\n")
	if err := SetHosting(p, []string{"project=p"}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
		t.Errorf("line endings mixed:\n%q", out)
	}
	c, err := Load(Options{ConfigPath: p, Getenv: noEnv})
	if err != nil || !c.Hosted() {
		t.Fatalf("reload: %v %+v", err, c)
	}
}
