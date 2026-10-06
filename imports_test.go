package leadscore_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/HarshitBadhwar8/leadscore"

// Inside the module only cmd/ and _test files may import the root package.
// Everything else (internal/*, adapters/*, storetest, sinktest) uses
// internal/api, whose names the root aliases; a root import from there would
// be an import cycle as soon as the root reaches that package.
func TestOnlyCmdAndTestsImportTheRoot(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".github", ".claude", "docs", "testdata", "cmd":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == "." {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == modulePath {
				t.Errorf("%s imports the root package; use internal/api instead", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
