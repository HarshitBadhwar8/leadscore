package cli

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
)

// runInstall is a folder with leadscore.yml on SQLite, a CSV source and a
// small rubric with an export lane.
func runInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"leadscore.yml": "version: 1\nstore: { type: sqlite, path: store.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\n",
		"rubric.yml": "version: 1\nderive:\n  tier:\n    - { when: { field: title, contains: head }, then: 1 }\n    - else: 2\n" +
			"score:\n  contact:\n    - { when: { field: title, contains: head }, points: 5 }\n" +
			"lanes:\n  - { id: list, kind: export, when: { field: tier, lte: 2 }, push: \"export:list\" }\n",
		"leads.csv": "Email,Name,Title\nana@acme.example,Ana A,Head of Ops\nbo@acme.example,Bo B,Clerk\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "leadscore.yml")
}

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunStatusRankedExplain(t *testing.T) {
	cfg := runInstall(t)

	code, out, errOut := cli("status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "no run has finished yet") {
		t.Fatalf("status before any run: %d %q %q", code, out, errOut)
	}
	code, out, errOut = cli("run", "--dry-run", "--config", cfg)
	if code != 0 || !strings.Contains(out, "dry run:") || !strings.Contains(out, "totals: 2 lead(s) scored: 2 new") {
		t.Fatalf("dry run: %d %q %q", code, out, errOut)
	}
	if code, out, _ := cli("ranked", "--config", cfg); code != 0 || !strings.Contains(out, "Ranked is empty") {
		t.Errorf("a dry run writes nothing: %d %q", code, out)
	}

	code, out, errOut = cli("run", "--config", cfg)
	if code != 0 || !strings.Contains(out, ": healthy; 2 lead(s) scored") {
		t.Fatalf("run: %d %q %q", code, out, errOut)
	}

	code, out, _ = cli("status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "last_result      healthy") || !strings.Contains(out, "no open problems") {
		t.Errorf("status: %q", out)
	}

	code, out, _ = cli("ranked", "--config", cfg)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 || !strings.HasPrefix(lines[0], "lead_id") || !strings.Contains(lines[1], "ana@acme.example") {
		t.Errorf("ranked (highest score first): %q", out)
	}
	if !strings.Contains(lines[0], "company_domain  tier  account_score") {
		t.Errorf("derived columns go after company_domain: %q", lines[0])
	}

	code, out, _ = cli("ranked", "--csv", "--config", cfg)
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if code != 0 || err != nil || len(recs) != 3 || strings.Join(recs[0], ",") !=
		"lead_id,email,linkedin_url,full_name,company_domain,tier,account_score,contact_score,score,status,lane,reasons,rubric_version" {
		t.Errorf("ranked --csv: %v %q", err, out)
	}

	code, out, _ = cli("explain", "ana@acme.example", "--config", cfg)
	for _, want := range []string{"email: ana@acme.example", "lane: list", "tier: 1", "score: 5 (account 0, contact 5)", "+5 contact: title contains head"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("explain lacks %q: %q", want, out)
		}
	}
	if code, _, errOut := cli("explain", "nobody@acme.example", "--config", cfg); code != 1 || !strings.Contains(errOut, "no lead matches") {
		t.Errorf("explain an unknown person: %d %q", code, errOut)
	}
}

// An unhealthy run exits 1, so a hosted job execution shows the failure.
func TestRunExitsOneWhenUnhealthy(t *testing.T) {
	cfg := runInstall(t)
	rubric := filepath.Join(filepath.Dir(cfg), "rubric.yml")
	if err := os.WriteFile(rubric, []byte("version: 1\nlanes:\n  - { id: seq, kind: cold, when: { field: receiver_only, eq: false }, push: \"nosuchsink:x\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := cli("run", "--config", cfg); code != 1 || !strings.Contains(out, "lane_sink_unregistered:seq") {
		t.Errorf("got %d %q", code, out)
	}
}
