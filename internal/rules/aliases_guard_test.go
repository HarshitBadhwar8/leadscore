package rules

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The built-in alias table names built-in fields only. A team's own fields
// live in its private rubric, so none of them may be the target of a built-in
// spelling: that would mean the table had picked up a private field. The
// check reads the rubric in LEADSCORE_PARITY_DIR (see parity_test.go) and is
// skipped when that is not set. It names no fields itself.
func TestBuiltinAliasesNameNoPrivateField(t *testing.T) {
	dir := os.Getenv("LEADSCORE_PARITY_DIR")
	if dir == "" {
		t.Skip("LEADSCORE_PARITY_DIR is not set")
	}
	src, err := os.ReadFile(filepath.Join(dir, "rubric.yml"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compile(src)
	if err != nil {
		// Only a count: the error text quotes the private rubric.
		var errs LoadErrors
		if errors.As(err, &errs) {
			t.Fatalf("rubric.yml does not compile (%d errors)", len(errs))
		}
		t.Fatal("rubric.yml does not compile")
	}
	targets := map[string]bool{}
	for _, field := range api.BuiltinAliases() {
		targets[field] = true
	}
	declared := 0
	for prefix, table := range map[string]map[string]*fieldDef{"": r.leadFields, "company.": r.companyFields} {
		for name, def := range table {
			if def.builtin {
				continue
			}
			declared++
			// The field's name stays out of the message, so a failure in a
			// shared log does not leak it.
			if targets[prefix+name] {
				t.Errorf("a field the private rubric declares is a built-in alias target; drop its spellings from the built-in table")
			}
		}
	}
	t.Logf("checked %d declared fields", declared)
}
