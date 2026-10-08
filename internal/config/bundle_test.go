package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Whatever the two files hold, the bundle reads back as exactly them.
func TestMakeBundleRoundTrips(t *testing.T) {
	cfg := "version: 1\n# comment\nstore: { type: sheets, spreadsheet: \"0123\" }\n"
	for _, rubric := range []string{
		"version: 1\n",
		"version: 1",             // no final newline
		"  version: 1\n\tx: y\n", // leading indentation, a tab
		"version: 1   \nname: trailing spaces   \n", // trailing spaces
		"version: 1\n---\n...\n",
		"version: 1\r\nwindows: yes\r\n",
		"version: 1\n\n\n",
	} {
		b, err := MakeBundle([]byte(cfg), []byte(rubric))
		if err != nil {
			t.Fatalf("%q: %v", rubric, err)
		}
		path := filepath.Join(t.TempDir(), "bundle.yaml")
		_ = os.WriteFile(path, b, 0o600)
		c, err := Load(Options{ConfigPath: path, Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatalf("%q: %v", rubric, err)
		}
		got, _ := c.Rubric()
		if !c.Bundle || string(got) != rubric || c.Store.Spreadsheet != "0123" {
			t.Errorf("%q read back as %q (bundle %v)", rubric, got, c.Bundle)
		}
	}
	if _, err := MakeBundle([]byte(cfg), []byte(strings.Repeat("x", MaxBundleBytes))); err == nil {
		t.Error("a bundle over Secret Manager's 64 KiB must be refused")
	}
	if _, err := MakeBundle([]byte(cfg), nil); err == nil {
		t.Error("an empty rubric must be refused")
	}
}

// On Google Cloud store.lease_bucket defaults to <project>-leadscore-lease,
// and config get returns it; a name the team sets wins.
func TestLeaseBucketDefaultsFromTheProject(t *testing.T) {
	base := "version: 1\nstore: { type: sheets, spreadsheet: s }\nhosting: { project: team-proj }\n"
	c, err := Parse([]byte(base), "/c", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Get("store.lease_bucket")
	if c.Store.LeaseBucket != "team-proj-leadscore-lease" || got != c.Store.LeaseBucket || c.Store.Block["lease_bucket"] != c.Store.LeaseBucket {
		t.Errorf("lease bucket %q, config get %q, block %v", c.Store.LeaseBucket, got, c.Store.Block["lease_bucket"])
	}
	c, _ = Parse([]byte(strings.Replace(base, "spreadsheet: s", "spreadsheet: s, lease_bucket: mine", 1)), "/c", func(string) string { return "" })
	if c.Store.LeaseBucket != "mine" {
		t.Errorf("a set bucket was replaced: %q", c.Store.LeaseBucket)
	}
	c, _ = Parse([]byte("version: 1\nstore: { type: sheets, spreadsheet: s }\n"), "/c", func(string) string { return "" })
	if c.Store.LeaseBucket != "" {
		t.Errorf("not hosted: no default, got %q", c.Store.LeaseBucket)
	}
}
