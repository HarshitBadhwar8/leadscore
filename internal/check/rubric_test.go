package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

func TestRubricCheck(t *testing.T) {
	var rc Check
	for _, c := range InRun() {
		if c.Name() == "rubric" {
			rc = c
		}
	}
	if rc == nil {
		t.Fatal("the rubric check must be registered and run inside every run")
	}
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("leadscore.yml", "version: 1\nstore: { type: sqlite }\n")
	cfg, err := config.Load(config.Options{ConfigPath: filepath.Join(dir, "leadscore.yml")})
	if err != nil {
		t.Fatal(err)
	}

	write("rubric.yml", "version: 1\nlanes: []\n")
	if got := rc.Run(context.Background(), Env{Config: cfg}); len(got) != 0 {
		t.Errorf("a good rubric: %v", got)
	}

	write("rubric.yml", "version: 1\nlanes:\n  - { kind: cold, push: apollo:sequence/x }\n")
	got := rc.Run(context.Background(), Env{Config: cfg})
	if len(got) != 1 || got[0].Key != "rubric_invalid:compile" || got[0].Warning ||
		!strings.Contains(got[0].Message, "rubric.yml: line 3: lanes[0]: every lane needs an id") {
		t.Errorf("a bad rubric: %+v", got)
	}

	if err := os.Remove(filepath.Join(dir, "rubric.yml")); err != nil {
		t.Fatal(err)
	}
	if got := rc.Run(context.Background(), Env{Config: cfg}); len(got) != 1 || !strings.Contains(got[0].Message, "reading rubric") {
		t.Errorf("a missing rubric: %+v", got)
	}
	if got := rc.Run(context.Background(), Env{}); got != nil {
		t.Errorf("no config: %v", got)
	}
}
