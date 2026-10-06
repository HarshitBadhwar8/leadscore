package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func (c *compiler) compileConflicts(n *yaml.Node, a at) []string {
	if n == nil || isNull(n) {
		return nil
	}
	n = deref(n)
	if n.Kind != yaml.SequenceNode {
		c.errf(a, "conflicts is a list of { field: F }")
		return nil
	}
	var out []string
	for i, item := range n.Content {
		ia := at{deref(item), a.index(i)}
		ps := c.mapping(item, ia)
		if len(ps) != 1 || ps[0].key != "field" {
			c.errf(ia, "a conflicts entry is { field: F }")
			continue
		}
		s, _ := scalarOf(ps[0].value)
		fa := at{ps[0].value, ia.key("field")}
		r, ok := c.resolve(s.text, scope{what: "conflicts", rollup: true}, fa)
		if !ok {
			continue
		}
		if r.kind != rLead && r.kind != rFact {
			c.errf(fa, "conflicts names an input field; %s is not one", s.text)
			continue
		}
		name := r.name
		if r.kind == rFact {
			name = "company." + r.name
		}
		out = append(out, name)
	}
	return out
}

func (c *compiler) compileScore(n *yaml.Node, a at) (account, contact []*scoreRule) {
	ps := c.mapping(n, a)
	c.keysOnly(ps, a, "account", "contact")
	for _, p := range ps {
		pa := at{p.value, a.key(p.key)}
		sc := scope{what: "score.contact", derivedBefore: len(c.derive)}
		if p.key == "account" {
			sc = scope{what: "score.account", company: true, derivedBefore: len(c.derive)}
		}
		list := deref(p.value)
		if isNull(list) {
			continue
		}
		if list.Kind != yaml.SequenceNode {
			c.errf(pa, "%s is a list of rules", p.key)
			continue
		}
		for i, item := range list.Content {
			ia := at{deref(item), pa.index(i)}
			if rule := c.scoreRule(item, ia, sc); rule != nil {
				if p.key == "account" {
					account = append(account, rule)
				} else {
					contact = append(contact, rule)
				}
			}
		}
	}
	return account, contact
}

func (c *compiler) scoreRule(n *yaml.Node, a at, sc scope) *scoreRule {
	ps := c.mapping(n, a)
	vals := map[string]pair{}
	for _, p := range ps {
		vals[p.key] = p
	}
	pts, hasPts := vals["points"]
	if !hasPts {
		c.errf(a, "a score rule is { when: C, points: N } or { band: F, points: { <threshold>: N } }")
		return nil
	}
	pa := at{pts.value, a.key("points")}
	if b, ok := vals["band"]; ok {
		c.keysOnly(ps, a, "band", "points")
		s, _ := scalarOf(b.value)
		ba := at{b.value, a.key("band")}
		r, ok := c.resolve(s.text, sc, ba)
		if !ok || !c.allowed(r, sc, ba) {
			return nil
		}
		if r.typ.kind != kNumber {
			c.errf(ba, "a band reads a number; %s is %s", s.text, r.typ)
			return nil
		}
		bands := map[float64]float64{}
		bp := c.mapping(pts.value, pa)
		if len(bp) == 0 {
			c.errf(pa, "a band's points map thresholds to points, such as { 2: 10, 3: 20 }")
			return nil
		}
		for _, q := range bp {
			th, err := strconv.ParseFloat(q.key, 64)
			if err != nil || math.IsInf(th, 0) || math.IsNaN(th) {
				c.errf(at{q.keyN, pa.key(q.key)}, "a band threshold is a number")
				continue
			}
			if _, dup := bands[th]; dup {
				c.errf(at{q.keyN, pa.key(q.key)}, "threshold %s appears twice", renderValue(th))
				continue
			}
			v, ok := c.number(q.value, at{q.value, pa.key(q.key)})
			if ok {
				bands[th] = v
			}
		}
		return &scoreRule{band: &r, bands: bands}
	}
	c.keysOnly(ps, a, "when", "points")
	w, ok := vals["when"]
	if !ok {
		c.errf(a, "a score rule needs when (or band)")
		return nil
	}
	v, ok := c.number(pts.value, pa)
	cond := c.cond(w.value, at{w.value, a.key("when")}, sc)
	if !ok || cond == nil {
		return nil
	}
	return &scoreRule{when: cond, points: v}
}

func (c *compiler) number(n *yaml.Node, a at) (float64, bool) {
	s, ok := scalarOf(n)
	if ok {
		if f, isNum := s.natural().(float64); isNum {
			return f, true
		}
	}
	c.errf(a, "needs a number")
	return 0, false
}

func (c *compiler) compileLimits(n *yaml.Node, a at) Limits {
	l := Limits{MaxPushesPerRun: DefaultMaxPushesPerRun, MaxPushesPerDay: DefaultMaxPushesPerDay, Timezone: DefaultTimezone}
	ps := c.mapping(n, a)
	c.keysOnly(ps, a, "max_pushes_per_run", "max_pushes_per_day", "timezone")
	for _, p := range ps {
		pa := at{p.value, a.key(p.key)}
		s, _ := scalarOf(p.value)
		switch p.key {
		case "max_pushes_per_run", "max_pushes_per_day":
			v, err := strconv.Atoi(s.text)
			if err != nil || v < 0 {
				c.errf(pa, "a whole number of pushes, zero or more")
				continue
			}
			if p.key == "max_pushes_per_run" {
				l.MaxPushesPerRun = v
			} else {
				l.MaxPushesPerDay = v
			}
		case "timezone":
			l.Timezone = s.text
		}
	}
	loc, err := time.LoadLocation(l.Timezone)
	// "Local" is whatever zone the machine running leadscore has, which
	// differs between a laptop and Google Cloud; it is refused.
	if err != nil || l.Timezone == "" || l.Timezone == "Local" {
		c.errf(at{n, a.key("timezone")}, "unknown timezone %q; use an IANA name such as UTC or Asia/Kolkata", l.Timezone)
		return l
	}
	l.Location = loc
	return l
}

// laneIDRe keeps lane ids safe as table names and file names (`Export <lane
// id>`, `<export.dir>/<lane id>.csv`).
var laneIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// sinkRe is a sink type name.
var sinkRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func (c *compiler) compileLanes(n *yaml.Node, a at) []Lane {
	if n == nil || isNull(n) {
		return nil
	}
	n = deref(n)
	if n.Kind != yaml.SequenceNode {
		c.errf(a, "lanes is a list")
		return nil
	}
	var out []Lane
	ids := map[string]bool{}
	for i, item := range n.Content {
		ia := at{deref(item), a.index(i)}
		ps := c.mapping(item, ia)
		c.keysOnly(ps, ia, "id", "name", "kind", "priority", "when", "push")
		vals := map[string]pair{}
		for _, p := range ps {
			vals[p.key] = p
		}
		str := func(k string) (string, at) {
			p, ok := vals[k]
			if !ok {
				return "", at{deref(item), ia.key(k)}
			}
			s, _ := scalarOf(p.value)
			return s.text, at{p.value, ia.key(k)}
		}
		var l Lane
		var la at
		l.ID, la = str("id")
		switch {
		case l.ID == "":
			c.errf(ia, "every lane needs an id, which must never change")
		case !laneIDRe.MatchString(l.ID):
			c.errf(la, "a lane id uses letters, digits, hyphens and underscores")
		case ids[strings.ToLower(l.ID)]:
			// Ids name tabs, tables and files, which ignore case in places.
			c.errf(la, "lane id %q is used twice (ignoring case)", l.ID)
		}
		ids[strings.ToLower(l.ID)] = true
		l.Name, _ = str("name")
		if l.Name == "" {
			l.Name = l.ID
		}
		var ka at
		l.Kind, ka = str("kind")
		if l.Kind != "cold" && l.Kind != "non-cold" && l.Kind != "export" {
			c.errf(ka, "kind is cold, non-cold or export")
		}
		if p, ok := vals["priority"]; ok {
			s, _ := scalarOf(p.value)
			v, err := strconv.Atoi(s.text)
			if err != nil {
				c.errf(at{p.value, ia.key("priority")}, "priority is a whole number; higher wins")
			}
			l.Priority = v
		}
		var pa at
		l.Push, pa = str("push")
		l.Sink, l.Dest, _ = strings.Cut(l.Push, ":")
		if msg := checkPush(l); msg != "" {
			c.errf(pa, "%s", msg)
		}
		w, ok := vals["when"]
		if !ok {
			c.errf(ia, "every lane needs when: the condition a lead must meet")
		} else {
			wa := at{w.value, ia.key("when")}
			l.when = c.cond(w.value, wa, scope{what: "lane " + l.ID, derivedBefore: len(c.derive)})
			if l.when != nil {
				l.When = l.when.text
			}
			if l.Kind == "cold" && !requiresNotReceiverOnly(w.value) {
				c.warnf(wa, "cold lane %s does not require receiver_only to be false; a lead known only from webhooks (which anyone with the receiver secret can forge) could be cold-contacted. Add { field: receiver_only, eq: false } to its all: list", l.ID)
			}
		}
		out = append(out, l)
	}
	return out
}

// requiresNotReceiverOnly reports whether a condition is, or is an `all` that
// includes, { field: receiver_only, eq: false } (or ne: true).
func requiresNotReceiverOnly(n *yaml.Node) bool {
	ps := pairs(n)
	vals := map[string]*yaml.Node{}
	for _, p := range ps {
		vals[p.key] = p.value
	}
	if f, ok := scalarOf(vals["field"]); ok && f.text == "receiver_only" && len(ps) == 2 {
		if v, ok := scalarOf(vals["eq"]); ok && v.natural() == false {
			return true
		}
		if v, ok := scalarOf(vals["ne"]); ok && v.natural() == true {
			return true
		}
	}
	if all := deref(vals["all"]); all != nil && all.Kind == yaml.SequenceNode && len(ps) == 1 {
		return slices.ContainsFunc(all.Content, requiresNotReceiverOnly)
	}
	return false
}

// checkPush checks a lane's push target (contracts section 2, "Lanes"). That
// the sink is registered is checked at run start, not here.
func checkPush(l Lane) string {
	if l.Push == "" {
		return "push is required: <sink>:<destination>"
	}
	if !strings.Contains(l.Push, ":") || l.Dest == "" || !sinkRe.MatchString(l.Sink) {
		return fmt.Sprintf("push %q is not <sink>:<destination>", l.Push)
	}
	validKind := l.Kind == "cold" || l.Kind == "non-cold" || l.Kind == "export"
	if validKind && (l.Sink == "export") != (l.Kind == "export") {
		return "export lanes, and only export lanes, push to export:<name>"
	}
	switch l.Sink {
	case "apollo":
		name, ok := strings.CutPrefix(l.Dest, "sequence/")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Sprintf("unknown destination %q: Apollo lanes push to apollo:sequence/<sequence name>", l.Push)
		}
	case "hubspot":
		if l.Dest != "contacts" && l.Dest != "deals" {
			return fmt.Sprintf("unknown destination %q: HubSpot lanes push to hubspot:contacts or hubspot:deals", l.Push)
		}
	}
	return ""
}

// versionOf is `r-` plus the first 16 hex characters of the SHA-256 of the
// YAML re-marshalled with sorted keys and no comments, so it changes exactly
// when the rubric's content does.
func versionOf(src []byte) (string, error) {
	var v any
	if err := yaml.Unmarshal(src, &v); err != nil {
		return "", err
	}
	norm, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("normalizing the rubric for its version: %w", err)
	}
	sum := sha256.Sum256(norm)
	return "r-" + hex.EncodeToString(sum[:])[:16], nil
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
