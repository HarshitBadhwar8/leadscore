package rules

import (
	"reflect"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The rollup scenarios are core's account-observation tests
// (dao/account_observations_test.go, aggregateAccountObservations in
// dao/lead_repo.go): the tool signal is "strongest wins" (any), segment and
// trigger note are "first non-empty wins" by arrival (first), and evidence
// lands only on the lead's own company.
const rollupRubric = `version: 1
lanes: []
settings:
  ai_tools: [cursor, copilot]
fields:
  fleet: { type: number }
  joined: { type: date }
company:
  tool_seen: { any: { field: tool, in: $ai_tools } }
  all_tooled: { all: { field: tool, present: true } }
  tooled: { count: { field: tool, present: true } }
  segment: { first: segment }
  trigger: { first: trigger_note }
  biggest: { max: fleet }
  smallest: { min: fleet }
  earliest: { min: joined }
  latest: { max: joined }
derive:
  out:
    level: company
    rules:
      - else: done
`

// rollups evaluates and returns each company's values, rollups included.
func rollups(t *testing.T, leads ...api.LeadRef) map[string]map[string]any {
	t.Helper()
	return mustCompile(t, rollupRubric).run(Input{Leads: leads}, false).companies
}

func TestRollupAnyIsStrongestWins(t *testing.T) {
	// One lead naming a tool makes the company yes; a later quiet lead does
	// not undo it.
	got := rollups(t,
		lead("first", "strong.example", 0, map[string]string{"tool": "Cursor"}),
		lead("quiet", "strong.example", 1, nil),
	)["strong.example"]
	if got["tool_seen"] != true || got["all_tooled"] != false || got["tooled"] != 1.0 {
		t.Errorf("got %v", got)
	}
	// A keyless lead (no email, no LinkedIn) counts like any other.
	keyless := lead("keyless", "keyless.example", 0, map[string]string{"tool": "copilot "})
	if got := rollups(t, keyless)["keyless.example"]; got["tool_seen"] != true || got["all_tooled"] != true {
		t.Errorf("keyless lead: %v", got)
	}
	// An unrecognised tool is not evidence.
	if got := rollups(t, lead("x", "typo.example", 0, map[string]string{"tool": "n/a"}))["typo.example"]; got["tool_seen"] != false {
		t.Errorf("unrecognised tool: %v", got)
	}
}

func TestRollupFirstIsFirstNonEmptyByArrival(t *testing.T) {
	// The earliest lead carries "zeta", a later one "alpha": first-by-arrival
	// gives zeta, where a lexical pick would give alpha. Input order must not
	// matter, and an earlier lead with no value is skipped.
	got := rollups(t,
		lead("late", "order.example", 2, map[string]string{"segment": "alpha", "trigger_note": "raised"}),
		lead("blank", "order.example", 0, map[string]string{"segment": " "}),
		lead("early", "order.example", 1, map[string]string{"segment": "zeta"}),
	)["order.example"]
	if got["segment"] != "zeta" || got["trigger"] != "raised" {
		t.Errorf("got %v", got)
	}
	// Disagreement is kept as the first value, not resolved or blanked.
	if got := rollups(t,
		lead("one", "split.example", 0, map[string]string{"segment": "SEGMENT_A"}),
		lead("two", "split.example", 1, map[string]string{"segment": "SEGMENT_B"}),
	)["split.example"]; got["segment"] != "SEGMENT_A" {
		t.Errorf("got %v", got)
	}
}

func TestRollupLandsOnTheLeadsOwnCompany(t *testing.T) {
	got := rollups(t,
		lead("a", "own.example", 0, nil),
		lead("b", "other.example", 0, map[string]string{"tool": "cursor", "segment": "x"}),
	)
	if got["own.example"]["tool_seen"] != false || got["own.example"]["segment"] != nil {
		t.Errorf("evidence from another company leaked: %v", got["own.example"])
	}
	if got["other.example"]["tool_seen"] != true {
		t.Errorf("got %v", got["other.example"])
	}
}

func TestRollupMaxMin(t *testing.T) {
	got := rollups(t,
		lead("a", "f.example", 0, map[string]string{"fleet": "12", "joined": "2026-02-01"}),
		lead("b", "f.example", 1, map[string]string{"fleet": "40", "joined": "2025-11-30"}),
		lead("c", "f.example", 2, map[string]string{"fleet": "unknown"}),
	)["f.example"]
	d := func(s string) time.Time { v, _ := parseDate(s); return v }
	want := map[string]any{
		"domain": "f.example", "out": "done", "tool_seen": false, "all_tooled": false, "tooled": 0.0,
		"biggest": 40.0, "smallest": 12.0, "earliest": d("2025-11-30"), "latest": d("2026-02-01"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// No value at any lead: the rollup has no value.
	if got := rollups(t, lead("a", "e.example", 0, nil))["e.example"]; got["biggest"] != nil {
		t.Errorf("got %v", got)
	}
}

// Rollups are what company conditions read, through the full evaluation.
func TestRollupsFeedCompanyBlocks(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
settings:
  ai_tools: [cursor]
company:
  tool_seen: { any: { field: tool, in: $ai_tools } }
  segment: { first: segment }
derive:
  fit_signal:
    level: company
    rules:
      - { when: { field: company.tool_seen, eq: true }, then: yes }
      - { when: { field: company.segment, present: true }, then: yes }
`)
	got := values(t, r, Input{Leads: []api.LeadRef{
		lead("a", "a.example", 0, nil), lead("b", "a.example", 1, map[string]string{"tool": "Cursor"}),
		lead("c", "c.example", 0, map[string]string{"segment": "Migrant"}),
		lead("d", "d.example", 0, nil),
	}})
	for id, want := range map[api.LeadID]any{"a": "yes", "b": "yes", "c": "yes", "d": nil} {
		if got[id]["fit_signal"] != want {
			t.Errorf("%s: fit_signal %v, want %v", id, got[id]["fit_signal"], want)
		}
	}
}
