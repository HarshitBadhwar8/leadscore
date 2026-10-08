package leadscore_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// The reference's "Store tables" section names every table the store
// writes, with its columns in order, so the doc cannot drift from the code.
// A table's row is the one whose first cell starts with the table's name;
// the backticked names in its last cell that are columns of the table must
// be exactly its columns, in order.
func TestReferenceStoreTablesMatchModel(t *testing.T) {
	src, err := os.ReadFile("docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(src), "\n## Store tables\n")
	if ok {
		section, _, ok = strings.Cut(section, "\n## ")
	}
	if !ok {
		t.Fatal(`docs/reference.md has no "## Store tables" section followed by another section`)
	}
	ticked := regexp.MustCompile("`([^`]+)`")
	docName := map[string]string{model.EventsPrefix: "Events YYYY-MM", model.ExportPrefix: "Export <lane id>"}
	for _, def := range model.Tables {
		name := def.Name
		if d, ok := docName[name]; ok {
			name = d
		}
		var row string
		for _, line := range strings.Split(section, "\n") {
			if strings.HasPrefix(line, "| `"+name+"`") {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("docs/reference.md, Store tables: no row for %q", name)
			continue
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimSpace(row), "|"), "|")
		last := cells[len(cells)-1]
		var got []string
		for _, m := range ticked.FindAllStringSubmatch(last, -1) {
			if slices.Contains(def.Columns, m[1]) {
				got = append(got, m[1])
			}
		}
		if !slices.Equal(got, def.Columns) {
			t.Errorf("docs/reference.md, Store tables, %q: columns %v, want %v", name, got, def.Columns)
		}
	}
}

// The root godoc carries the types' field-level notes: the root aliases have no
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

// The reference's conformance-suite block names api.X, as the suites'
// signatures do, and says they are the root's types.
func TestReferenceSuiteBlockUsesAPINames(t *testing.T) {
	src, err := os.ReadFile("docs/reference.md")
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
			t.Errorf("docs/reference.md conformance block must contain %q", want)
		}
	}
}
