// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package rules

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// lead is a lead at domain with the given fields, first seen at t0 plus n hours.
func lead(id, domain string, n int, fields map[string]string) api.LeadRef {
	return api.LeadRef{ID: api.LeadID(id), Domain: domain, Status: "new", SourcesSeen: 1,
		FirstSeenAt: t0.Add(time.Duration(n) * time.Hour), Fields: fields}
}

// values evaluates and returns each lead's derived values.
func values(t *testing.T, r *Rubric, in Input) map[api.LeadID]map[string]any {
	t.Helper()
	vs := r.Evaluate(in).Verdicts
	out := map[api.LeadID]map[string]any{}
	for id, v := range vs {
		out[id] = v.Values
	}
	return out
}

// derive1 is a rubric with one lead-level block x: 1 when cond holds, else 0.
func derive1(cond string) string {
	return "version: 1\nlanes: []\nsettings:\n  funding_order: [pre_seed, seed, series_a, series_b, series_c]\n  titles: [CTO, \"VP Engineering\"]\n  boss: CTO\n  bar: series_b\n" +
		"fields:\n  fleet: { type: number }\n  joined: { type: date }\n  active: { type: bool }\n  stage: { type: { ordered: funding_order } }\n" +
		"derive:\n  x:\n    - when: " + cond + "\n      then: 1\n    - else: 0\n"
}

// Text matching, types, and absent values.
func TestConditions(t *testing.T) {
	five := 5
	in := func(fields map[string]string, facts api.CompanyFacts) Input {
		l := lead("a", "a.example", 0, fields)
		l.Status = "replied_positive"
		facts.Employees = &five
		return Input{Leads: []api.LeadRef{l}, Companies: map[string]api.CompanyFacts{"a.example": facts}}
	}
	tests := []struct {
		name, cond string
		fields     map[string]string
		facts      api.CompanyFacts
		want       float64
	}{
		{"eq ignores case and spaces", "{ field: title, eq: cto }", map[string]string{"title": "  CTO "}, api.CompanyFacts{}, 1},
		{"eq differs", "{ field: title, eq: cfo }", map[string]string{"title": "CTO"}, api.CompanyFacts{}, 0},
		{"ne", "{ field: title, ne: cfo }", map[string]string{"title": "CTO"}, api.CompanyFacts{}, 1},
		{"ne on absent is false", "{ field: title, ne: cfo }", nil, api.CompanyFacts{}, 0},
		{"eq on absent is false", "{ field: title, eq: cfo }", nil, api.CompanyFacts{}, 0},
		{"blank is absent", "{ field: title, present: true }", map[string]string{"title": "  "}, api.CompanyFacts{}, 0},
		{"missing", "{ field: title, missing: true }", nil, api.CompanyFacts{}, 1},
		{"present", "{ field: title, present: true }", map[string]string{"title": "x"}, api.CompanyFacts{}, 1},
		{"in a setting list", "{ field: title, in: $titles }", map[string]string{"title": "vp engineering"}, api.CompanyFacts{}, 1},
		{"not_in", "{ field: title, not_in: [cfo, ceo] }", map[string]string{"title": "CTO"}, api.CompanyFacts{}, 1},
		{"not_in on absent is false", "{ field: title, not_in: [cfo] }", nil, api.CompanyFacts{}, 0},
		{"contains ignores case", "{ field: title, contains: ENGIN }", map[string]string{"title": "VP Engineering"}, api.CompanyFacts{}, 1},
		{"undeclared column is text", "{ field: preferredcarrier, eq: dhl }", map[string]string{"preferredcarrier": "DHL"}, api.CompanyFacts{}, 1},
		{"number lt", "{ field: fleet, lt: 10 }", map[string]string{"fleet": "9"}, api.CompanyFacts{}, 1},
		{"number gte", "{ field: fleet, gte: 10 }", map[string]string{"fleet": "9.5"}, api.CompanyFacts{}, 0},
		{"unparseable number is absent", "{ field: fleet, missing: true }", map[string]string{"fleet": "50-200"}, api.CompanyFacts{}, 1},
		{"number in", "{ field: fleet, in: [1, 2, 3] }", map[string]string{"fleet": "2"}, api.CompanyFacts{}, 1},
		{"date gt", "{ field: joined, gt: 2025-12-31 }", map[string]string{"joined": "2026-01-15"}, api.CompanyFacts{}, 1},
		{"date with time", "{ field: joined, lt: 2026-01-15 }", map[string]string{"joined": "2026-01-14T23:00:00Z"}, api.CompanyFacts{}, 1},
		{"bool yes", "{ field: active, eq: true }", map[string]string{"active": "Yes"}, api.CompanyFacts{}, 1},
		{"bool 0", "{ field: active, eq: false }", map[string]string{"active": "0"}, api.CompanyFacts{}, 1},
		{"built-in bool", "{ field: receiver_only, eq: false }", nil, api.CompanyFacts{}, 1},
		{"built-in number", "{ field: sources_seen, eq: 1 }", nil, api.CompanyFacts{}, 1},
		{"status", "{ field: status, eq: Replied_Positive }", nil, api.CompanyFacts{}, 1},
		{"status in", "{ field: status, in: [new, contacted] }", nil, api.CompanyFacts{}, 0},
		{"company number", "{ field: company.employees, lt: 20 }", nil, api.CompanyFacts{}, 1},
		{"company text", "{ field: company.region, eq: germany }", nil, api.CompanyFacts{Region: "Germany"}, 1},
		{"ordered gte matches loosely", "{ field: company.funding_stage, gte: series_b }", nil, api.CompanyFacts{FundingStage: "Series-C"}, 1},
		{"ordered lt", "{ field: company.funding_stage, lt: series_b }", nil, api.CompanyFacts{FundingStage: "seed"}, 1},
		{"ordered unknown sorts below all", "{ field: company.funding_stage, lt: pre_seed }", nil, api.CompanyFacts{FundingStage: "Angel"}, 1},
		{"ordered absent is false", "{ field: company.funding_stage, lt: series_b }", nil, api.CompanyFacts{}, 0},
		{"ordered eq", "{ field: company.funding_stage, eq: series_a }", nil, api.CompanyFacts{FundingStage: "Series A"}, 1},
		{"ordered in", "{ field: company.funding_stage, in: [series_a, series_b] }", nil, api.CompanyFacts{FundingStage: "SERIES B"}, 1},
		{"company extra fact", "{ field: company.industry, eq: retail }", nil, api.CompanyFacts{Extra: map[string]string{"industry": "Retail"}}, 1},
		{"all", "{ all: [ { field: title, present: true }, { field: fleet, gt: 1 } ] }", map[string]string{"title": "x", "fleet": "2"}, api.CompanyFacts{}, 1},
		{"any", "{ any: [ { field: title, present: true }, { field: fleet, gt: 1 } ] }", map[string]string{"fleet": "2"}, api.CompanyFacts{}, 1},
		{"not of absent is true", "{ not: { field: title, eq: cto } }", nil, api.CompanyFacts{}, 1},
		{"expr", `{ expr: "has(lead.fleet) && lead.fleet > 3 && company.employees == 5" }`, map[string]string{"fleet": "4"}, api.CompanyFacts{}, 1},
		{"expr settings", `{ expr: "settings.titles.size() == 2" }`, nil, api.CompanyFacts{}, 1},
		{"number ne", "{ field: fleet, ne: 5 }", map[string]string{"fleet": "4"}, api.CompanyFacts{}, 1},
		{"number ne equal", "{ field: fleet, ne: 5 }", map[string]string{"fleet": "5"}, api.CompanyFacts{}, 0},
		{"date ne", "{ field: joined, ne: 2026-01-01 }", map[string]string{"joined": "2026-01-02"}, api.CompanyFacts{}, 1},
		{"bool ne", "{ field: active, ne: true }", map[string]string{"active": "no"}, api.CompanyFacts{}, 1},
		{"number not_in", "{ field: fleet, not_in: [1, 2] }", map[string]string{"fleet": "3"}, api.CompanyFacts{}, 1},
		{"number not_in hit", "{ field: fleet, not_in: [1, 3] }", map[string]string{"fleet": "3"}, api.CompanyFacts{}, 0},
		{"bool not_in both", "{ field: active, not_in: [true, false] }", map[string]string{"active": "yes"}, api.CompanyFacts{}, 0},
		{"bool not_in other", "{ field: active, not_in: [false] }", map[string]string{"active": "yes"}, api.CompanyFacts{}, 1},
		{"sources_seen not_in", "{ field: sources_seen, not_in: [2, 3] }", nil, api.CompanyFacts{}, 1},
		{"number in empty", "{ field: fleet, in: [] }", map[string]string{"fleet": "3"}, api.CompanyFacts{}, 0},
		{"number not_in empty", "{ field: fleet, not_in: [] }", map[string]string{"fleet": "3"}, api.CompanyFacts{}, 1},
		{"text not_in empty", "{ field: title, not_in: [] }", map[string]string{"title": "x"}, api.CompanyFacts{}, 1},
		{"not_in empty on absent", "{ field: fleet, not_in: [] }", nil, api.CompanyFacts{}, 0},
		{"status contains", "{ field: status, contains: replied }", nil, api.CompanyFacts{}, 1},
		{"NFC text", "{ field: title, eq: \"Caf\u00e9\" }", map[string]string{"title": "Cafe\u0301"}, api.CompanyFacts{}, 1},
		{"ordered ignores unicode spaces", "{ field: company.funding_stage, eq: series_b }", nil, api.CompanyFacts{FundingStage: "Series\u00a0B"}, 1},
		{"single-value setting eq", "{ field: title, eq: $boss }", map[string]string{"title": "cto"}, api.CompanyFacts{}, 1},
		{"single-value setting gte", "{ field: company.funding_stage, gte: $bar }", nil, api.CompanyFacts{FundingStage: "Series C"}, 1},
		{"declared ordered", "{ field: stage, gte: series_a }", map[string]string{"stage": "Series B"}, api.CompanyFacts{}, 1},
		{"a date is midnight UTC", "{ field: joined, lt: 2026-01-15 }", map[string]string{"joined": "2026-01-15T00:00:01Z"}, api.CompanyFacts{}, 0},
		{"expr on a missing key is false", `{ expr: "lead.fleet > 3" }`, nil, api.CompanyFacts{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := mustCompile(t, derive1(tt.cond))
			got := values(t, r, in(tt.fields, tt.facts))["a"]["x"]
			if got != tt.want {
				t.Errorf("x = %v, want %v", got, tt.want)
			}
		})
	}
}

// A value that does not parse is absent and warned once per field; a raw
// expression that fails, or gives something other than true or false, is false
// and warned once. Warnings name fields and rules, never a lead's values.
func TestEvaluateWarnings(t *testing.T) {
	r := mustCompile(t, derive1(`{ any: [ { field: fleet, gt: 1 }, { expr: "lead.title == 'x'" }, { expr: "int(lead.email) > 1" }, { expr: "lead.full_name" } ] }`))
	a := lead("a", "", 0, map[string]string{"fleet": "anna.weber@kranlogistik.example", "joined": "Anna Weber"})
	a.Emails = []string{"anna.weber@kranlogistik.example"}
	a.FullName = "Anna Weber"
	a.LinkedInURLs = []string{"https://www.linkedin.com/in/example-anna"}
	b := lead("b", "", 1, map[string]string{"fleet": "many"})
	w := r.Evaluate(Input{Leads: []api.LeadRef{a, b}}).Warnings
	want := []string{
		"field fleet: a value is not a number, so it is treated as missing (reported once per run)",
		"field joined: a value is not a date, so it is treated as missing (reported once per run)",
		`condition "fleet > 1 or lead.title == 'x' or int(lead.email) > 1 or lead.full_name" failed (it reads a value the lead does not have; test it with has() first), so it counts as false`,
	}
	if !reflect.DeepEqual(w, want) {
		t.Errorf("warnings:\n%q\nwant\n%q", w, want)
	}
	// Each failing raw expression on its own, with the lead's values present.
	for _, expr := range []string{"int(lead.email) > 1", "lead.full_name", "lead.linkedin_url.size() > 1000000 || int(lead.linkedin_url) > 1"} {
		r := mustCompile(t, derive1(`{ expr: "`+expr+`" }`))
		ws := r.Evaluate(Input{Leads: []api.LeadRef{a}}).Warnings
		if !slices.ContainsFunc(ws, func(m string) bool { return strings.Contains(m, "counts as false") }) {
			t.Errorf("%s: no warning for the failing condition: %q", expr, ws)
		}
		for _, msg := range ws {
			for _, secret := range []string{"anna", "Anna", "kranlogistik"} {
				if strings.Contains(msg, secret) {
					t.Errorf("%s: warning %q leaks a lead's value %q", expr, msg, secret)
				}
			}
		}
	}
}

// Derive: the first match wins, then: null and no match give "no value", a
// later block reads an earlier one, and a derived name shadows its column.
func TestDerive(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
derive:
  band:
    - { when: { field: segment, eq: a }, then: null }
    - { when: { field: segment, present: true }, then: first }
    - { when: { field: segment, present: true }, then: second }
  later:
    - { when: { field: band, eq: first }, then: yes }
    - { when: { field: band, missing: true }, then: no }
  segment:
    - { when: { field: segment, eq: b }, then: renamed }
  after:
    - { when: { field: segment, eq: renamed }, then: true }
    - else: false
`)
	got := values(t, r, Input{Leads: []api.LeadRef{
		lead("a", "", 0, map[string]string{"segment": "a"}),
		lead("b", "", 0, map[string]string{"segment": "b"}),
		lead("c", "", 0, map[string]string{"segment": "c"}),
	}})
	want := map[api.LeadID]map[string]any{
		"a": {"band": nil, "later": "no", "segment": nil, "after": false},
		"b": {"band": "first", "later": "yes", "segment": "renamed", "after": true},
		"c": {"band": "first", "later": "yes", "segment": nil, "after": false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// Company blocks run once per company and every lead there gets the value; a
// lead block reads company values; detectors per subject.
func TestCompanyLevel(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
detectors:
  pricing: { kind: count_in_window, event: visit_pricing, window: 7d, min: 3, subject: company }
  demo: { kind: first_seen, event: visit_demo, within: 7d }
derive:
  tier:
    level: company
    rules:
      - { when: { detector: pricing }, then: 1 }
      - { when: { field: company.leads_seen, gte: 2 }, then: 2 }
      - else: 3
  hot:
    - { when: { all: [ { field: tier, eq: 1 }, { detector: demo } ] }, then: true }
    - { when: { expr: "company.tier == 2 && detector.demo" }, then: true }
    - else: false
`)
	in := Input{
		Leads: []api.LeadRef{
			lead("a1", "a.example", 0, nil), lead("a2", "a.example", 1, nil),
			lead("b1", "b.example", 0, nil), lead("b2", "b.example", 1, nil),
			lead("n", "", 0, nil),
		},
		LeadsSeen: map[string]int{"a.example": 2, "b.example": 2},
		Detectors: DetectorResults{
			Companies: map[string]map[string]bool{"a.example": {"pricing": true}},
			Leads:     map[api.LeadID]map[string]bool{"a2": {"demo": true}, "b1": {"demo": true}},
		},
	}
	got := values(t, r, in)
	want := map[api.LeadID]map[string]any{
		"a1": {"tier": 1.0, "hot": false},
		"a2": {"tier": 1.0, "hot": true},
		"b1": {"tier": 2.0, "hot": true},
		"b2": {"tier": 2.0, "hot": false},
		"n":  {"tier": nil, "hot": false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// Score: points rules, bands (highest threshold at or below, a fourth adds
// nothing), account half 0 with no company, the two halves summed.
func TestScore(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
derive:
  tier:
    level: company
    rules:
      - { when: { field: company.employees, gte: 20 }, then: 1 }
score:
  account:
    - { when: { field: tier, eq: 1 }, points: 40 }
    - { band: company.leads_seen, points: { 1: 0, 2: 10, 3: 20 } }
  contact:
    - { when: { field: warm_path, present: true }, points: 15 }
    - { band: sources_seen, points: { 1: 0, 2: 15, 3: 25 } }
    - { when: { field: title, eq: intern }, points: -5 }
`)
	emp := 50
	mk := func(id, domain string, sources int, fields map[string]string) api.LeadRef {
		l := lead(id, domain, 0, fields)
		l.SourcesSeen = sources
		return l
	}
	in := Input{
		Leads: []api.LeadRef{
			mk("a", "a.example", 1, nil),
			mk("b", "b.example", 4, map[string]string{"warm_path": "Priya"}),
			mk("c", "c.example", 2, map[string]string{"title": "Intern"}),
			mk("n", "", 3, nil),
		},
		Companies: map[string]api.CompanyFacts{"a.example": {Employees: &emp}, "b.example": {Employees: &emp}},
		LeadsSeen: map[string]int{"a.example": 1, "b.example": 4, "c.example": 2},
	}
	vs := r.Evaluate(in).Verdicts
	type sc struct{ account, contact float64 }
	got := map[api.LeadID]sc{}
	for id, v := range vs {
		got[id] = sc{v.AccountScore, v.ContactScore}
	}
	want := map[api.LeadID]sc{"a": {40, 0}, "b": {60, 40}, "c": {10, 10}, "n": {0, 25}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if rs := vs["n"].Reasons; !slices.Contains(rs, "no company domain") {
		t.Errorf("a lead with no company says so: %q", rs)
	}
	if rs := vs["b"].Reasons; !reflect.DeepEqual(rs, []string{
		"tier = 1 (rule 1: company.employees >= 20)",
		"+40 account: tier = 1",
		"+20 account: company.leads_seen is 4 (band 3)",
		"+15 contact: warm_path is present",
		"+25 contact: sources_seen is 4 (band 3)",
	}) {
		t.Errorf("reasons: %q", rs)
	}
	if rs := vs["c"].Reasons; !slices.Contains(rs, "-5 contact: title = intern") || !slices.Contains(rs, "tier: no value (no rule matched)") {
		t.Errorf("reasons: %q", rs)
	}
}

func TestBandPoints(t *testing.T) {
	bands := map[float64]float64{1: 0, 2: 15, 3: 25}
	for _, tt := range []struct {
		v      float64
		th, pt float64
		ok     bool
	}{{0, 0, 0, false}, {1, 1, 0, true}, {2, 2, 15, true}, {2.5, 2, 15, true}, {3, 3, 25, true}, {9, 3, 25, true}} {
		th, pt, ok := bandPoints(bands, tt.v)
		if th != tt.th || pt != tt.pt || ok != tt.ok {
			t.Errorf("bandPoints(%v) = %v, %v, %v", tt.v, th, pt, ok)
		}
	}
	if _, pt, ok := bandPoints(map[float64]float64{-1: 3}, -0.5); !ok || pt != 3 {
		t.Error("a negative threshold works")
	}
}

// A lead whose sources disagree on a conflicts field is blocked with the
// reason, and still gets a verdict.
func TestConflictsBlock(t *testing.T) {
	r := mustCompile(t, "version: 1\nlanes: []\nconflicts:\n  - { field: segment }\n  - { field: company.region }\n")
	a := lead("a", "", 0, nil)
	a.ConflictFields = []string{"segment", "title"}
	b := lead("b", "", 0, nil)
	b.ConflictFields = []string{"title"}
	c := lead("c", "", 0, nil)
	c.ConflictFields = []string{"company.region", "segment"}
	res := r.Evaluate(Input{Leads: []api.LeadRef{a, b, c}})
	vs, blocked := res.Verdicts, res.Blocked
	want := map[api.LeadID]string{"a": "sources disagree on segment", "c": "sources disagree on segment, company.region"}
	if !reflect.DeepEqual(blocked, want) {
		t.Errorf("blocked %v, want %v", blocked, want)
	}
	if len(vs) != 3 {
		t.Errorf("every lead gets a verdict: %v", vs)
	}
}

// Lanes match on derived values and status, highest priority first.
func TestLaneMatches(t *testing.T) {
	r := mustCompile(t, `version: 1
derive:
  tier:
    - { when: { field: title, present: true }, then: 1 }
lanes:
  - { id: export, kind: export, priority: 1, when: { field: sources_seen, gte: 1 }, push: export:all }
  - { id: cold, kind: cold, priority: 10, when: { all: [ { field: tier, eq: 1 }, { field: receiver_only, eq: false } ] }, push: apollo:sequence/a }
  - { id: warm, kind: non-cold, priority: 20, when: { field: status, eq: replied_positive }, push: hubspot:deals }
`)
	a := lead("a", "", 0, map[string]string{"title": "x"})
	a.Status = "replied_positive"
	b := lead("b", "", 0, map[string]string{"title": "x"})
	b.ReceiverOnly = true
	got := r.Evaluate(Input{Leads: []api.LeadRef{a, b}}).Lanes
	want := map[api.LeadID][]string{"a": {"warm", "cold", "export"}, "b": {"export"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Built-in lead fields come from LeadRef, which wins over a merged field.
func TestLeadRefFields(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
derive:
  ok:
    - when: { all: [ { field: email, eq: a@x.example }, { field: linkedin_url, contains: anna }, { field: full_name, eq: Anna }, { field: title, eq: CTO } ] }
      then: true
`)
	l := lead("a", "", 0, map[string]string{"email": "old@x.example", "title": "Engineer"})
	l.Emails = []string{"a@x.example", "b@x.example"}
	l.LinkedInURLs = []string{"https://linkedin.com/in/anna"}
	l.FullName, l.Title = "Anna", "CTO"
	if got := values(t, r, Input{Leads: []api.LeadRef{l}})["a"]["ok"]; got != true {
		t.Errorf("ok = %v", got)
	}
}
