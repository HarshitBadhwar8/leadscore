package rules

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// ne and not_in on a derived number.
func TestNeOnDerived(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
derive:
  tier:
    - { when: { field: title, present: true }, then: 1 }
    - else: 3
  away:
    - { when: { all: [ { field: tier, ne: 1 }, { field: tier, not_in: [1, 2] } ] }, then: true }
    - else: false
`)
	got := values(t, r, Input{Leads: []api.LeadRef{lead("a", "", 0, map[string]string{"title": "x"}), lead("b", "", 0, nil)}})
	if got["a"]["away"] != false || got["b"]["away"] != true {
		t.Errorf("got %v", got)
	}
}

// A lane whose expression fails is reported, not silently skipped.
func TestLaneExprWarning(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes:
  - { id: x, kind: export, when: { expr: "lead.fleet > 3" }, push: export:x }
`)
	res := r.Evaluate(Input{Leads: []api.LeadRef{lead("a", "", 0, nil)}})
	if len(res.Lanes["a"]) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `condition "lead.fleet > 3" failed`) {
		t.Errorf("lanes %v, warnings %q", res.Lanes, res.Warnings)
	}
}

// A cancelled context stops the evaluation with its error.
func TestEvaluateContextCancelled(t *testing.T) {
	r := mustCompile(t, derive1("{ field: title, present: true }"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.EvaluateContext(ctx, Input{Leads: []api.LeadRef{lead("a", "", 0, nil)}}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// Deep YAML aliases are refused at once, not expanded.
func TestAliasBombIsRefusedQuickly(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nlanes: []\nsettings:\n  a0: [x, x]\n")
	for i := 1; i <= 40; i++ {
		b.WriteString("  a" + strconv.Itoa(i) + ": &a" + strconv.Itoa(i) + " [*a" + strconv.Itoa(i-1) + ", *a" + strconv.Itoa(i-1) + "]\n")
	}
	src := strings.Replace(b.String(), "a0: [x, x]", "a0: &a0 [x, x]", 1)
	start := time.Now()
	_, err := Compile([]byte(src))
	var errs LoadErrors
	if !errors.As(err, &errs) || errs[0].Line != 5 || !strings.Contains(errs[0].Msg, "YAML aliases") {
		t.Errorf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// A company block named like a company field shadows it, with a warning.
func TestCompanyDeriveShadowsAField(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
derive:
  region:
    level: company
    rules:
      - { when: { field: company.region, in: [germany, austria] }, then: dach }
      - else: elsewhere
  home:
    - { when: { field: company.region, eq: dach }, then: true }
    - else: false
`)
	if w := r.Warnings(); len(w) != 1 || w[0].Field != "derive.region" || !strings.Contains(w[0].Msg, "shadows the company field region") {
		t.Errorf("warnings %v", r.Warnings())
	}
	got := values(t, r, Input{
		Leads:     []api.LeadRef{lead("a", "a.example", 0, nil)},
		Companies: map[string]api.CompanyFacts{"a.example": {Region: "Germany"}},
	})
	if got["a"]["region"] != "dach" || got["a"]["home"] != true {
		t.Errorf("got %v", got)
	}
}

// A cold lane whose when does not require receiver_only false is warned about.
func TestColdLaneReceiverOnlyWarning(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes:
  - { id: a, kind: cold, when: { field: status, eq: new }, push: apollo:sequence/a }
  - { id: b, kind: cold, when: { all: [ { field: status, eq: new }, { field: receiver_only, eq: false } ] }, push: apollo:sequence/b }
  - { id: c, kind: cold, when: { field: receiver_only, ne: true }, push: apollo:sequence/c }
  - { id: d, kind: export, when: { field: status, eq: new }, push: export:d }
`)
	w := r.Warnings()
	if len(w) != 1 || w[0].Field != "lanes[0].when" || !strings.Contains(w[0].Msg, "cold lane a does not require receiver_only to be false") {
		t.Errorf("warnings %v", w)
	}
}

// Declaring a field under a built-in header spelling is allowed, with a warning.
func TestDeclaredFieldOnABuiltinSpelling(t *testing.T) {
	r := mustCompile(t, rubric("fields:\n  stage: { type: text }\n"))
	if w := r.Warnings(); len(w) != 1 || !strings.Contains(w[0].Msg, `"stage" usually names company.funding_stage`) {
		t.Errorf("warnings %v", w)
	}
	if r.Aliases()["stage"] != "stage" {
		t.Errorf("aliases %v", r.Aliases())
	}
}

// In Extra, an exact company.<name> key wins over <name>, whatever the map order.
func TestExtraPrecedence(t *testing.T) {
	r := mustCompile(t, derive1("{ field: company.industry, eq: exact }"))
	for i := 0; i < 20; i++ {
		got := values(t, r, Input{
			Leads:     []api.LeadRef{lead("a", "a.example", 0, nil)},
			Companies: map[string]api.CompanyFacts{"a.example": {Extra: map[string]string{"industry": "bare", "company.industry": "exact"}}},
		})
		if got["a"]["x"] != 1.0 {
			t.Fatalf("run %d: the bare key won", i)
		}
	}
}

// Detector event kinds may end in * as a prefix match.
func TestDetectorEventPrefix(t *testing.T) {
	r := mustCompile(t, rubric("detectors:\n  d: { kind: count_in_window, event: \"visit_*\", window: 7d, min: 1 }\n"))
	if got := r.Detectors(); !reflect.DeepEqual(got[0].Event, "visit_*") {
		t.Errorf("got %v", got)
	}
}

// Company facts that do not parse warn in a fixed order (sorted by fact
// name), whatever order the map gives them.
func TestCompanyFactWarningsAreOrdered(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
fields:
  alpha: { type: number, level: company }
  beta: { type: number, level: company }
  gamma: { type: date, level: company }
derive:
  x:
    level: company
    rules:
      - { when: { all: [ { field: company.alpha, present: true }, { field: company.beta, present: true }, { field: company.gamma, present: true } ] }, then: 1 }
`)
	in := Input{
		Leads:     []api.LeadRef{lead("a", "d.example", 0, nil)},
		Companies: map[string]api.CompanyFacts{"d.example": {Domain: "d.example", Extra: map[string]string{"gamma": "soon", "beta": "many", "alpha": "lots"}}},
	}
	want := []string{
		"field alpha: a value is not a number, so it is treated as missing (reported once per run)",
		"field beta: a value is not a number, so it is treated as missing (reported once per run)",
		"field gamma: a value is not a date, so it is treated as missing (reported once per run)",
	}
	if w := r.Evaluate(in).Warnings; !reflect.DeepEqual(w, want) {
		t.Errorf("warnings:\n%q\nwant\n%q", w, want)
	}
}
