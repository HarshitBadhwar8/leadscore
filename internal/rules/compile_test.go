package rules

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rubric wraps body in the two required keys.
func rubric(body string) string {
	return "version: 1\nlanes: []\n" + body
}

func mustCompile(t *testing.T, src string) *Rubric {
	t.Helper()
	r, err := Compile([]byte(src))
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, src)
	}
	return r
}

// Every load error names the line and the field.
func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, src string
		line      int
		field     string
		msg       string
	}{
		{"not yaml", "version: [1", 0, "", "not valid YAML"},
		{"not a mapping", "- 1\n", 1, "", "a rubric is a mapping"},
		{"empty", "", 0, "", "a rubric is a mapping"},
		{"no version", "lanes: []\n", 1, "", "version is required"},
		{"wrong version", "version: 2\nlanes: []\n", 1, "version", "the only rubric version is 1"},
		{"no lanes", "version: 1\n", 1, "", "lanes is required"},
		{"unknown top key", rubric("scores: {}\n"), 3, "scores", "unknown key"},

		{"bad field type", rubric("fields:\n  x: { type: integer }\n"), 4, "fields.x.type", "type is text, number"},
		{"bad field level", rubric("fields:\n  x: { level: account }\n"), 4, "fields.x.level", "level is lead or company"},
		{"unknown field key", rubric("fields:\n  x: { kind: text }\n"), 4, "fields.x.kind", "unknown key"},
		{"bad field name", rubric("fields:\n  Job Title: { type: text }\n"), 4, "fields.Job Title", "lowercase letters"},
		{"builtin type mismatch", rubric("fields:\n  employees: { type: text, level: company }\n"), 4, "fields.employees", "built-in field of type number"},
		{"builtin at the wrong level", rubric("fields:\n  segment: { level: company }\n"), 4, "fields.segment", "built-in lead field"},
		{"alias clash", rubric("fields:\n  a: { aliases: [Tool] }\n  b: { aliases: [tool] }\n"), 5, "fields.b", `header spelling "tool" (squashed "tool") already names a`},
		{"ordered needs a list", rubric("fields:\n  stage: { type: { ordered: nope } }\n"), 4, "fields.stage.type.ordered", "settings.nope must be declared as a list"},

		{"setting not a list or value", rubric("settings:\n  s: { a: 1 }\n"), 4, "settings.s", "a list or a single value"},
		{"undeclared setting", rubric("derive:\n  x:\n    - { when: { field: title, in: $titles }, then: 1 }\n"), 5, "derive.x[0].when.in", "setting $titles is not declared"},
		{"list setting used as a value", rubric("settings:\n  s: [a]\nderive:\n  x:\n    - { when: { field: title, eq: $s }, then: 1 }\n"), 7, "derive.x[0].when.eq", "setting $s is a list"},
		{"scalar setting used as a list", rubric("settings:\n  s: a\nderive:\n  x:\n    - { when: { field: title, in: $s }, then: 1 }\n"), 7, "derive.x[0].when.in", "single value, not a list"},

		{"two forms", rubric("derive:\n  x:\n    - { when: { all: [], any: [] }, then: 1 }\n"), 5, "derive.x[0].when", "exactly one form"},
		{"field with two operators", rubric("derive:\n  x:\n    - { when: { field: title, eq: a, ne: b }, then: 1 }\n"), 5, "derive.x[0].when", "one operator"},
		{"unknown operator", rubric("derive:\n  x:\n    - { when: { field: title, like: a }, then: 1 }\n"), 5, "derive.x[0].when.like", "unknown operator"},
		{"unknown form", rubric("derive:\n  x:\n    - { when: { either: [] }, then: 1 }\n"), 5, "derive.x[0].when", "unknown condition form"},
		{"empty all", rubric("derive:\n  x:\n    - { when: { all: [] }, then: 1 }\n"), 5, "derive.x[0].when.all", "non-empty list"},
		{"condition not a mapping", rubric("derive:\n  x:\n    - { when: title, then: 1 }\n"), 5, "derive.x[0].when", "must be a mapping"},
		{"present false", rubric("derive:\n  x:\n    - { when: { field: title, present: false }, then: 1 }\n"), 5, "derive.x[0].when.present", "write present: true"},
		{"lt on text", rubric("derive:\n  x:\n    - { when: { field: title, lt: 3 }, then: 1 }\n"), 5, "derive.x[0].when.lt", "compares numbers, dates and ordered fields"},
		{"contains on a number", rubric("derive:\n  x:\n    - { when: { field: company.employees, contains: 3 }, then: 1 }\n"), 5, "derive.x[0].when.contains", "contains matches text"},
		{"number value mismatch", rubric("derive:\n  x:\n    - { when: { field: company.employees, gt: lots }, then: 1 }\n"), 5, "derive.x[0].when.gt", `"lots" is not a number`},
		{"date value mismatch", rubric("fields:\n  d: { type: date }\nderive:\n  x:\n    - { when: { field: d, gt: soon }, then: 1 }\n"), 7, "derive.x[0].when.gt", "not an ISO 8601 date"},
		{"bool value mismatch", rubric("derive:\n  x:\n    - { when: { field: receiver_only, eq: maybe }, then: 1 }\n"), 5, "derive.x[0].when.eq", "not true/false"},
		{"ordered value not in the list", rubric("settings:\n  funding_order: [seed, series_a]\nderive:\n  x:\n    - { when: { field: company.funding_stage, gte: series_z }, then: 1 }\n"), 7, "derive.x[0].when.gte", `"series_z" is not in settings.funding_order`},
		{"ordered comparison without its list", rubric("derive:\n  x:\n    - { when: { field: company.funding_stage, gte: seed }, then: 1 }\n"), 5, "derive.x[0].when.gte", "need settings.funding_order"},
		{"unknown status", rubric("derive:\n  x:\n    - { when: { field: status, eq: replied_positiv }, then: 1 }\n"), 5, "derive.x[0].when.eq", "is not a status"},
		{"unknown field", rubric("derive:\n  x:\n    - { when: { field: Job Title, present: true }, then: 1 }\n"), 5, "derive.x[0].when.field", "unknown field"},
		{"unknown company field", rubric("derive:\n  x:\n    - { when: { field: company.Head Count, present: true }, then: 1 }\n"), 5, "derive.x[0].when.field", "unknown company field"},
		{"company field without prefix", rubric("fields:\n  industry: { level: company }\nderive:\n  x:\n    - { when: { field: industry, present: true }, then: 1 }\n"), 7, "derive.x[0].when.field", "write company.industry"},
		{"undeclared detector", rubric("derive:\n  x:\n    - { when: { detector: pricing }, then: 1 }\n"), 5, "derive.x[0].when.detector", `detector "pricing" is not declared`},
		{"raw expr does not compile", rubric("derive:\n  x:\n    - { when: { expr: \"lead.title ==\" }, then: 1 }\n"), 5, "derive.x[0].when", "does not compile"},
		{"raw expr not bool", rubric("derive:\n  x:\n    - { when: { expr: \"1 + 2\" }, then: 1 }\n"), 5, "derive.x[0].when", "must be true or false"},

		{"detector without kind", rubric("detectors:\n  d: { event: visit_pricing }\n"), 4, "detectors.d", "kind is required"},
		{"detector window over 90 days", rubric("detectors:\n  d: { kind: count_in_window, event: visit_pricing, window: 91d, min: 1 }\n"), 4, "detectors.d.window", "longer than 90 days"},
		{"detector within over 90 days", rubric("detectors:\n  d: { kind: first_seen, event: visit_demo, within: 2160h1m }\n"), 4, "detectors.d.within", "longer than 90 days"},
		{"detector bad window", rubric("detectors:\n  d: { kind: count_in_window, event: e, window: soon, min: 1 }\n"), 4, "detectors.d.window", "invalid duration"},
		{"detector missing event", rubric("detectors:\n  d: { kind: first_seen, within: 7d }\n"), 4, "detectors.d", "first_seen needs event"},
		{"detector bad min", rubric("detectors:\n  d: { kind: count_in_window, event: e, window: 7d, min: 0 }\n"), 4, "detectors.d.min", "at least 1"},
		{"detector bad subject", rubric("detectors:\n  d: { kind: first_seen, event: e, within: 7d, subject: account }\n"), 4, "detectors.d.subject", "lead or company"},
		{"detector unknown key", rubric("detectors:\n  d: { kind: first_seen, event: e, within: 7d, window: 7d }\n"), 4, "detectors.d.window", "unknown key"},
		{"change on a lead field", rubric("detectors:\n  d: { kind: change, field: Job Title, within: 7d }\n"), 4, "detectors.d.field", "watches a company fact"},
		{"registered kind with stray keys", rubric("detectors:\n  d: { kind: my_kind, event: e }\n"), 4, "detectors.d.event", "unknown key"},

		{"rollup two ops", rubric("company:\n  r: { any: { field: title, present: true }, count: { field: title, present: true } }\n"), 4, "company.r", "a rollup is one of"},
		{"rollup unknown op", rubric("company:\n  r: { sum: fleet }\n"), 4, "company.r.sum", "unknown rollup"},
		{"rollup max on text", rubric("company:\n  r: { max: title }\n"), 4, "company.r.max", "number or date"},
		{"rollup reads a rollup", rubric("company:\n  a: { any: { field: title, present: true } }\n  b: { any: { field: company.a, eq: true } }\n"), 5, "company.b.any.field", "cannot read another rollup"},
		{"rollup reads a derived name", rubric("company:\n  a: { any: { field: x, eq: 1 } }\nderive:\n  x:\n    - else: 1\n"), 4, "company.a.any.field", "cannot read the derived name x"},
		{"rollup clashes with a company field", rubric("company:\n  employees: { count: { field: title, present: true } }\n"), 4, "company.employees", "already a company field"},

		{"derive not a list", rubric("derive:\n  x: 3\n"), 4, "derive.x", "non-empty list of rules"},
		{"derive bad rule", rubric("derive:\n  x:\n    - { when: { field: title, present: true } }\n"), 5, "derive.x[0]", "a rule is { when: C, then: V }"},
		{"else not last", rubric("derive:\n  x:\n    - else: 1\n    - { when: { field: title, present: true }, then: 2 }\n"), 5, "derive.x[0]", "else must be the last rule"},
		{"mixed value types", rubric("derive:\n  x:\n    - { when: { field: title, present: true }, then: 1 }\n    - else: one\n"), 4, "derive.x", "must have one type"},
		{"bad level", rubric("derive:\n  x: { level: account, rules: [ { else: 1 } ] }\n"), 4, "derive.x.level", "level is lead or company"},
		{"engine-produced name", rubric("derive:\n  status:\n    - else: new\n"), 4, "derive.status", "produced by the engine"},
		{"reads a later block", rubric("derive:\n  a:\n    - { when: { field: b, eq: 1 }, then: 1 }\n  b:\n    - else: 1\n"), 5, "derive.a[0].when.field", "derived at or after it"},
		{"reads itself", rubric("derive:\n  a:\n    - { when: { field: a, eq: 1 }, then: 1 }\n"), 5, "derive.a[0].when.field", "derived at or after it"},
		{"company block reads a lead field", rubric("derive:\n  t:\n    level: company\n    rules:\n      - { when: { field: title, present: true }, then: 1 }\n"), 7, "derive.t.rules[0].when.field", "company level and cannot read the lead value title"},
		{"company block reads status", rubric("derive:\n  t:\n    level: company\n    rules:\n      - { when: { field: status, eq: new }, then: 1 }\n"), 7, "derive.t.rules[0].when.field", "cannot read the lead value status"},
		{"company block reads a lead block", rubric("derive:\n  l:\n    - else: 1\n  t:\n    level: company\n    rules:\n      - { when: { field: l, eq: 1 }, then: 1 }\n"), 9, "derive.t.rules[0].when.field", "cannot read the lead value l"},
		{"company block reads a lead detector", rubric("detectors:\n  d: { kind: first_seen, event: e, within: 7d }\nderive:\n  t:\n    level: company\n    rules:\n      - { when: { detector: d }, then: 1 }\n"), 9, "derive.t.rules[0].when.detector", "lead-subject detector d"},
		{"company block raw expr reads lead", rubric("derive:\n  t:\n    level: company\n    rules:\n      - { when: { expr: \"has(lead.title)\" }, then: 1 }\n"), 7, "derive.t.rules[0].when.expr", "cannot read the lead value title"},
		{"lead raw expr reads a company block as lead", rubric("derive:\n  t: { level: company, rules: [ { else: 1 } ] }\n  l:\n    - { when: { expr: \"has(lead.t)\" }, then: 1 }\n"), 6, "derive.l[0].when.expr", "write company.t"},
		{"company. prefix on a lead block", rubric("derive:\n  l:\n    - else: 1\n  m:\n    - { when: { field: company.l, eq: 1 }, then: 1 }\n"), 7, "derive.m[0].when.field", "lead-level derived name"},

		{"conflicts on a derived name", rubric("derive:\n  x:\n    - else: 1\nconflicts:\n  - { field: x }\n"), 7, "conflicts[0].field", "cannot read the derived name x"},
		{"conflicts entry shape", rubric("conflicts:\n  - segment\n"), 4, "conflicts[0]", "must be a mapping"},

		{"score unknown half", rubric("score:\n  lead: []\n"), 4, "score.lead", "unknown key"},
		{"score rule without points", rubric("score:\n  contact:\n    - { when: { field: title, present: true } }\n"), 5, "score.contact[0]", "a score rule is"},
		{"score points not a number", rubric("score:\n  contact:\n    - { when: { field: title, present: true }, points: lots }\n"), 5, "score.contact[0].points", "needs a number"},
		{"account reads a lead value", rubric("score:\n  account:\n    - { when: { field: title, present: true }, points: 1 }\n"), 5, "score.account[0].when.field", "company level and cannot read the lead value title"},
		{"band on text", rubric("score:\n  contact:\n    - { band: title, points: { 1: 1 } }\n"), 5, "score.contact[0].band", "a band reads a number"},
		{"band threshold not a number", rubric("score:\n  contact:\n    - { band: sources_seen, points: { two: 1 } }\n"), 5, "score.contact[0].points.two", "threshold is a number"},
		{"account band on a lead value", rubric("score:\n  account:\n    - { band: sources_seen, points: { 1: 1 } }\n"), 5, "score.account[0].band", "cannot read the lead value sources_seen"},

		{"limits negative", rubric("limits: { max_pushes_per_run: -1 }\n"), 3, "limits.max_pushes_per_run", "zero or more"},
		{"limits bad timezone", rubric("limits: { timezone: Mars/Olympus }\n"), 3, "limits.timezone", "unknown timezone"},
		{"limits unknown key", rubric("limits: { per_hour: 3 }\n"), 3, "limits.per_hour", "unknown key"},

		{"lane without id", "version: 1\nlanes:\n  - { kind: export, push: export:x }\n", 3, "lanes[0]", "every lane needs an id"},
		{"lane id unsafe", "version: 1\nlanes:\n  - { id: a/b, kind: export, push: export:x }\n", 3, "lanes[0].id", "letters, digits"},
		{"duplicate lane id", "version: 1\nlanes:\n  - { id: a, kind: export, push: export:x }\n  - { id: a, kind: export, push: export:y }\n", 4, "lanes[1].id", "used twice"},
		{"bad lane kind", "version: 1\nlanes:\n  - { id: a, kind: warm, push: export:x }\n", 3, "lanes[0].kind", "cold, non-cold or export"},
		{"bad priority", "version: 1\nlanes:\n  - { id: a, kind: export, priority: high, push: export:x }\n", 3, "lanes[0].priority", "whole number"},
		{"no push", "version: 1\nlanes:\n  - { id: a, kind: cold }\n", 3, "lanes[0].push", "push is required"},
		{"push without sink", "version: 1\nlanes:\n  - { id: a, kind: cold, push: qualified }\n", 3, "lanes[0].push", "is not <sink>:<destination>"},
		{"apollo without sequence", "version: 1\nlanes:\n  - { id: a, kind: cold, push: apollo:list/x }\n", 3, "lanes[0].push", "unknown destination"},
		{"hubspot unknown object", "version: 1\nlanes:\n  - { id: a, kind: cold, push: hubspot:companies }\n", 3, "lanes[0].push", "unknown destination"},
		{"export on a cold lane", "version: 1\nlanes:\n  - { id: a, kind: cold, push: export:x }\n", 3, "lanes[0].push", "only export lanes"},
		{"export lane to a vendor", "version: 1\nlanes:\n  - { id: a, kind: export, push: hubspot:contacts }\n", 3, "lanes[0].push", "only export lanes"},
		{"lane unknown key", "version: 1\nlanes:\n  - { id: a, kind: export, push: export:x, sink: x }\n", 3, "lanes[0].sink", "unknown key"},
		// Review additions.
		{"alias node", "version: 1\nlanes: []\nsettings:\n  a: &x [1]\n  b: *x\n", 5, "settings.b", "YAML aliases (*x) are not allowed"},
		{"duplicate key", "version: 1\nlanes: []\nlimits: { timezone: UTC }\nlimits: { timezone: UTC }\n", 4, "limits", "limits appears twice"},
		{"duplicate nested key", rubric("derive:\n  x:\n    - { when: { field: title, present: true }, when: { field: email, present: true }, then: 1 }\n"), 5, "derive.x[0].when", "when appears twice"},
		{"aliases not a list", rubric("fields:\n  x: { aliases: Tool }\n"), 4, "fields.x.aliases", "aliases is a list"},
		{"empty alias", rubric("fields:\n  x: { aliases: [\"--\"] }\n"), 4, "fields.x.aliases[0]", "at least one letter or digit"},
		{"declared status", rubric("fields:\n  status: { type: text }\n"), 4, "fields.status", "cannot be declared"},
		{"change without field", rubric("detectors:\n  d: { kind: change, within: 7d }\n"), 4, "detectors.d", "change needs field"},
		{"change without within", rubric("detectors:\n  d: { kind: change, field: region }\n"), 4, "detectors.d", "change needs within"},
		{"change on a rollup", rubric("company:\n  r: { count: { field: title, present: true } }\ndetectors:\n  d: { kind: change, field: company.r, within: 7d }\n"), 6, "detectors.d.field", "worked out each run"},
		{"change on leads_seen", rubric("detectors:\n  d: { kind: change, field: leads_seen, within: 7d }\n"), 4, "detectors.d.field", "worked out each run"},
		{"change within over 90 days", rubric("detectors:\n  d: { kind: change, field: region, within: 91d }\n"), 4, "detectors.d.within", "longer than 90 days"},
		{"params not a mapping", rubric("detectors:\n  d: { kind: my_kind, params: [1] }\n"), 4, "detectors.d.params", "params is a mapping"},
		{"zero duration", rubric("detectors:\n  d: { kind: first_seen, event: e, within: 0d }\n"), 4, "detectors.d.within", "longer than zero"},
		{"star inside an event", rubric("detectors:\n  d: { kind: first_seen, event: \"visit_*_page\", within: 1d }\n"), 4, "detectors.d.event", "* may only end an event kind"},
		{"rollup max on a company field", rubric("company:\n  r: { max: company.employees }\n"), 4, "company.r.max", "reads a lead field"},
		{"rollup first on status", rubric("company:\n  r: { first: status }\n"), 4, "company.r.first", "reads a lead field"},
		{"rollup and company block share a name", rubric("company:\n  t: { count: { field: title, present: true } }\nderive:\n  t: { level: company, rules: [ { else: 1 } ] }\n"), 4, "company.t", "also a company-level derived name"},
		{"then a list", rubric("derive:\n  x:\n    - { when: { field: title, present: true }, then: [1] }\n"), 5, "derive.x[0]", "single value or null"},
		{"derived Ranked column", rubric("derive:\n  score:\n    - else: 1\n"), 4, "derive.score", "fixed column of the Ranked table"},
		{"derived email", rubric("derive:\n  email:\n    - else: x\n"), 4, "derive.email", "fixed column of the Ranked table"},
		{"conflicts on status", rubric("conflicts:\n  - { field: status }\n"), 4, "conflicts[0].field", "names an input field"},
		{"conflicts on a rollup", rubric("company:\n  r: { count: { field: title, present: true } }\nconflicts:\n  - { field: company.r }\n"), 6, "conflicts[0].field", "names an input field"},
		{"conflicts not a list", rubric("conflicts: { field: segment }\n"), 3, "conflicts", "conflicts is a list"},
		{"band with empty points", rubric("score:\n  contact:\n    - { band: sources_seen, points: {} }\n"), 5, "score.contact[0].points", "map thresholds to points"},
		{"band repeated threshold", rubric("score:\n  contact:\n    - { band: sources_seen, points: { 2: 1, 2.0: 3 } }\n"), 5, "score.contact[0].points.2.0", "threshold 2 appears twice"},
		{"points without when or band", rubric("score:\n  contact:\n    - { points: 3 }\n"), 5, "score.contact[0]", "needs when (or band)"},
		{"score half not a list", rubric("score:\n  contact: { points: 3 }\n"), 4, "score.contact", "contact is a list of rules"},
		{"in not a list", rubric("derive:\n  x:\n    - { when: { field: title, in: cto }, then: 1 }\n"), 5, "derive.x[0].when.in", "needs a list, or a $setting"},
		{"in a mapping", rubric("derive:\n  x:\n    - { when: { field: title, in: { a: 1 } }, then: 1 }\n"), 5, "derive.x[0].when.in", "needs a list, not a mapping"},
		{"eq null", rubric("derive:\n  x:\n    - { when: { field: title, eq: null }, then: 1 }\n"), 5, "derive.x[0].when.eq", "a value is required"},
		{"empty field", rubric("derive:\n  x:\n    - { when: { field: \"\", eq: a }, then: 1 }\n"), 5, "derive.x[0].when.field", "field needs a field name"},
		{"empty detector", rubric("derive:\n  x:\n    - { when: { detector: \"\" }, then: 1 }\n"), 5, "derive.x[0].when.detector", "needs a detector name"},
		{"empty expr", rubric("derive:\n  x:\n    - { when: { expr: \" \" }, then: 1 }\n"), 5, "derive.x[0].when.expr", "needs a CEL expression"},
		{"detector as a field", rubric("detectors:\n  d: { kind: first_seen, event: e, within: 1d }\nderive:\n  x:\n    - { when: { field: detector.d, eq: true }, then: 1 }\n"), 7, "derive.x[0].when.field", "unknown field"},
		{"header spelling of another field", rubric("derive:\n  x:\n    - { when: { field: jobtitle, present: true }, then: 1 }\n"), 5, "derive.x[0].when.field", "jobtitle is a header spelling of title; write title"},
		{"company header spelling", rubric("derive:\n  x:\n    - { when: { field: company.headcount, gt: 1 }, then: 1 }\n"), 5, "derive.x[0].when.field", "write company.employees"},
		{"rubric alias as a field", rubric("fields:\n  uses_tool: { aliases: [Tool] }\nderive:\n  x:\n    - { when: { field: tool, present: true }, then: 1 }\n"), 7, "derive.x[0].when.field", "write uses_tool"},
		{"raw expr header spelling", rubric("derive:\n  x:\n    - { when: { expr: \"has(lead.jobtitle)\" }, then: 1 }\n"), 5, "derive.x[0].when.expr", "write title"},
		{"too costly", rubric("derive:\n  x:\n    - when: { expr: \"company.all(a, company.all(b, company.all(c, a != b)))\" }\n      then: 1\n"), 5, "derive.x[0].when", "could cost too much"},
		{"too costly literal", rubric("derive:\n  x:\n    - when: { expr: \"" + nestedAll(6) + "\" }\n      then: 1\n"), 5, "derive.x[0].when", "could cost too much"},
		{"limits Local", rubric("limits: { timezone: Local }\n"), 3, "limits.timezone", "unknown timezone"},
		{"lanes not a list", "version: 1\nlanes: { id: a }\n", 2, "lanes", "lanes is a list"},
		{"lane without when", "version: 1\nlanes:\n  - { id: a, kind: export, push: export:x }\n", 3, "lanes[0]", "every lane needs when"},
		{"lane ids differ only in case", "version: 1\nlanes:\n  - { id: Warm, kind: export, when: { field: status, eq: new }, push: export:x }\n  - { id: warm, kind: export, when: { field: status, eq: new }, push: export:y }\n", 4, "lanes[1].id", "used twice (ignoring case)"},
		{"lane id starts with a hyphen", "version: 1\nlanes:\n  - { id: -a, kind: export, when: { field: status, eq: new }, push: export:x }\n", 3, "lanes[0].id", "letters, digits"},
		{"status typo in a list", rubric("derive:\n  x:\n    - { when: { field: status, not_in: [new, replyed] }, then: 1 }\n"), 5, "derive.x[0].when.not_in", `"replyed" is not a status`},
		{"undeclared detector in expr", rubric("derive:\n  x:\n    - { when: { expr: \"detector.pricing\" }, then: 1 }\n"), 5, "derive.x[0].when.expr", `detector "pricing" is not declared`},
		{"undeclared single-value setting", rubric("derive:\n  x:\n    - { when: { field: title, eq: $boss }, then: 1 }\n"), 5, "derive.x[0].when.eq", "setting $boss is not declared"},
		{"duration not a value", rubric("detectors:\n  d: { kind: first_seen, event: e, within: [7d] }\n"), 4, "detectors.d.within", "needs a duration such as 7d"},
		{"setting list item not a value", rubric("settings:\n  s: [a, [b]]\n"), 4, "settings.s[1]", "list items must be single values"},
		{"ordered type with extra keys", rubric("settings:\n  o: [a]\nfields:\n  x: { type: { ordered: o, by: x } }\n"), 6, "fields.x.type", "type is text, number, date, bool"},
		{"rollup first without a field", rubric("company:\n  r: { first: \"\" }\n"), 4, "company.r.first", "first needs a lead field name"},
		{"comparison value a list", rubric("derive:\n  x:\n    - { when: { field: title, eq: [a] }, then: 1 }\n"), 5, "derive.x[0].when.eq", "needs a single value, not a list"},
		{"in list item a list", rubric("derive:\n  x:\n    - { when: { field: title, in: [a, [b]] }, then: 1 }\n"), 5, "derive.x[0].when.in[1]", "list items must be single values"},
		{"in list item wrong type", rubric("derive:\n  x:\n    - { when: { field: sources_seen, in: [1, two] }, then: 1 }\n"), 5, "derive.x[0].when.in", `"two" is not a number`},
		{"lane when error", "version: 1\nlanes:\n  - { id: a, kind: export, push: export:x, when: { field: tier, lte: 2 } }\n", 3, "lanes[0].when.lte", "tier is text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile([]byte(tt.src))
			var errs LoadErrors
			if !errors.As(err, &errs) {
				t.Fatalf("got %v, want LoadErrors", err)
			}
			for _, e := range errs {
				if strings.Contains(e.Msg, tt.msg) {
					if e.Line != tt.line || e.Field != tt.field {
						t.Errorf("got line %d field %q, want line %d field %q (%s)", e.Line, e.Field, tt.line, tt.field, e.Msg)
					}
					return
				}
			}
			t.Errorf("no error containing %q; got:\n%v", tt.msg, err)
		})
	}
}

// Compile keeps going after an error, so `rules check` reports them all.
func TestLoadErrorsAreAllReported(t *testing.T) {
	_, err := Compile([]byte("version: 1\nlanes:\n  - { kind: cold, push: x }\nlimits: { timezone: Nowhere }\n"))
	var errs LoadErrors
	if !errors.As(err, &errs) || len(errs) < 3 {
		t.Fatalf("want at least 3 errors, got %v", err)
	}
	if !strings.HasPrefix(errs.Error(), "line 3: lanes[0]: ") {
		t.Errorf("errors are in line order with line and field: %q", errs.Error())
	}
}

func TestDefaultsAndAccessors(t *testing.T) {
	r := mustCompile(t, rubric(`
fields:
  uses_competitor: { aliases: ["Current tool", "Tool in use"] }
  employees: { type: number, level: company, aliases: ["# Employees"] }
  industry: { level: company, aliases: [Sector] }
detectors:
  pricing: { kind: count_in_window, event: "visit_*", window: 7d, min: 3, subject: company }
  demo: { kind: first_seen, event: visit_demo, within: 90d }
  raised: { kind: change, field: company.funding_stage, within: 30d, to: series_b }
  custom: { kind: my_kind, subject: company, params: { threshold: 3, name: "x" } }
conflicts:
  - { field: segment }
  - { field: company.region }
`))
	if got := r.Limits(); got.MaxPushesPerRun != 100 || got.MaxPushesPerDay != 200 || got.Timezone != "UTC" || got.Location != time.UTC {
		t.Errorf("limits defaults: %+v", got)
	}
	wantAliases := map[string]string{
		"usescompetitor": "uses_competitor", "currenttool": "uses_competitor", "toolinuse": "uses_competitor",
		"employees": "company.employees", "industry": "company.industry", "sector": "company.industry",
	}
	if got := r.Aliases(); !reflect.DeepEqual(got, wantAliases) {
		t.Errorf("Aliases() = %v, want %v", got, wantAliases)
	}
	if got := r.ConflictFields(); !reflect.DeepEqual(got, []string{"segment", "company.region"}) {
		t.Errorf("ConflictFields() = %v", got)
	}
	ds := r.Detectors()
	if len(ds) != 4 || ds[0].Name != "pricing" || ds[0].Event != "visit_*" || ds[0].Window != 7*24*time.Hour || ds[0].Min != 3 || ds[0].Subject != "company" {
		t.Errorf("detectors[0] = %+v", ds[0])
	}
	if ds[1].Subject != "lead" || ds[1].Within != 90*24*time.Hour {
		t.Errorf("detectors[1] = %+v", ds[1])
	}
	if ds[2].Field != "funding_stage" || ds[2].To == nil || *ds[2].To != "series_b" || ds[2].From != nil {
		t.Errorf("detectors[2] = %+v", ds[2])
	}
	if ds[3].Kind != "my_kind" || ds[3].Params["threshold"] != 3 {
		t.Errorf("detectors[3] = %+v", ds[3])
	}
	if got := r.Fields(); !reflect.DeepEqual(got, []string{"company.funding_stage", "company.region", "segment"}) {
		t.Errorf("Fields() = %v", got)
	}
}

func TestLanes(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes:
  - { id: q, kind: cold, priority: 10, when: { field: receiver_only, eq: false }, push: "apollo:sequence/Fleet ops: intro" }
  - { id: c, name: Contacts, kind: non-cold, when: { field: status, eq: replied_positive }, push: hubspot:contacts }
  - { id: d, kind: non-cold, when: { field: status, eq: replied_positive }, push: hubspot:deals }
  - { id: x, kind: export, when: { field: sources_seen, gte: 1 }, push: export:ranked-list }
  - { id: p, kind: cold, when: { field: receiver_only, eq: false }, push: mysink:anything/here }
`)
	got := r.Lanes()
	if len(got) != 5 {
		t.Fatalf("lanes: %+v", got)
	}
	if l := got[0]; l.ID != "q" || l.Name != "q" || l.Kind != "cold" || l.Priority != 10 || l.Sink != "apollo" ||
		l.Dest != "sequence/Fleet ops: intro" || l.When != "receiver_only = false" {
		t.Errorf("lane q: %+v", l)
	}
	if got[1].Name != "Contacts" || got[1].When != "status = replied_positive" {
		t.Errorf("lane c: %+v", got[1])
	}
	if got[4].Sink != "mysink" || got[4].Dest != "anything/here" {
		t.Errorf("a plug-in sink's syntax is accepted; registration is checked at run start: %+v", got[4])
	}
}

// The version changes with content, not with comments, key order or layout.
func TestVersion(t *testing.T) {
	a := mustCompile(t, "version: 1\nlanes: []\nlimits: { max_pushes_per_run: 5, timezone: UTC }\n")
	b := mustCompile(t, "# a comment\nlimits:\n  timezone: UTC   # trailing\n  max_pushes_per_run: 5\nlanes: []\nversion: 1\n")
	c := mustCompile(t, "version: 1\nlanes: []\nlimits: { max_pushes_per_run: 6, timezone: UTC }\n")
	if a.Version() != b.Version() {
		t.Errorf("comments and order changed the version: %s vs %s", a.Version(), b.Version())
	}
	if a.Version() == c.Version() {
		t.Error("a content change kept the version")
	}
	if len(a.Version()) != 18 || !strings.HasPrefix(a.Version(), "r-") {
		t.Errorf("version %q is not r- plus 16 hex characters", a.Version())
	}
}

func TestWarnings(t *testing.T) {
	r := mustCompile(t, rubric("derive:\n  segment:\n    - else: x\n"))
	if w := r.Warnings(); len(w) != 1 || w[0].Line != 4 || !strings.Contains(w[0].Msg, "shadows the input field segment") {
		t.Errorf("warnings: %v", w)
	}
}

// The rendered text of each condition form, used in reasons.
func TestConditionText(t *testing.T) {
	r := mustCompile(t, `version: 1
settings:
  funding_order: [seed, series_a, series_b]
  tools: [a, b]
detectors:
  d: { kind: first_seen, event: e, within: 1d }
lanes:
  - { id: l1, kind: export, push: export:x, when: { all: [ { field: title, eq: "Head of Ops" }, { any: [ { field: company.employees, gte: 20 }, { not: { field: company.funding_stage, lt: series_a } } ] } ] } }
  - { id: l2, kind: export, push: export:x, when: { any: [ { field: tool, in: $tools }, { field: tool, not_in: [c, "d e"] }, { detector: d } ] } }
  - { id: l3, kind: export, push: export:x, when: { all: [ { field: title, contains: vp }, { field: email, missing: true }, { expr: "status == 'new'" } ] } }
`)
	want := []string{
		`title = "Head of Ops" and (company.employees >= 20 or not company.funding_stage < series_a)`,
		`tool in $tools or tool not in [c, "d e"] or d fired`,
		`title contains vp and email is missing and status == 'new'`,
	}
	for i, l := range r.Lanes() {
		if l.When != want[i] {
			t.Errorf("lane %d: %q, want %q", i, l.When, want[i])
		}
	}
}

// nestedAll is depth nested .all() calls over a ten-item list: 10^depth steps.
func nestedAll(depth int) string {
	l := "[0, 1, 2, 3, 4, 5, 6, 7, 8, 9]"
	e := "true"
	for i := 0; i < depth; i++ {
		e = l + ".all(v" + strconv.Itoa(i) + ", " + e + ")"
	}
	return e
}

// A built-in detector kind in another case is the built-in kind, with its
// checks: written without min it fails, rather than compiling as a registered
// kind that fires for every lead.
func TestDetectorKindNamesAreLowercased(t *testing.T) {
	_, err := Compile([]byte(rubric("detectors:\n  hot: { kind: Count_In_Window, event: visit_x, window: 7d }\n")))
	if err == nil || !strings.Contains(err.Error(), "min") {
		t.Fatalf("err = %v, want count_in_window's missing min", err)
	}
	r := mustCompile(t, rubric("detectors:\n  hot: { kind: Count_In_Window, event: visit_x, window: 7d, min: 2 }\n"))
	if d := r.Detectors()[0]; d.Kind != "count_in_window" || d.Min != 2 {
		t.Errorf("%+v", d)
	}
}
