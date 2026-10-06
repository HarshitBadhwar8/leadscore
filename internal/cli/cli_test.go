package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

// Every command whose slice has not landed says which slice builds it.
func TestUnbuiltCommandsSayWhichSlice(t *testing.T) {
	for _, c := range commands {
		if c.run != nil {
			continue
		}
		args := append([]string{}, c.path...)
		for i := 0; i < c.minArgs; i++ {
			args = append(args, "x")
		}
		code, _, stderr := run(args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if want := "leadscore " + c.name() + ": not built yet (slice " + c.slice + ")\n"; stderr != want {
			t.Errorf("%v: stderr %q, want %q", args, stderr, want)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		cmd   string
		pos   []string
		flags map[string]string
	}{
		{"global flags before", []string{"--config", "a.yml", "--rubric", "r.yml", "doctor"}, "doctor", nil,
			map[string]string{"config": "a.yml", "rubric": "r.yml"}},
		{"global flags after", []string{"doctor", "--config", "a.yml", "-rubric", "r.yml"}, "doctor", nil,
			map[string]string{"config": "a.yml", "rubric": "r.yml"}},
		{"equals form", []string{"config", "get", "--config=a.yml", "store.type"}, "config get", []string{"store.type"},
			map[string]string{"config": "a.yml"}},
		{"value starting with dash via equals", []string{"run", "--config=-odd.yml"}, "run", nil,
			map[string]string{"config": "-odd.yml"}},
		{"between command words", []string{"config", "--config", "a.yml", "set-hosting", "project=p"}, "config set-hosting",
			[]string{"project=p"}, map[string]string{"config": "a.yml"}},
		{"bool flag", []string{"run", "--dry-run"}, "run", nil, map[string]string{"dry-run": ""}},
		{"flags among positionals", []string{"retry", "a@example.com", "--lane", "warm"}, "retry", []string{"a@example.com"},
			map[string]string{"lane": "warm"}},
		{"every without a value", []string{"serve", "--every"}, "serve", nil, map[string]string{"every": ""}},
		{"every with a value", []string{"serve", "--every", "5m"}, "serve", nil, map[string]string{"every": "5m"}},
		{"every with equals", []string{"serve", "--every=1d"}, "serve", nil, map[string]string{"every": "1d"}},
		{"every then a flag", []string{"serve", "--every", "--config", "a.yml"}, "serve", nil,
			map[string]string{"every": "", "config": "a.yml"}},
		{"double dash ends flags", []string{"explain", "--", "--odd"}, "explain", []string{"--odd"}, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parse(tt.args)
			if err != nil {
				t.Fatalf("parse(%q): %v", tt.args, err)
			}
			if inv.cmd.name() != tt.cmd {
				t.Errorf("command = %q, want %q", inv.cmd.name(), tt.cmd)
			}
			if len(inv.args) != len(tt.pos) || (len(tt.pos) > 0 && !reflect.DeepEqual(inv.args, tt.pos)) {
				t.Errorf("args = %q, want %q", inv.args, tt.pos)
			}
			if !reflect.DeepEqual(inv.flags, tt.flags) {
				t.Errorf("flags = %v, want %v", inv.flags, tt.flags)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{}, "no command"},
		{[]string{"nope"}, "unknown command"},
		{[]string{"config"}, "incomplete command"},
		{[]string{"config", "nope"}, "unknown command"},
		{[]string{"--dry-run", "run"}, "before the command"},
		{[]string{"run", "--csv"}, "unknown flag"},
		{[]string{"run", "--dry-run=yes"}, "takes no value"},
		{[]string{"run", "--config"}, "needs a value"},
		{[]string{"run", "--config", "--dry-run"}, "needs a value"},
		{[]string{"run", "--config="}, "non-empty"},
		{[]string{"run", "--rubric", ""}, "non-empty"},
		{[]string{"serve", "--every", "soon"}, "not a duration"},
		{[]string{"serve", "--every=0s"}, "longer than zero"},
		{[]string{"serve", "extra"}, "usage"},
		{[]string{"explain"}, "usage"},
		{[]string{"merge", "a", "b", "c"}, "usage"},
		{[]string{"config", "set-hosting"}, "usage"},
	}
	for _, tt := range tests {
		_, err := parse(tt.args)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("parse(%q) err = %v, want it to contain %q", tt.args, err, tt.want)
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
