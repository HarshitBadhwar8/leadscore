package leadscore_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The root godoc carries C1's field-level notes: the root aliases have no
// fields of their own in godoc, so the alias comment is all a reader sees.
func TestRootGodocCarriesFieldNotes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "leadscore.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range g.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				doc := ts.Doc
				if doc == nil {
					doc = g.Doc
				}
				docs[ts.Name.Name] = doc.Text()
			}
		}
	}
	want := map[string][]string{
		"Event":        {"Attrs[\"reject\"]", "employer domain", "company-only event"},
		"CompanyFacts": {"country", "latest_funding_at"},
		"LeadRef":      {"Fields holds merged fields", "CompanyDealID"},
		"RunResult":    {"Healthy", "Pushed"},
	}
	for name, phrases := range want {
		for _, p := range phrases {
			if !strings.Contains(strings.Join(strings.Fields(docs[name]), " "), p) {
				t.Errorf("godoc of %s must mention %q; got %q", name, p, docs[name])
			}
		}
	}
}

func TestRegistryCommentMatchesRoot(t *testing.T) {
	src, err := os.ReadFile("internal/api/registry.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "Registering the same type twice panics.") ||
		!strings.Contains(string(src), "panics on an empty type, a nil factory, or a type already") {
		t.Error("internal/api/registry.go must state every Register* panic case, as the root does")
	}
}

func TestContractsSuiteBlockUsesAPINames(t *testing.T) {
	src, err := os.ReadFile("docs/design/oss-outbound-engine-contracts.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		"func Run(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog))",
		"New    func(cfg api.Config) (api.Sink, error)",
		"same types as `leadscore.X`",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("contracts C1 conformance block must contain %q", want)
		}
	}
}
