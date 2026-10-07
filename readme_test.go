package leadscore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// section returns the README text from heading to the next heading of the
// same level.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	i := strings.Index(doc, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("README has no %q", heading)
	}
	rest := doc[i+len(heading)+2:]
	level := heading[:strings.Index(heading, " ")+1]
	if j := strings.Index(rest, "\n"+level); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// The README carries every duty the design gives it (RFC 6.11, 6.12, 6.13,
// 7; contracts sections 4, 5.1 and 9; S0's answers), so a later edit cannot
// drop one unnoticed.
func TestReadmeDuties(t *testing.T) {
	readme := readFile(t, "README.md")
	flat := strings.Join(strings.Fields(readme), " ")
	duties := map[string]string{
		"Google Cloud is for non-technical teams, said up front":           "**Google Cloud** | non-technical teams",
		"steps marked as agent or person":                                  "**[person]**",
		"no alerting in v1 (RFC 6.13)":                                     "There is no alerting in v1, on any path.",
		"filter on do_not_contact (RFC 6.11)":                              "Filter on `do_not_contact` before every send.",
		"export opt-outs come only from receiver, polling, Overrides (C4)": "Opt-outs reach the lists from the receiver, polling and `Overrides`",
		"export-row limit per run (C4)":                                    "A run adds at most `ingest_chunk_rows` (2,000) new rows across all lists",
		"laptop: poll or tunnel (RFC 6.12)":                                "A Cloudflare Tunnel",
		"laptop sleep warning (RFC 6.12)":                                  "A tunnel only works while the laptop is awake",
		"monthly cost (RFC 7)":                                             "### Run time and monthly cost",
		"event-rate advice (C4)":                                           "Above about 1,500 events a day, use SQLite",
		"Workspace exception (C9.1 step 6)":                                "ask your Workspace admin to allow sharing this file with the service accounts",
		"receiver secret warning (C5.1)":                                   "works like a password: anyone who has it can send fake events",
		"Apollo-only opt-out warning (S0, RFC 10)":                         "apollo-key:no_optout_flag",
		"owner edits protected tabs without warning (S5)":                  "edit the tabs leadscore protects (`Ranked`, `Health`, `Pushes`, ...) **with no warning**",
		"CSV formula injection (csvsafe)":                                  "never runs a formula a lead's data carried",
		"pre-release CLI install with the gcloud mount (C9.1 step 2)":      "-v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro",
		"plug-in stores get no CSV files (S16 decision)":                   "A plug-in store (for example Postgres, `docs/postgres-store.md`) keeps its lists as tables in that store.",
		"cold lanes claim leads with no sink (S13)":                        "A cold lane claims its leads even before its sink is set up",
		"receiver-silence needs public_url (S15)":                          "Without `receiver.public_url`, silence is not watched",
		"upgrade and roll back (C9.3)":                                     "Rolling back is the previous tag.",
	}
	for duty, phrase := range duties {
		if !strings.Contains(flat, strings.Join(strings.Fields(phrase), " ")) {
			t.Errorf("README lacks %s: %q", duty, phrase)
		}
	}
	if !strings.Contains(readFile(t, "setup/apollo/README.md"), "Keep the receiver secret private") {
		t.Error("setup/apollo/README.md must warn to keep the receiver secret private (C5.1)")
	}

	// The Google Cloud steps in C9.1's order.
	gcp := section(t, readme, "## Path 2: Google Cloud")
	order := []string{"setup/gcp.sh accounts", "setup/gcp.sh bucket", "leadscore setup sheet", "setup/gcp.sh secrets",
		"leadscore config push", "setup/gcp.sh deploy <image>", "setup/apollo/", "setup/gcp.sh schedule",
		"leadscore doctor", "leadscore run --dry-run", "pushes_enabled: true"}
	at := 0
	for _, step := range order {
		i := strings.Index(gcp[at:], step)
		if i < 0 {
			t.Errorf("Path 2 has %q out of C9.1's order (or not at all)", step)
			continue
		}
		at += i
	}
	// The Docker steps in C9.2's order.
	docker := section(t, readme, "## Path 3: Docker (a laptop or a server)")
	order = []string{"compose.yaml", ".env", "rules check", "Caddy", "docker compose up -d", "setup hubspot",
		"leadscore doctor", "ranked --csv", "run --dry-run", "pushes_enabled: true"}
	at = 0
	for _, step := range order {
		i := strings.Index(docker[at:], step)
		if i < 0 {
			t.Errorf("Path 3 has %q out of C9.2's order (or not at all)", step)
			continue
		}
		at += i
	}
}

// Every example leadscore.yml loads; the vendor ones carry the enrich and
// sinks blocks, and the CSV-only pair has no vendor at all and no cold lane.
func TestExampleFiles(t *testing.T) {
	load := func(name string) *config.Config {
		t.Helper()
		data := []byte(readFile(t, filepath.Join("examples", name)))
		c, err := config.Parse(data, "/config", func(string) string { return "" })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return c
	}
	for _, name := range []string{"leadscore.laptop.yml", "leadscore.server.yml", "leadscore.gcp.yml"} {
		c := load(name)
		if c.Enrich == nil || c.Enrich.Type != "apollo" {
			t.Errorf("%s has no enrich block", name)
		}
		if _, ok := c.Sinks["apollo"]; !ok {
			t.Errorf("%s has no sinks.apollo block", name)
		}
		if !strings.Contains(readFile(t, filepath.Join("examples", name)), "# hubspot: {") {
			t.Errorf("%s does not show the sinks.hubspot block", name)
		}
		if c.PushesEnabled {
			t.Errorf("%s turns pushes on", name)
		}
	}
	csvOnly := load("leadscore.csv-only.yml")
	if csvOnly.Enrich != nil || len(csvOnly.Sinks) != 0 || csvOnly.Store.Type != "sqlite" || csvOnly.PushesEnabled {
		t.Errorf("the CSV-only example uses a vendor or pushes: %+v", csvOnly)
	}
	r, err := rules.Compile([]byte(readFile(t, "examples/rubric.csv-only.yml")))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lanes()) == 0 {
		t.Error("the CSV-only rubric has no lanes")
	}
	for _, l := range r.Lanes() {
		if l.Kind != "export" {
			t.Errorf("the CSV-only rubric's lane %s is %s, not export", l.ID, l.Kind)
		}
	}
}
