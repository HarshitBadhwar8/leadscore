package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulesCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yml")
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(good, []byte("version: 1\nlanes: []\nderive:\n  segment:\n    - else: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("version: 1\nlanes:\n  - { id: a, kind: warm, push: export:x }\nlimits: { timezone: Nowhere }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run("rules", "check", good)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, good+": ok (version r-") ||
		!strings.Contains(stdout, "warning: line 4: derive.segment shadows the input field segment") {
		t.Errorf("good: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = run("rules", "check", bad)
	want := "leadscore rules check: 2 problems\n" +
		bad + ":3: lanes[0].kind: kind is cold, non-cold or export\n" +
		bad + `:4: limits.timezone: unknown timezone "Nowhere"; use an IANA name such as UTC or Asia/Kolkata` + "\n"
	if code != exitFail || stdout != "" || stderr != want {
		t.Errorf("bad: exit %d, stdout %q, stderr %q\nwant %q", code, stdout, stderr, want)
	}

	if code, _, stderr := run("rules", "check", filepath.Join(dir, "none.yml")); code != exitFail || !strings.Contains(stderr, "none.yml") {
		t.Errorf("missing file: exit %d, stderr %q", code, stderr)
	}
}

// The shipped example rubric compiles.
func TestRulesCheckExample(t *testing.T) {
	if code, stdout, stderr := run("rules", "check", "../../examples/rubric.yml"); code != exitOK {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
