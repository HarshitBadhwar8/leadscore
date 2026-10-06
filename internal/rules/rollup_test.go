package rules

import (
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Rollup scenarios: `any` is "strongest wins" (one lead is enough, and a later
// lead with nothing to say does not undo it), `first` is the first non-empty
// value by arrival, and a lead's values count only toward its own company.
// Each check is a company derive block reading the rollup, so the test sees
// exactly what a rubric sees.
const rollupBase = `version: 1
lanes: []
settings:
  legacy_tools: [spreadsheets, paper]
fields:
  warehouse_software: { type: text }
  industry: { type: text }
  fleet_size: { type: number }
  joined: { type: date }
company:
  legacy_seen: { any: { field: warehouse_software, in: $legacy_tools } }
  all_answered: { all: { field: warehouse_software, present: true } }
  answered: { count: { field: warehouse_software, present: true } }
  main_industry: { first: industry }
  biggest_fleet: { max: fleet_size }
  smallest_fleet: { min: fleet_size }
  earliest: { min: joined }
  latest: { max: joined }
derive:
  check:
    level: company
    rules:
      - when: `

// holds evaluates cond (a company-level condition) for each lead's company.
func holds(t *testing.T, cond string, leads ...api.LeadRef) map[string]bool {
	t.Helper()
	r := mustCompile(t, rollupBase+cond+"\n        then: true\n      - else: false\n")
	out := map[string]bool{}
	res := r.Evaluate(Input{Leads: leads})
	for _, l := range leads {
		out[l.Domain] = res.Verdicts[l.ID].Values["check"] == true
	}
	return out
}

func TestRollupAnyIsStrongestWins(t *testing.T) {
	leads := []api.LeadRef{
		lead("first", "kran.example", 0, map[string]string{"warehouse_software": "Spreadsheets"}),
		lead("quiet", "kran.example", 1, nil),
		lead("keyless", "solmar.example", 0, map[string]string{"warehouse_software": "paper "}),
		lead("other", "haven.example", 0, map[string]string{"warehouse_software": "SAP EWM"}),
	}
	got := holds(t, "{ field: company.legacy_seen, eq: true }", leads...)
	want := map[string]bool{"kran.example": true, "solmar.example": true, "haven.example": false}
	for d, w := range want {
		if got[d] != w {
			t.Errorf("%s: legacy_seen %v, want %v", d, got[d], w)
		}
	}
	if got := holds(t, "{ all: [ { field: company.all_answered, eq: false }, { field: company.answered, eq: 1 } ] }", leads...); !got["kran.example"] || got["solmar.example"] {
		t.Errorf("all/count: %v", got)
	}
}

func TestRollupFirstIsFirstNonEmptyByArrival(t *testing.T) {
	// The earliest lead says "zeta", a later one "alpha": first by arrival
	// gives zeta where a lexical pick would give alpha. Input order does not
	// matter, an earlier blank is skipped, and disagreement keeps the first.
	got := holds(t, "{ field: company.main_industry, eq: zeta }",
		lead("late", "order.example", 2, map[string]string{"industry": "alpha"}),
		lead("blank", "order.example", 0, map[string]string{"industry": " "}),
		lead("early", "order.example", 1, map[string]string{"industry": "zeta"}),
	)
	if !got["order.example"] {
		t.Error("first must be the first non-empty value by arrival")
	}
}

func TestRollupLandsOnTheLeadsOwnCompany(t *testing.T) {
	got := holds(t, "{ any: [ { field: company.legacy_seen, eq: true }, { field: company.main_industry, present: true } ] }",
		lead("a", "own.example", 0, nil),
		lead("b", "other.example", 0, map[string]string{"warehouse_software": "paper", "industry": "Haulage"}),
	)
	if got["own.example"] || !got["other.example"] {
		t.Errorf("evidence from another company leaked: %v", got)
	}
}

func TestRollupMaxMin(t *testing.T) {
	leads := []api.LeadRef{
		lead("a", "f.example", 0, map[string]string{"fleet_size": "12", "joined": "2026-02-01"}),
		lead("b", "f.example", 1, map[string]string{"fleet_size": "40", "joined": "2025-11-30"}),
		lead("c", "f.example", 2, map[string]string{"fleet_size": "unknown"}),
		lead("d", "e.example", 0, nil),
	}
	got := holds(t, `{ all: [ { field: company.biggest_fleet, eq: 40 }, { field: company.smallest_fleet, eq: 12 },
                { field: company.earliest, eq: 2025-11-30 }, { field: company.latest, eq: 2026-02-01 } ] }`, leads...)
	if !got["f.example"] {
		t.Error("max/min over numbers and dates")
	}
	if got := holds(t, "{ field: company.biggest_fleet, missing: true }", leads...); !got["e.example"] || got["f.example"] {
		t.Errorf("no value at any lead gives no value: %v", got)
	}
}
