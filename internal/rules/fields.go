package rules

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// kind is a field's value type (contracts section 2, "Fields").
type kind int

const (
	kText kind = iota
	kNumber
	kDate
	kBool
	kOrdered // text ranked by a settings list
)

// ftype is a field's type; order names the settings list of an ordered field.
type ftype struct {
	kind  kind
	order string
}

func (t ftype) String() string {
	switch t.kind {
	case kNumber:
		return "number"
	case kDate:
		return "date"
	case kBool:
		return "bool"
	case kOrdered:
		return "ordered by $" + t.order
	}
	return "text"
}

// fieldDef is a built-in or declared field.
type fieldDef struct {
	name    string // without the company. prefix
	typ     ftype
	company bool
	builtin bool
	aliases []string
}

// statuses are the folded statuses (RFC 6.3); a condition on `status` may name
// only these, so a typo fails at load instead of never matching.
var statuses = []string{
	"new", "contacted", "replied_positive", "replied_negative", "replied_neutral",
	"replied_unlabelled", "deal", "unsubscribed", "blocked",
}

// Built-in fields (RFC 6.4). `status` is not here: it is the CEL `status`
// variable, not a lead field.
func builtinLeadFields() map[string]*fieldDef {
	m := map[string]*fieldDef{}
	for _, n := range []string{"email", "linkedin_url", "full_name", "title", "contact_id", "warm_path", "trigger_note", "segment"} {
		m[n] = &fieldDef{name: n, builtin: true}
	}
	m["sources_seen"] = &fieldDef{name: "sources_seen", typ: ftype{kind: kNumber}, builtin: true}
	m["receiver_only"] = &fieldDef{name: "receiver_only", typ: ftype{kind: kBool}, builtin: true}
	return m
}

func builtinCompanyFields() map[string]*fieldDef {
	m := map[string]*fieldDef{}
	for _, n := range []string{"domain", "name", "region"} {
		m[n] = &fieldDef{name: n, company: true, builtin: true}
	}
	m["employees"] = &fieldDef{name: "employees", typ: ftype{kind: kNumber}, company: true, builtin: true}
	m["funding_stage"] = &fieldDef{name: "funding_stage", typ: ftype{kind: kOrdered, order: "funding_order"}, company: true, builtin: true}
	m["leads_seen"] = &fieldDef{name: "leads_seen", typ: ftype{kind: kNumber}, company: true, builtin: true}
	return m
}

// mergeProduced are built-in lead fields merge or the status fold produce,
// never an input column, so a derived name may not take them.
var mergeProduced = map[string]bool{"status": true, "sources_seen": true, "receiver_only": true}

var (
	// nameRe is a declared, derived, rollup, detector or setting name.
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// squashedRe is an undeclared column's name: a squashed header.
	squashedRe = regexp.MustCompile(`^[a-z0-9]+$`)
)

// normText is the text-matching form: trimmed and lowercased.
func normText(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normOrdered is the ordered-list matching form: normText, also ignoring
// spaces, hyphens and underscores, so `Series B` matches `series_b`.
func normOrdered(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '_', '\t':
			return -1
		}
		return r
	}, normText(s))
}

// parseValue reads a raw input value as its field's type. ok is false for an
// empty value (absent) and for one that does not parse (also absent; bad is
// then true so the caller can warn).
func parseValue(t ftype, raw string) (v any, ok, bad bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false, false
	}
	switch t.kind {
	case kNumber:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, false, true
		}
		return f, true, false
	case kDate:
		d, err := parseDate(s)
		if err != nil {
			return nil, false, true
		}
		return d, true, false
	case kBool:
		b, err := parseBool(s)
		if err != nil {
			return nil, false, true
		}
		return b, true, false
	}
	return s, true, false
}

// parseDate reads ISO 8601: a date, or a date and time with a zone.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not an ISO 8601 date", s)
}

func parseBool(s string) (bool, error) {
	switch normText(s) {
	case "true", "yes", "1":
		return true, nil
	case "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not true/false/yes/no/1/0", s)
}

// scalar is one YAML scalar from the rubric: its text as written and its YAML
// type, so a value can be read as whatever type its field needs.
type scalar struct {
	text string
	tag  string // !!str, !!int, !!float, !!bool, !!timestamp, !!null
}

func scalarOf(n *yaml.Node) (scalar, bool) {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return scalar{}, false
	}
	return scalar{text: n.Value, tag: n.ShortTag()}, true
}

// natural is a scalar's own YAML value: float64, bool, string or nil.
func (s scalar) natural() any {
	switch s.tag {
	case "!!int", "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(s.text, "_", ""), 64); err == nil {
			return f
		}
		if i, err := strconv.ParseInt(strings.ReplaceAll(s.text, "_", ""), 0, 64); err == nil {
			return float64(i)
		}
	case "!!bool":
		if b, err := parseBool(s.text); err == nil {
			return b
		}
	case "!!null":
		return nil
	}
	return s.text
}

// as reads a rubric value as type t, for comparing with a field of that type.
func (s scalar) as(t ftype) (any, error) {
	if s.tag == "!!null" {
		return nil, fmt.Errorf("a value is required")
	}
	switch t.kind {
	case kNumber:
		if f, ok := s.natural().(float64); ok {
			return f, nil
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s.text), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("%q is not a number", s.text)
		}
		return f, nil
	case kDate:
		return parseDate(s.text)
	case kBool:
		if b, ok := s.natural().(bool); ok {
			return b, nil
		}
		return parseBool(s.text)
	}
	return s.text, nil
}
