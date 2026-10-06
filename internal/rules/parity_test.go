package rules

import (
	"os"
	"path/filepath"
	"testing"
)

// A private rubric kept outside the repo, in LEADSCORE_PARITY_DIR, must
// compile. S3 adds the parity run over that directory's cases.
func TestPrivateRubricCompiles(t *testing.T) {
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
		t.Fatalf("rubric.yml:\n%v", err)
	}
	for _, w := range r.Warnings() {
		t.Logf("warning: %v", w)
	}
	t.Logf("compiled, version %s", r.Version())
}
