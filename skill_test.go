package leadscore_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/cli"
)

// c10Checks reads the check names from the contracts doc's section 10 table,
// in its order.
func c10Checks(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("docs/design/oss-outbound-engine-contracts.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "## 10. Doctor checks")
	end := strings.Index(s, "## 11. ")
	if start < 0 || end < start {
		t.Fatal("contracts section 10 not found")
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z-]+)`")
	var out []string
	for _, m := range row.FindAllStringSubmatch(s[start:end], -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("read only %d rows from section 10", len(out))
	}
	return out
}

// Every section 10 row has a troubleshooting entry in SKILL.md and a doctor
// test (an entry in internal/cli's doctorRows), and doctor prints the rows in
// the table's order. A row added to the contracts fails here until both
// exist.
func TestEveryDoctorRowHasATestAndASkillEntry(t *testing.T) {
	rows := c10Checks(t)
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
		t.Errorf("doctor's order %v differs from section 10's %v", cli.DoctorOrder, rows)
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
