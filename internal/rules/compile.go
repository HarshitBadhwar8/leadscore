// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // limits.timezone must load in a container with no zone files

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/duration"
)

// rankedColumns are the Ranked table's fixed columns; a
// derived name becomes a column beside them, so it may not take one.
var rankedColumns = map[string]bool{
	"lead_id": true, "email": true, "linkedin_url": true, "full_name": true, "company_domain": true,
	"account_score": true, "contact_score": true, "score": true, "status": true, "lane": true,
	"reasons": true, "rubric_version": true,
}

// maxWindow is the longest detector window: events are kept 90 days.
const maxWindow = 90 * 24 * time.Hour

// setting is one entry of the `settings` block: a list or a single value.
type setting struct {
	list   []scalar // nil for a single value
	scalar scalar
}

// rollup is one entry of the rubric's `company` block.
type rollup struct {
	name  string
	op    string // any, all, count, max, min, first
	when  *condition
	field fieldRef // max, min, first
	typ   ftype
}

// deriveBlock is one entry of `derive`.
type deriveBlock struct {
	name    string
	company bool
	idx     int
	typ     ftype
	rules   []deriveRule
}

type deriveRule struct {
	when  *condition // nil for else
	value any        // nil for "no value"
	whenN *yaml.Node // compile time only: the condition's source
	whenA at
}

// scoreRule is a `when`/`points` rule or a band rule.
type scoreRule struct {
	when   *condition
	points float64
	band   *fieldRef
	bands  map[float64]float64
}

type compiler struct {
	errs     LoadErrors
	warnings LoadErrors
	// rollupNames are the `company` block's names, known before rollups compile.
	rollupNames map[string]bool

	settings       map[string]setting
	leadFields     map[string]*fieldDef
	companyFields  map[string]*fieldDef
	aliases        map[string]string
	aliasAt        map[string]string // squashed alias -> the field that claimed it
	detectorByName map[string]*DetectorSpec
	detectors      []*DetectorSpec
	rollupByName   map[string]*rollup
	rollups        []*rollup
	deriveByName   map[string]*deriveBlock
	derive         []*deriveBlock
	reads          map[string]bool

	leadEnv, companyEnv *cel.Env
}

func (c *compiler) errf(a at, format string, args ...any) {
	c.errs = append(c.errs, LoadError{Line: a.line(), Field: a.path, Msg: fmt.Sprintf(format, args...)})
}

func (c *compiler) warnf(a at, format string, args ...any) {
	c.warnings = append(c.warnings, LoadError{Line: a.line(), Field: a.path, Msg: fmt.Sprintf(format, args...)})
}

// checkTree refuses YAML aliases (`*name`), whose expansion can grow
// exponentially, and a key repeated in one mapping, which YAML would silently
// resolve to the last value.
func (c *compiler) checkTree(n *yaml.Node, path string) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.AliasNode:
		c.errf(at{n, path}, "YAML aliases (*%s) are not allowed in a rubric; write the value out", n.Value)
	case yaml.DocumentNode:
		for _, ch := range n.Content {
			c.checkTree(ch, path)
		}
	case yaml.SequenceNode:
		for i, ch := range n.Content {
			c.checkTree(ch, at{nil, path}.index(i))
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			kp := at{nil, path}.key(k.Value)
			if k.Kind == yaml.AliasNode {
				c.checkTree(k, kp)
				continue
			}
			if seen[k.Value] {
				c.errf(at{k, kp}, "%s appears twice in the same block", k.Value)
			}
			seen[k.Value] = true
			c.checkTree(n.Content[i+1], kp)
		}
	}
}

var topKeys = []string{"version", "fields", "settings", "company", "detectors", "derive", "conflicts", "score", "limits", "lanes"}

// Compile checks a rubric and compiles it. On failure the error is LoadErrors,
// naming the line and field of every problem found.
func Compile(src []byte) (*Rubric, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, LoadErrors{{Msg: "not valid YAML: " + err.Error()}}
	}
	root := deref(&doc)
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = deref(root.Content[0])
	}
	if root == nil || root.Kind != yaml.MappingNode {
		line := 0
		if root != nil {
			line = root.Line
		}
		return nil, LoadErrors{{Line: line, Msg: "a rubric is a mapping with at least version and lanes"}}
	}
	c := &compiler{
		settings:       map[string]setting{},
		leadFields:     builtinLeadFields(),
		companyFields:  builtinCompanyFields(),
		aliases:        map[string]string{},
		aliasAt:        map[string]string{},
		detectorByName: map[string]*DetectorSpec{},
		rollupByName:   map[string]*rollup{},
		deriveByName:   map[string]*deriveBlock{},
		reads:          map[string]bool{},
		rollupNames:    map[string]bool{},
	}
	c.checkTree(&doc, "")
	if len(c.errs) > 0 {
		return nil, c.errs // nothing else is read until aliases and duplicates are gone
	}
	var err error
	if c.leadEnv, err = newEnv(false); err != nil {
		return nil, fmt.Errorf("building the CEL environment: %w", err)
	}
	if c.companyEnv, err = newEnv(true); err != nil {
		return nil, fmt.Errorf("building the CEL environment: %w", err)
	}

	top := map[string]pair{}
	for _, p := range pairs(root) {
		if !slices.Contains(topKeys, p.key) {
			c.errf(at{p.keyN, p.key}, "unknown key; a rubric has %s", strings.Join(topKeys, ", "))
			continue
		}
		top[p.key] = p
	}
	get := func(k string) (*yaml.Node, at) {
		p, ok := top[k]
		if !ok {
			return nil, at{root, k}
		}
		return p.value, at{p.value, k}
	}

	if n, a := get("version"); n == nil {
		c.errf(at{root, ""}, "version is required (version: 1)")
	} else if s, ok := scalarOf(n); !ok || s.natural() != float64(1) {
		c.errf(a, "the only rubric version is 1")
	}
	if _, ok := top["lanes"]; !ok {
		c.errf(at{root, ""}, "lanes is required (it may be an empty list)")
	}

	if n, _ := get("company"); n != nil {
		for _, p := range pairs(n) {
			c.rollupNames[p.key] = true
		}
	}
	c.compileSettings(get("settings"))
	c.compileFields(get("fields"))
	c.compileDetectors(get("detectors"))
	c.scanDerive(get("derive"))
	c.compileRollups(get("company"))
	c.compileDerive()
	conflicts := c.compileConflicts(get("conflicts"))
	account, contact := c.compileScore(get("score"))
	limits := c.compileLimits(get("limits"))
	lanes := c.compileLanes(get("lanes"))

	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, j int) bool { return c.errs[i].Line < c.errs[j].Line })
		return nil, c.errs
	}
	version, err := versionOf(src)
	if err != nil {
		return nil, LoadErrors{{Msg: err.Error()}}
	}
	r := &Rubric{
		version:       version,
		settings:      c.settingValues(),
		leadFields:    c.leadFields,
		companyFields: c.companyFields,
		aliases:       c.aliases,
		rollups:       c.rollups,
		derive:        c.derive,
		conflicts:     conflicts,
		account:       account,
		contact:       contact,
		limits:        limits,
		lanes:         lanes,
		warnings:      c.warnings,
	}
	for _, d := range c.detectors {
		r.detectors = append(r.detectors, *d)
	}
	for f := range c.reads {
		r.reads = append(r.reads, f)
	}
	sort.Strings(r.reads)
	return r, nil
}

// mapping checks n is a mapping (or absent) and returns its entries.
func (c *compiler) mapping(n *yaml.Node, a at) []pair {
	if n == nil || isNull(n) {
		return nil
	}
	if deref(n).Kind != yaml.MappingNode {
		c.errf(a, "must be a mapping, not %s", kindName(n))
		return nil
	}
	return pairs(n)
}

// keysOnly reports unknown keys in a mapping.
func (c *compiler) keysOnly(ps []pair, a at, allowed ...string) {
	for _, p := range ps {
		if !slices.Contains(allowed, p.key) {
			c.errf(at{p.keyN, a.key(p.key)}, "unknown key; allowed here: %s", strings.Join(allowed, ", "))
		}
	}
}

func (c *compiler) name(p pair, a at, what string) bool {
	if !nameRe.MatchString(p.key) {
		c.errf(at{p.keyN, a.key(p.key)}, "%s names use lowercase letters, digits and underscores, starting with a letter", what)
		return false
	}
	return true
}

func (c *compiler) compileSettings(n *yaml.Node, a at) {
	for _, p := range c.mapping(n, a) {
		pa := at{p.value, a.key(p.key)}
		if !c.name(p, a, "setting") {
			continue
		}
		v := deref(p.value)
		switch v.Kind {
		case yaml.SequenceNode:
			list := []scalar{}
			for i, item := range v.Content {
				s, ok := scalarOf(item)
				if !ok || s.tag == "!!null" {
					c.errf(at{item, pa.index(i)}, "list items must be single values")
					continue
				}
				list = append(list, s)
			}
			c.settings[p.key] = setting{list: list}
		case yaml.ScalarNode:
			s, _ := scalarOf(v)
			c.settings[p.key] = setting{scalar: s}
		default:
			c.errf(pa, "a setting is a list or a single value, not %s", kindName(v))
		}
	}
}

// settingValues is the CEL `settings` variable.
func (c *compiler) settingValues() map[string]any {
	out := map[string]any{}
	for name, s := range c.settings {
		if s.list != nil {
			l := make([]any, len(s.list))
			for i, x := range s.list {
				l[i] = x.natural()
			}
			out[name] = l
		} else {
			out[name] = s.scalar.natural()
		}
	}
	return out
}

func (c *compiler) compileFields(n *yaml.Node, a at) {
	for _, p := range c.mapping(n, a) {
		pa := at{p.value, a.key(p.key)}
		if !c.name(p, a, "field") {
			continue
		}
		if p.key == "status" {
			c.errf(pa, "status is the folded status, not a column; it cannot be declared")
			continue
		}
		ps := c.mapping(p.value, pa)
		c.keysOnly(ps, pa, "type", "level", "aliases")
		def := &fieldDef{name: p.key}
		typeSet := false
		for _, q := range ps {
			qa := at{q.value, pa.key(q.key)}
			switch q.key {
			case "type":
				def.typ, typeSet = c.fieldType(q.value, qa)
			case "level":
				s, _ := scalarOf(q.value)
				switch s.text {
				case "lead":
				case "company":
					def.company = true
				default:
					c.errf(qa, "level is lead or company")
				}
			case "aliases":
				v := deref(q.value)
				if v.Kind != yaml.SequenceNode {
					c.errf(qa, "aliases is a list of header spellings")
					continue
				}
				for i, item := range v.Content {
					s, ok := scalarOf(item)
					if !ok || api.SquashHeader(s.text) == "" {
						c.errf(at{item, qa.index(i)}, "an alias is a header spelling with at least one letter or digit")
						continue
					}
					def.aliases = append(def.aliases, s.text)
				}
			}
		}
		table, other := c.leadFields, c.companyFields
		if def.company {
			table, other = c.companyFields, c.leadFields
		}
		if b, ok := other[def.name]; ok && b.builtin {
			lvl := "lead"
			if b.company {
				lvl = "company"
			}
			c.errf(pa, "%s is a built-in %s field; it cannot be declared at the other level", def.name, lvl)
			continue
		}
		if b, ok := table[def.name]; ok && b.builtin {
			// Re-declaring a built-in only adds aliases; its type is fixed.
			if typeSet && def.typ != b.typ {
				c.errf(pa, "%s is a built-in field of type %s", def.name, b.typ)
				continue
			}
			b.aliases = append(b.aliases, def.aliases...)
			def = b
		} else {
			table[def.name] = def
		}
		target := def.name
		if def.company {
			target = "company." + def.name
		}
		builtin := api.BuiltinAliases()
		for _, spelling := range append([]string{def.name}, def.aliases...) {
			sq := api.SquashHeader(spelling)
			if b, ok := builtin[sq]; ok && b != target {
				c.warnf(pa, "header spelling %q usually names %s; in this rubric a column headed so goes to %s instead", spelling, b, target)
			}
			if prev, ok := c.aliasAt[sq]; ok && prev != target {
				c.errf(pa, "header spelling %q (squashed %q) already names %s", spelling, sq, prev)
				continue
			}
			c.aliasAt[sq] = target
			c.aliases[sq] = target
		}
	}
}

func (c *compiler) fieldType(n *yaml.Node, a at) (ftype, bool) {
	n = deref(n)
	if n.Kind == yaml.MappingNode {
		ps := pairs(n)
		if len(ps) != 1 || ps[0].key != "ordered" {
			c.errf(a, "type is text, number, date, bool, or { ordered: <settings list> }")
			return ftype{}, false
		}
		s, _ := scalarOf(ps[0].value)
		name := strings.TrimPrefix(s.text, "$")
		set, ok := c.settings[name]
		if !ok || set.list == nil {
			c.errf(at{ps[0].value, a.key("ordered")}, "settings.%s must be declared as a list", name)
			return ftype{}, false
		}
		return ftype{kind: kOrdered, order: name}, true
	}
	s, _ := scalarOf(n)
	switch s.text {
	case "text":
		return ftype{kind: kText}, true
	case "number":
		return ftype{kind: kNumber}, true
	case "date":
		return ftype{kind: kDate}, true
	case "bool":
		return ftype{kind: kBool}, true
	}
	c.errf(a, "type is text, number, date, bool, or { ordered: <settings list> }")
	return ftype{}, false
}

func (c *compiler) duration(n *yaml.Node, a at) time.Duration {
	s, ok := scalarOf(n)
	if !ok {
		c.errf(a, "needs a duration such as 7d or 12h")
		return 0
	}
	d, err := duration.Parse(s.text)
	if err != nil {
		c.errf(a, "%v", err)
		return 0
	}
	if d <= 0 {
		c.errf(a, "must be longer than zero")
	}
	if d > maxWindow {
		c.errf(a, "%s is longer than 90 days, the longest window events are kept for", s.text)
	}
	return d
}

func (c *compiler) compileDetectors(n *yaml.Node, a at) {
	for _, p := range c.mapping(n, a) {
		pa := at{p.value, a.key(p.key)}
		if !c.name(p, a, "detector") {
			continue
		}
		ps := c.mapping(p.value, pa)
		spec := &DetectorSpec{Name: p.key, Subject: "lead"}
		vals := map[string]pair{}
		for _, q := range ps {
			vals[q.key] = q
		}
		str := func(k string) (string, at, bool) {
			q, ok := vals[k]
			if !ok {
				return "", at{}, false
			}
			s, _ := scalarOf(q.value)
			return s.text, at{q.value, pa.key(k)}, true
		}
		if s, sa, ok := str("subject"); ok {
			if s != "lead" && s != "company" {
				c.errf(sa, "subject is lead or company")
			}
			spec.Subject = s
		}
		kindName, _, ok := str("kind")
		// Kind names compare lowercased, here and in internal/detect: a
		// built-in kind written in another case gets the built-in checks.
		kindName = strings.ToLower(strings.TrimSpace(kindName))
		if !ok || kindName == "" {
			c.errf(pa, "kind is required: count_in_window, first_seen, change, or a registered kind")
			continue
		}
		spec.Kind = kindName
		need := func(k string) (string, at, bool) {
			s, sa, ok := str(k)
			if !ok || s == "" {
				c.errf(pa, "%s needs %s", kindName, k)
				return "", sa, false
			}
			return s, sa, true
		}
		// dur reads a required duration; a value that is not a single one is
		// reported by c.duration, not as missing.
		dur := func(k string) time.Duration {
			q, ok := vals[k]
			if !ok || isNull(q.value) {
				c.errf(pa, "%s needs %s", kindName, k)
				return 0
			}
			return c.duration(q.value, at{q.value, pa.key(k)})
		}
		switch kindName {
		case "count_in_window":
			c.keysOnly(ps, pa, "kind", "subject", "event", "window", "min")
			spec.Event = c.event(need("event"))
			spec.Window = dur("window")
			if s, sa, ok := need("min"); ok {
				m, err := strconv.Atoi(s)
				if err != nil || m < 1 {
					c.errf(sa, "min is a whole number of at least 1")
				}
				spec.Min = m
			}
		case "first_seen":
			c.keysOnly(ps, pa, "kind", "subject", "event", "within")
			spec.Event = c.event(need("event"))
			spec.Within = dur("within")
		case "change":
			c.keysOnly(ps, pa, "kind", "subject", "field", "within", "from", "to")
			if f, fa, ok := need("field"); ok {
				f = strings.TrimPrefix(f, "company.")
				_, known := c.companyFields[f]
				switch {
				case f == "leads_seen" || f == "domain" || c.rollupNames[f]:
					c.errf(fa, "change watches a stored company fact; %s is worked out each run", f)
				case !known && !squashedRe.MatchString(f):
					c.errf(fa, "change watches a company fact; %q is not one", f)
				}
				spec.Field = f
				c.reads["company."+f] = true
			}
			spec.Within = dur("within")
			if s, _, ok := str("from"); ok {
				spec.From = &s
			}
			if s, _, ok := str("to"); ok {
				spec.To = &s
			}
		default:
			// A registered kind; that it is registered is checked at run start.
			c.keysOnly(ps, pa, "kind", "subject", "params")
			if q, ok := vals["params"]; ok {
				var params map[string]any
				if err := q.value.Decode(&params); err != nil || deref(q.value).Kind != yaml.MappingNode {
					c.errf(at{q.value, pa.key("params")}, "params is a mapping")
				}
				spec.Params = params
			}
		}
		c.detectorByName[spec.Name] = spec
		c.detectors = append(c.detectors, spec)
	}
}

// event checks a detector's event kind: `*` may only end it, as a prefix match.
func (c *compiler) event(e string, a at, ok bool) string {
	if ok && strings.Contains(strings.TrimSuffix(e, "*"), "*") {
		c.errf(a, "* may only end an event kind, as in visit_*")
	}
	return e
}

func (c *compiler) compileRollups(n *yaml.Node, a at) {
	for _, p := range c.mapping(n, a) {
		pa := at{p.value, a.key(p.key)}
		if !c.name(p, a, "rollup") {
			continue
		}
		if _, ok := c.companyFields[p.key]; ok {
			c.errf(pa, "%s is already a company field", p.key)
			continue
		}
		if d, ok := c.deriveByName[p.key]; ok && d.company {
			c.errf(pa, "%s is also a company-level derived name", p.key)
			continue
		}
		ps := c.mapping(p.value, pa)
		if len(ps) != 1 {
			c.errf(pa, "a rollup is one of { any: C }, { all: C }, { count: C }, { max: F }, { min: F }, { first: F }")
			continue
		}
		q := ps[0]
		qa := at{q.value, pa.key(q.key)}
		ro := &rollup{name: p.key, op: q.key}
		sc := scope{what: "rollup " + p.key, rollup: true}
		switch q.key {
		case "any", "all", "count":
			ro.when = c.cond(q.value, qa, sc)
			ro.typ = ftype{kind: kBool}
			if q.key == "count" {
				ro.typ = ftype{kind: kNumber}
			}
		case "max", "min", "first":
			s, ok := scalarOf(q.value)
			if !ok || s.text == "" {
				c.errf(qa, "%s needs a lead field name", q.key)
				continue
			}
			r, ok := c.resolve(s.text, sc, qa)
			if !ok {
				continue
			}
			if r.kind != rLead {
				c.errf(qa, "%s reads a lead field; %s is not one", q.key, s.text)
				continue
			}
			if q.key != "first" && r.typ.kind != kNumber && r.typ.kind != kDate {
				c.errf(qa, "%s needs a number or date field; %s is %s", q.key, s.text, r.typ)
				continue
			}
			ro.field, ro.typ = r, r.typ
		default:
			c.errf(qa, "unknown rollup %q; use any, all, count, max, min or first", q.key)
			continue
		}
		c.rollupByName[ro.name] = ro
		c.rollups = append(c.rollups, ro)
	}
}

// scanDerive reads derive block names, levels and value types first, so
// conditions anywhere can resolve them; compileDerive compiles the conditions.
func (c *compiler) scanDerive(n *yaml.Node, a at) {
	for i, p := range c.mapping(n, a) {
		pa := at{p.keyN, a.key(p.key)}
		if !c.name(p, a, "derived") {
			continue
		}
		d := &deriveBlock{name: p.key, idx: i}
		rulesN := deref(p.value)
		ra := pa
		if rulesN.Kind == yaml.MappingNode {
			ps := pairs(rulesN)
			c.keysOnly(ps, pa, "level", "rules")
			rulesN = nil
			for _, q := range ps {
				switch q.key {
				case "level":
					s, _ := scalarOf(q.value)
					switch s.text {
					case "lead":
					case "company":
						d.company = true
					default:
						c.errf(at{q.value, pa.key("level")}, "level is lead or company")
					}
				case "rules":
					rulesN, ra = q.value, at{q.value, pa.key("rules")}
				}
			}
		}
		if rulesN == nil || rulesN.Kind != yaml.SequenceNode || len(rulesN.Content) == 0 {
			c.errf(ra, "a derive block is a non-empty list of rules, or { level, rules }")
			continue
		}
		switch {
		case mergeProduced[d.name]:
			c.errf(pa, "%s is produced by the engine and cannot be derived", d.name)
			continue
		case rankedColumns[d.name]:
			c.errf(pa, "%s is a fixed column of the Ranked table; choose another name", d.name)
			continue
		case d.company && c.companyFields[d.name] != nil:
			c.warnf(pa, "shadows the company field %s from here on", d.name)
		case !d.company && c.leadFields[d.name] != nil:
			c.warnf(pa, "shadows the input field %s from here on", d.name)
		}
		d.idx = len(c.derive)
		var types []string
		for j, rn := range rulesN.Content {
			rn = deref(rn)
			rpa := at{rn, ra.index(j)}
			ps := c.mapping(rn, rpa)
			var valN, whenN *yaml.Node
			keys := map[string]bool{}
			for _, q := range ps {
				keys[q.key] = true
			}
			switch {
			case keys["else"] && len(ps) == 1:
				if j != len(rulesN.Content)-1 {
					c.errf(rpa, "else must be the last rule")
				}
				valN = ps[0].value
			case keys["when"] && keys["then"] && len(ps) == 2:
				for _, q := range ps {
					switch q.key {
					case "then":
						valN = q.value
					case "when":
						whenN = q.value
					}
				}
			default:
				c.errf(rpa, "a rule is { when: C, then: V }, or { else: V } last")
				continue
			}
			s, ok := scalarOf(valN)
			if !ok {
				c.errf(rpa, "then and else take a single value or null")
				continue
			}
			v := s.natural()
			d.rules = append(d.rules, deriveRule{value: v, whenN: whenN, whenA: at{whenN, rpa.key("when")}})
			switch v.(type) {
			case float64:
				d.typ = ftype{kind: kNumber}
				types = append(types, "number")
			case bool:
				d.typ = ftype{kind: kBool}
				types = append(types, "bool")
			case string:
				d.typ = ftype{kind: kText}
				types = append(types, "text")
			}
		}
		for _, t := range types {
			if t != types[0] {
				c.errf(pa, "every value of %s must have one type; got %s", d.name, strings.Join(dedupe(types), " and "))
				break
			}
		}
		c.deriveByName[d.name] = d
		c.derive = append(c.derive, d)
	}
}

// compileDerive compiles every derive condition, once all names are known.
func (c *compiler) compileDerive() {
	for _, d := range c.derive {
		what := "lead block " + d.name
		if d.company {
			what = "company block " + d.name
		}
		sc := scope{what: what, company: d.company, derivedBefore: d.idx}
		for i := range d.rules {
			r := &d.rules[i]
			if r.whenN != nil {
				r.when = c.cond(r.whenN, r.whenA, sc)
			}
			r.whenN = nil
		}
	}
}
