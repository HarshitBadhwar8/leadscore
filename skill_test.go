package leadscore_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/cli"
)

// referenceChecks reads the check names from the "Doctor checks" table in
// docs/reference.md, in its order.
func referenceChecks(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	_, section, ok := strings.Cut(s, "\n## Doctor checks\n")
	if ok {
		section, _, ok = strings.Cut(section, "\n## ")
	}
	if !ok {
		t.Fatal(`docs/reference.md has no "## Doctor checks" section followed by another section`)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z-]+)`")
	var out []string
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("read only %d rows from the doctor checks table", len(out))
	}
	return out
}

// Every row of the reference's doctor checks table has a troubleshooting
// entry in SKILL.md and a doctor test (an entry in internal/cli's
// doctorRows), and doctor prints the rows in the table's order. A row added
// to the reference fails here until both exist.
func TestEveryDoctorRowHasATestAndASkillEntry(t *testing.T) {
	rows := referenceChecks(t)
	skill, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	tests, err := os.ReadFile("internal/cli/doctor_rows_test.go")
	if err != nil {
		t.Fatal(err)
	}
	troubleshooting := string(skill)[strings.Index(string(skill), "## Troubleshooting"):]
	for _, name := range rows {
		if !strings.Contains(troubleshooting, "\n### "+name+"\n") {
			t.Errorf("SKILL.md has no troubleshooting entry `### %s`", name)
		}
		if !strings.Contains(string(tests), "\t\""+name+"\": {func(t *testing.T) string {") {
			t.Errorf("internal/cli/doctor_rows_test.go has no doctorRows entry for %s", name)
		}
	}
	if !slices.Equal(rows, cli.DoctorOrder) {
		t.Errorf("doctor's order %v differs from docs/reference.md's %v", cli.DoctorOrder, rows)
	}
}

// SKILL.md carries the frontmatter skill loaders read: a name and a
// description.
func TestSkillFrontmatter(t *testing.T) {
	src, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	front, _, ok := strings.Cut(strings.TrimPrefix(string(src), "---\n"), "\n---\n")
	if !strings.HasPrefix(string(src), "---\n") || !ok {
		t.Fatal("SKILL.md must start with --- frontmatter")
	}
	if !regexp.MustCompile(`(?m)^name: leadscore$`).MatchString(front) ||
		!regexp.MustCompile(`(?m)^description: \S.{40,}$`).MatchString(front) {
		t.Errorf("frontmatter needs name and description:\n%s", front)
	}
	if !strings.Contains(string(src), "## Changing the rules (the rule-change loop)") {
		t.Error("SKILL.md must describe the rule-change loop")
	}
}
