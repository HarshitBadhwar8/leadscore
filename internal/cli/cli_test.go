package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

// Every command in RFC 6.13 exists; each not yet built says which slice builds it.
func TestUnbuiltCommandsSayWhichSlice(t *testing.T) {
	tests := []struct {
		args  []string
		slice string
	}{
		{[]string{"run"}, "S10a"},
		{[]string{"run", "--dry-run"}, "S10a"},
		{[]string{"explain", "a@example.com"}, "S10a"},
		{[]string{"doctor"}, "S16"},
		{[]string{"ranked", "--csv"}, "S10a"},
		{[]string{"serve"}, "S14a"},
		{[]string{"serve", "--every"}, "S14a"},
		{[]string{"serve", "--every", "5m"}, "S14a"},
		{[]string{"status"}, "S10a"},
		{[]string{"set-status", "a@example.com", "none"}, "S6"},
		{[]string{"merge", "a@example.com", "b@example.com"}, "S6"},
		{[]string{"mark-distinct", "a@example.com", "b@example.com"}, "S6"},
		{[]string{"retry"}, "S6"},
		{[]string{"retry", "--lane", "warm", "a@example.com"}, "S6"},
		{[]string{"config", "push"}, "S14b"},
		{[]string{"setup", "hubspot"}, "S11"},
		{[]string{"rules", "check", "rubric.yml"}, "S2"},
		{[]string{"setup", "sheet", "--view", "--repair"}, "S5"},
		{[]string{"healthz"}, "S14a"},
		{[]string{"--config", "x.yml", "--rubric", "r.yml", "doctor"}, "S16"},
		{[]string{"doctor", "--config=x.yml", "--rubric=r.yml"}, "S16"},
	}
	for _, tt := range tests {
		code, _, stderr := run(tt.args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want 2", tt.args, code)
		}
		if want := "not built yet (slice " + tt.slice + ")"; !strings.Contains(stderr, want) {
			t.Errorf("%v: stderr %q, want %q", tt.args, stderr, want)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nope"},
		{"config"},
		{"config", "nope"},
		{"run", "--csv"},
		{"--dry-run", "run"},
		{"explain"},
		{"set-status", "a@example.com"},
		{"merge", "a", "b", "c"},
		{"retry", "a", "b"},
		{"run", "--config"},
		{"run", "--dry-run=yes"},
		{"config", "get"},
		{"config", "set-hosting"},
	} {
		code, _, stderr := run(args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if strings.Contains(stderr, "not built yet (slice") {
			t.Errorf("%v: a usage error must be reported before 'not built yet': %q", args, stderr)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"run", "--help"}} {
		code, stdout, _ := run(args...)
		if code != exitOK || !strings.Contains(stdout, "config set-hosting") {
			t.Errorf("%v: exit %d, stdout %q", args, code, stdout)
		}
	}
}

func TestConfigGetAndSetHosting(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "leadscore.yml")
	if err := os.WriteFile(cfg, []byte("# keep me\nversion: 1\nstore: { type: sqlite } # local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run("config", "get", "store.path", "--config", cfg)
	if code != exitOK || stdout != "/data/leadscore.db\n" {
		t.Fatalf("config get store.path: exit %d, out %q, err %q", code, stdout, stderr)
	}
	if code, _, _ = run("--config", cfg, "config", "get", "hosting.project"); code != exitFail {
		t.Fatalf("config get of an unset key: exit %d, want 1", code)
	}

	code, _, stderr = run("--config", cfg, "config", "set-hosting", "project=p1", "region=asia-south1")
	if code != exitOK {
		t.Fatalf("set-hosting: exit %d, err %q", code, stderr)
	}
	code, stdout, _ = run("--config", cfg, "config", "get", "hosting.project")
	if code != exitOK || stdout != "p1\n" {
		t.Fatalf("after set-hosting: exit %d, out %q", code, stdout)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "# keep me") || !strings.Contains(string(data), "# local") {
		t.Errorf("comments lost:\n%s", data)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode changed to %v", fi.Mode().Perm())
	}

	if code, _, _ = run("--config", cfg, "config", "set-hosting", "zone=x"); code != exitFail {
		t.Errorf("set-hosting with an unknown key: exit %d, want 1", code)
	}
	if code, _, _ = run("--config", filepath.Join(dir, "missing.yml"), "config", "get", "version"); code != exitFail {
		t.Errorf("config get with a missing file: exit %d, want 1", code)
	}
}

func TestEveryCommandIsReachable(t *testing.T) {
	for _, c := range commands {
		cmd, _ := match(c.path)
		if cmd != c {
			t.Errorf("command %q cannot be reached", c.name())
		}
		if c.run == nil && c.slice == "" {
			t.Errorf("command %q has neither a body nor an owning slice", c.name())
		}
	}
}
