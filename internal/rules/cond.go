package rules

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"gopkg.in/yaml.v3"
)

// condition is one compiled condition: its CEL program and how it reads to a
// person, for reasons.
type condition struct {
	text string
	expr string
	prg  cel.Program
	raw  bool // from `expr:`; may fail at run time on a missing key
}

// refKind is what a field name refers to.
type refKind int

const (
	rLead     refKind = iota // a lead input field: lead[name]
	rFact                    // a company input fact: company[name]
	rRollup                  // company[name]
	rDerived                 // lead[name] or company[name], per the block's level
	rStatus                  // the status variable
	rDetector                // detector[name]
)

// fieldRef is a resolved field reference.
type fieldRef struct {
	kind    refKind
	name    string // the key in its map
	display string // as a person writes it
	typ     ftype
	company bool // lives in the company map
	derived *deriveBlock
	det     *DetectorSpec
}

func (r fieldRef) access() string {
	switch r.kind {
	case rStatus:
		return "status"
	case rDetector:
		return "detector[" + celString(r.name) + "]"
	}
	if r.company {
		return "company[" + celString(r.name) + "]"
	}
	return "lead[" + celString(r.name) + "]"
}

// has is the CEL presence test, or "" for a value that is always present.
func (r fieldRef) has() string {
	switch r.kind {
	case rStatus, rDetector:
		return ""
	}
	if r.company {
		return celString(r.name) + " in company"
	}
	return celString(r.name) + " in lead"
}

// scope says what a condition may read.
type scope struct {
	what    string // for errors: "a rollup", "company block tier", ...
	company bool   // company level: no lead values, no lead-subject detectors
	rollup  bool   // a rollup: no rollups, no derived names
	// derivedBefore: derived blocks with a lower index are readable.
	derivedBefore int
}

// resolve finds what a field name refers to (contracts section 2: a built-in
// field, a declared or input column, company.<name> for a company field or
// rollup, or a derived name).
func (c *compiler) resolve(name string, sc scope, a at) (fieldRef, bool) {
	if name == "status" {
		return fieldRef{kind: rStatus, name: name, display: name, typ: ftype{kind: kText}}, true
	}
	if d, ok := strings.CutPrefix(name, "detector."); ok {
		spec, ok := c.detectorByName[d]
		if !ok {
			c.errf(a, "detector %q is not declared under detectors", d)
			return fieldRef{}, false
		}
		return fieldRef{kind: rDetector, name: d, display: name, typ: ftype{kind: kBool}, det: spec}, true
	}
	visible := func(d *deriveBlock) bool { return !sc.rollup && d.idx < sc.derivedBefore }
	if rest, ok := strings.CutPrefix(name, "company."); ok {
		if d, ok := c.deriveByName[rest]; ok && d.company && visible(d) {
			return fieldRef{kind: rDerived, name: rest, display: name, typ: d.typ, company: true, derived: d}, true
		}
		if f, ok := c.companyFields[rest]; ok {
			return c.readFact(f.name, f.typ, name), true
		}
		if ro, ok := c.rollupByName[rest]; ok {
			return fieldRef{kind: rRollup, name: rest, display: name, typ: ro.typ, company: true}, true
		}
		if d, ok := c.deriveByName[rest]; ok {
			if !d.company {
				c.errf(a, "%s is a lead-level derived name; write %s", rest, rest)
				return fieldRef{}, false
			}
			return c.notYet(d, sc, a)
		}
		if squashedRe.MatchString(rest) {
			// An undeclared Companies tab column: text, like any undeclared column.
			return c.readFact(rest, ftype{kind: kText}, name), true
		}
		c.errf(a, "unknown company field %q: declare it under fields with level: company, or use the column's squashed header name", name)
		return fieldRef{}, false
	}
	if d, ok := c.deriveByName[name]; ok && visible(d) {
		return fieldRef{kind: rDerived, name: name, display: name, typ: d.typ, company: d.company, derived: d}, true
	}
	if f, ok := c.leadFields[name]; ok {
		return c.readLead(f.name, f.typ), true
	}
	if d, ok := c.deriveByName[name]; ok {
		return c.notYet(d, sc, a)
	}
	if _, ok := c.companyFields[name]; ok {
		c.errf(a, "%s is a company field; write company.%s", name, name)
		return fieldRef{}, false
	}
	if squashedRe.MatchString(name) {
		return c.readLead(name, ftype{kind: kText}), true
	}
	c.errf(a, "unknown field %q: declare it under fields, or use the column's squashed header name (lowercase letters and digits only)", name)
	return fieldRef{}, false
}

func (c *compiler) notYet(d *deriveBlock, sc scope, a at) (fieldRef, bool) {
	switch {
	case sc.rollup:
		c.errf(a, "%s cannot read the derived name %s: it is worked out before derive runs", sc.what, d.name)
	default:
		c.errf(a, "%s reads %s, which is derived at or after it; derive blocks read only names above them", sc.what, d.name)
	}
	return fieldRef{}, false
}

func (c *compiler) readLead(name string, t ftype) fieldRef {
	c.reads[name] = true
	return fieldRef{kind: rLead, name: name, display: name, typ: t}
}

func (c *compiler) readFact(name string, t ftype, display string) fieldRef {
	c.reads["company."+name] = true
	return fieldRef{kind: rFact, name: name, display: display, typ: t, company: true}
}

// allowed checks a resolved reference against the scope.
func (c *compiler) allowed(r fieldRef, sc scope, a at) bool {
	if sc.company {
		switch {
		case r.kind == rLead, r.kind == rStatus, r.kind == rDerived && !r.company:
			c.errf(a, "%s is company level and cannot read the lead value %s", sc.what, r.display)
			return false
		case r.kind == rDetector && r.det.Subject != "company":
			c.errf(a, "%s is company level and cannot read the lead-subject detector %s", sc.what, r.name)
			return false
		}
	}
	if sc.rollup && r.kind == rRollup {
		c.errf(a, "%s cannot read another rollup (%s)", sc.what, r.display)
		return false
	}
	return true
}

// cond compiles one condition. It returns nil after recording an error.
func (c *compiler) cond(n *yaml.Node, a at, sc scope) *condition {
	errs := len(c.errs)
	expr, text, raw := c.condExpr(n, a, sc)
	if len(c.errs) > errs {
		return nil
	}
	env := c.leadEnv
	if sc.company {
		env = c.companyEnv
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		c.errf(a, "the condition does not compile: %v", iss.Err())
		return nil
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		c.errf(a, "the condition must be true or false, but gives %s", t)
		return nil
	}
	prg, err := env.Program(ast)
	if err != nil {
		c.errf(a, "the condition does not compile: %v", err)
		return nil
	}
	return &condition{text: unwrap(text), expr: expr, prg: prg, raw: raw}
}

// unwrap drops one pair of parentheses around a whole condition's text.
func unwrap(s string) string {
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return s
	}
	depth, quoted := 0, false
	for i, r := range s {
		if r == '"' && (i == 0 || s[i-1] != '\\') {
			quoted = !quoted
		}
		if quoted {
			continue
		}
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s // the first group closes before the end: (a) and (b)
			}
		}
	}
	return s[1 : len(s)-1]
}

var comparisons = map[string]string{"eq": "=", "ne": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}

var celOps = map[string]string{"eq": "==", "ne": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}

// condExpr turns a condition into CEL source and its text. raw is true when
// any part came from `expr:`.
func (c *compiler) condExpr(n *yaml.Node, a at, sc scope) (expr, text string, raw bool) {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		c.errf(a, "a condition must be a mapping such as { field: F, eq: V }, not %s", kindName(n))
		return "", "", false
	}
	ps := pairs(n)
	keys := make([]string, len(ps))
	for i, p := range ps {
		keys[i] = p.key
	}
	byKey := map[string]pair{}
	for _, p := range ps {
		byKey[p.key] = p
	}
	if f, ok := byKey["field"]; ok {
		if len(ps) != 2 {
			c.errf(a, "a field condition has `field` and one operator (eq, ne, lt, lte, gt, gte, in, not_in, contains, present, missing); got %s", strings.Join(keys, ", "))
			return "", "", false
		}
		var op pair
		for _, p := range ps {
			if p.key != "field" {
				op = p
			}
		}
		return c.fieldCond(f, op, a, sc)
	}
	if len(ps) != 1 {
		c.errf(a, "a condition has exactly one form (field, all, any, not, detector, expr); got %s", strings.Join(keys, ", "))
		return "", "", false
	}
	p := ps[0]
	pa := at{p.value, a.key(p.key)}
	switch p.key {
	case "all", "any":
		items := deref(p.value)
		if items == nil || items.Kind != yaml.SequenceNode || len(items.Content) == 0 {
			c.errf(pa, "%s needs a non-empty list of conditions", p.key)
			return "", "", false
		}
		var exprs, texts []string
		for i, item := range items.Content {
			e, t, r := c.condExpr(item, at{deref(item), pa.index(i)}, sc)
			exprs = append(exprs, "("+e+")")
			texts = append(texts, t)
			raw = raw || r
		}
		join, word := " && ", " and "
		if p.key == "any" {
			join, word = " || ", " or "
		}
		if len(texts) == 1 {
			return exprs[0], texts[0], raw
		}
		return strings.Join(exprs, join), "(" + strings.Join(texts, word) + ")", raw
	case "not":
		e, t, r := c.condExpr(p.value, pa, sc)
		return "!(" + e + ")", "not " + t, r
	case "detector":
		s, ok := scalarOf(p.value)
		if !ok || s.text == "" {
			c.errf(pa, "detector needs a detector name")
			return "", "", false
		}
		spec, ok := c.detectorByName[s.text]
		if !ok {
			c.errf(pa, "detector %q is not declared under detectors", s.text)
			return "", "", false
		}
		r := fieldRef{kind: rDetector, name: s.text, display: "detector." + s.text, typ: ftype{kind: kBool}, det: spec}
		if !c.allowed(r, sc, pa) {
			return "", "", false
		}
		return r.access(), s.text + " fired", false
	case "expr":
		s, ok := scalarOf(p.value)
		if !ok || strings.TrimSpace(s.text) == "" {
			c.errf(pa, "expr needs a CEL expression")
			return "", "", false
		}
		c.checkRawRefs(s.text, pa, sc)
		return s.text, s.text, true
	}
	c.errf(a, "unknown condition form %q; use field, all, any, not, detector or expr", p.key)
	return "", "", false
}

func (c *compiler) fieldCond(f, op pair, a at, sc scope) (expr, text string, raw bool) {
	fa := at{f.value, a.key("field")}
	fs, ok := scalarOf(f.value)
	if !ok || fs.text == "" {
		c.errf(fa, "field needs a field name")
		return "", "", false
	}
	r, ok := c.resolve(fs.text, sc, fa)
	if !ok || !c.allowed(r, sc, fa) {
		return "", "", false
	}
	oa := at{op.value, a.key(op.key)}
	guard := func(cmp string) string {
		if h := r.has(); h != "" {
			return "(" + h + " && " + cmp + ")"
		}
		return cmp
	}
	switch op.key {
	case "present", "missing":
		s, ok := scalarOf(op.value)
		if !ok || s.natural() != true {
			c.errf(oa, "write %s: true", op.key)
			return "", "", false
		}
		h := r.has()
		if h == "" {
			h = "true"
		}
		if op.key == "missing" {
			return "!(" + h + ")", r.display + " is missing", false
		}
		return h, r.display + " is present", false
	case "eq", "ne":
		v, vt, ok := c.value(op.value, r, oa)
		if !ok {
			return "", "", false
		}
		var cmp string
		switch r.typ.kind {
		case kText:
			cmp = "textEq(" + r.access() + ", " + celString(v.(string)) + ")"
		case kOrdered:
			cmp = "ordEq(" + r.access() + ", " + celString(v.(string)) + ")"
		default:
			cmp = r.access() + " == " + celLiteral(v)
		}
		if op.key == "ne" {
			cmp = "!" + cmp
		}
		return guard(cmp), r.display + " " + comparisons[op.key] + " " + vt, false
	case "lt", "lte", "gt", "gte":
		v, vt, ok := c.value(op.value, r, oa)
		if !ok {
			return "", "", false
		}
		switch r.typ.kind {
		case kNumber, kDate:
			return guard(r.access() + " " + celOps[op.key] + " " + celLiteral(v)), r.display + " " + comparisons[op.key] + " " + vt, false
		case kOrdered:
			list, ok := c.orderList(r.typ.order, oa)
			if !ok {
				return "", "", false
			}
			k := rank(v.(string), list)
			if k < 0 {
				c.errf(oa, "%q is not in settings.%s", v, r.typ.order)
				return "", "", false
			}
			cmp := fmt.Sprintf("ordRank(%s, %s) %s %d", r.access(), celList(toAny(list)), celOps[op.key], k)
			return guard(cmp), r.display + " " + comparisons[op.key] + " " + vt, false
		}
		c.errf(oa, "%s compares numbers, dates and ordered fields; %s is %s", op.key, r.display, r.typ)
		return "", "", false
	case "in", "not_in":
		vs, vt, ok := c.values(op.value, r, oa)
		if !ok {
			return "", "", false
		}
		var cmp string
		switch r.typ.kind {
		case kText:
			cmp = "textIn(" + r.access() + ", " + celList(vs) + ")"
		case kOrdered:
			cmp = "ordIn(" + r.access() + ", " + celList(vs) + ")"
		default:
			cmp = r.access() + " in " + celList(vs)
		}
		word := " in "
		if op.key == "not_in" {
			cmp, word = "!"+cmp, " not in "
		}
		return guard(cmp), r.display + word + vt, false
	case "contains":
		if r.typ.kind != kText && r.typ.kind != kOrdered {
			c.errf(oa, "contains matches text; %s is %s", r.display, r.typ)
			return "", "", false
		}
		v, vt, ok := c.value(op.value, r, oa)
		if !ok {
			return "", "", false
		}
		return guard("textContains(" + r.access() + ", " + celString(v.(string)) + ")"), r.display + " contains " + vt, false
	}
	c.errf(oa, "unknown operator %q; use eq, ne, lt, lte, gt, gte, in, not_in, contains, present or missing", op.key)
	return "", "", false
}

// value reads one comparison value (a scalar or a $setting) as r's type.
func (c *compiler) value(n *yaml.Node, r fieldRef, a at) (v any, text string, ok bool) {
	s, isScalar := scalarOf(n)
	if !isScalar {
		c.errf(a, "needs a single value, not %s", kindName(n))
		return nil, "", false
	}
	text = renderValue(s.natural())
	if name, isSetting := settingName(s); isSetting {
		set, ok := c.settings[name]
		if !ok {
			c.errf(a, "setting $%s is not declared under settings", name)
			return nil, "", false
		}
		if set.list != nil {
			c.errf(a, "setting $%s is a list; use in or not_in", name)
			return nil, "", false
		}
		s, text = set.scalar, "$"+name
	}
	v, err := s.as(r.typ)
	if err != nil {
		c.errf(a, "%s is %s: %v", r.display, r.typ, err)
		return nil, "", false
	}
	if r.kind == rStatus && !validStatus(v.(string)) {
		c.errf(a, "%q is not a status; statuses are %s", s.text, strings.Join(statuses, ", "))
		return nil, "", false
	}
	return v, text, true
}

// values reads an in/not_in list (a YAML list or a $setting list) as r's type.
func (c *compiler) values(n *yaml.Node, r fieldRef, a at) (vs []any, text string, ok bool) {
	var items []scalar
	n = deref(n)
	if s, isScalar := scalarOf(n); isScalar {
		name, isSetting := settingName(s)
		if !isSetting {
			c.errf(a, "needs a list, or a $setting that holds one")
			return nil, "", false
		}
		set, ok := c.settings[name]
		if !ok {
			c.errf(a, "setting $%s is not declared under settings", name)
			return nil, "", false
		}
		if set.list == nil {
			c.errf(a, "setting $%s is a single value, not a list", name)
			return nil, "", false
		}
		items, text = set.list, "$"+name
	} else if n != nil && n.Kind == yaml.SequenceNode {
		var texts []string
		for i, item := range n.Content {
			s, ok := scalarOf(item)
			if !ok {
				c.errf(at{item, a.index(i)}, "list items must be single values")
				return nil, "", false
			}
			items = append(items, s)
			texts = append(texts, renderValue(s.natural()))
		}
		text = "[" + strings.Join(texts, ", ") + "]"
	} else {
		c.errf(a, "needs a list, not %s", kindName(n))
		return nil, "", false
	}
	for _, s := range items {
		v, err := s.as(r.typ)
		if err != nil {
			c.errf(a, "%s is %s: %v", r.display, r.typ, err)
			return nil, "", false
		}
		if r.kind == rStatus && !validStatus(v.(string)) {
			c.errf(a, "%q is not a status; statuses are %s", s.text, strings.Join(statuses, ", "))
			return nil, "", false
		}
		vs = append(vs, v)
	}
	return vs, text, true
}

func settingName(s scalar) (string, bool) {
	if s.tag != "!!str" || !strings.HasPrefix(s.text, "$") {
		return "", false
	}
	return s.text[1:], true
}

func validStatus(s string) bool {
	for _, st := range statuses {
		if normText(s) == st {
			return true
		}
	}
	return false
}

// orderList returns the settings list an ordered field ranks by.
func (c *compiler) orderList(name string, a at) ([]string, bool) {
	set, ok := c.settings[name]
	if !ok || set.list == nil {
		c.errf(a, "ordered comparisons need settings.%s, a list in rank order", name)
		return nil, false
	}
	out := make([]string, len(set.list))
	for i, s := range set.list {
		out[i] = s.text
	}
	return out, true
}

// checkRawRefs checks the fields a raw expression reads, as a structured
// condition's are checked, and records them for Fields(): lead.x, has(lead.x),
// lead["x"], company.x, detector.x and status.
func (c *compiler) checkRawRefs(src string, a at, sc scope) {
	parsed, iss := c.leadEnv.Parse(src)
	if iss.Err() != nil {
		return // cond reports the compile error
	}
	type use struct{ v, name string }
	var uses []use
	seen := map[use]bool{}
	add := func(u use) {
		if !seen[u] {
			seen[u] = true
			uses = append(uses, u)
		}
	}
	celast.PreOrderVisit(parsed.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.SelectKind:
			s := e.AsSelect()
			if s.Operand().Kind() == celast.IdentKind {
				add(use{s.Operand().AsIdent(), s.FieldName()})
			}
		case celast.CallKind:
			call := e.AsCall()
			if call.FunctionName() == operators.Index && len(call.Args()) == 2 &&
				call.Args()[0].Kind() == celast.IdentKind && call.Args()[1].Kind() == celast.LiteralKind {
				if k, ok := call.Args()[1].AsLiteral().Value().(string); ok {
					add(use{call.Args()[0].AsIdent(), k})
				}
			}
		case celast.IdentKind:
			if e.AsIdent() == "status" {
				add(use{"status", ""})
			}
		}
	}))
	for _, u := range uses {
		var name string
		switch u.v {
		case "lead":
			name = u.name
			if d, ok := c.deriveByName[name]; ok && d.company {
				c.errf(a, "%s is company level; write company.%s", name, name)
				continue
			}
		case "company":
			name = "company." + u.name
		case "detector":
			name = "detector." + u.name
		case "status":
			name = "status"
		default:
			continue
		}
		if r, ok := c.resolve(name, sc, a); ok {
			c.allowed(r, sc, a)
		}
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
