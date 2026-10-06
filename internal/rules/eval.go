package rules

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Result is one evaluation's output.
type Result struct {
	Verdicts map[api.LeadID]api.Verdict
	// Blocked names leads whose sources disagree on a `conflicts` field, with
	// the reason; they still get a verdict.
	Blocked map[api.LeadID]string
	// Lanes holds the ids of the lanes whose `when` holds for each lead,
	// highest priority first (file order on a tie). The built-in lane checks,
	// the ledger and limits are the engine's (S10b), not applied here.
	Lanes map[api.LeadID][]string
	// Warnings (a value that does not parse as its declared type, a raw
	// expression that fails) are given once per run each, for the engine to
	// write to Log. They name fields and rules, never a lead's values.
	Warnings []string
}

// Evaluate runs the rubric over every lead in Input (RFC 6.4, "Evaluation per
// run"): company rollups and company derive blocks once per company, then each
// lead's derive blocks, both halves of its score, and its lanes.
func (r *Rubric) Evaluate(in Input) Result {
	res, _ := r.EvaluateContext(context.Background(), in)
	return res
}

// EvaluateContext is Evaluate stopped by ctx: a cancelled ctx interrupts the
// condition running and returns ctx's error with no result.
func (r *Rubric) EvaluateContext(ctx context.Context, in Input) (Result, error) {
	res := r.run(&evalRun{ctx: ctx, seen: map[string]bool{}}, in)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// evalRun is one evaluation's context and its warnings, each kept once.
type evalRun struct {
	ctx  context.Context
	seen map[string]bool
	list []string
}

func (w *evalRun) add(key, format string, args ...any) {
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.list = append(w.list, fmt.Sprintf(format, args...))
}

// companyState is one company's values and its company-level results.
type companyState struct {
	values  map[string]any
	det     map[string]bool
	reasons map[string]string // derive block name -> its reason
	account float64
	acctWhy []string
}

func (r *Rubric) run(w *evalRun, in Input) Result {
	res := Result{
		Verdicts: map[api.LeadID]api.Verdict{},
		Blocked:  map[api.LeadID]string{},
		Lanes:    map[api.LeadID][]string{},
	}

	// Lead input values, and leads grouped by company oldest first (rollups'
	// `first` takes the first present value in that order).
	leadVals := make([]map[string]any, len(in.Leads))
	byDomain := map[string][]int{}
	for i := range in.Leads {
		l := &in.Leads[i]
		leadVals[i] = r.leadValues(l, w)
		if l.Domain != "" {
			byDomain[l.Domain] = append(byDomain[l.Domain], i)
		}
	}
	for _, idx := range byDomain {
		sort.SliceStable(idx, func(a, b int) bool {
			la, lb := in.Leads[idx[a]], in.Leads[idx[b]]
			if !la.FirstSeenAt.Equal(lb.FirstSeenAt) {
				return la.FirstSeenAt.Before(lb.FirstSeenAt)
			}
			return la.ID < lb.ID
		})
	}

	// Companies: facts, rollups, company derive blocks, account half.
	companies := map[string]*companyState{}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		if w.ctx.Err() != nil {
			return res // EvaluateContext returns the error
		}
		cs := &companyState{
			values:  r.companyValues(domain, in, w),
			det:     r.detectorMap(in.Detectors.Companies[domain], nil),
			reasons: map[string]string{},
		}
		facts := maps.Clone(cs.values)
		for _, ro := range r.rollups {
			if v, ok := r.rollupValue(ro, byDomain[domain], in, leadVals, facts, w); ok {
				cs.values[ro.name] = v
			}
		}
		act := map[string]any{"company": cs.values, "detector": cs.det, "settings": r.settings}
		for _, d := range r.derive {
			if d.company {
				cs.reasons[d.name] = r.deriveInto(d, act, cs.values, w)
			}
		}
		cs.account, cs.acctWhy = r.scoreHalf(r.account, "account", act, w)
		companies[domain] = cs
	}

	// Leads: lead derive blocks, contact half, conflicts, lanes.
	for i := range in.Leads {
		if w.ctx.Err() != nil {
			return res
		}
		l := &in.Leads[i]
		cs := companies[l.Domain]
		cvals := map[string]any{}
		if cs != nil {
			cvals = cs.values
		}
		lv := leadVals[i]
		act := map[string]any{
			"lead":     lv,
			"company":  cvals,
			"detector": r.detectorMap(in.Detectors.Companies[l.Domain], in.Detectors.Leads[l.ID]),
			"status":   l.Status,
			"settings": r.settings,
		}
		v := api.Verdict{RubricVersion: r.version, Values: map[string]any{}}
		for _, d := range r.derive {
			if d.company {
				if cs == nil {
					v.Values[d.name] = nil
					continue
				}
				v.Values[d.name] = cvals[d.name]
				v.Reasons = append(v.Reasons, cs.reasons[d.name])
				continue
			}
			v.Reasons = append(v.Reasons, r.deriveInto(d, act, lv, w))
			v.Values[d.name] = lv[d.name]
		}
		if cs == nil {
			v.Reasons = append(v.Reasons, "no company domain")
		} else {
			v.AccountScore = cs.account
			v.Reasons = append(v.Reasons, cs.acctWhy...)
		}
		var why []string
		v.ContactScore, why = r.scoreHalf(r.contact, "contact", act, w)
		v.Reasons = append(v.Reasons, why...)
		res.Verdicts[l.ID] = v

		var disagree []string
		for _, f := range r.conflicts {
			if slices.Contains(l.ConflictFields, f) {
				disagree = append(disagree, f)
			}
		}
		if len(disagree) > 0 {
			res.Blocked[l.ID] = "sources disagree on " + strings.Join(disagree, ", ")
		}

		res.Lanes[l.ID] = r.matchLanes(act, w)
	}
	res.Warnings = w.list
	return res
}

// leadValues builds the `lead` map: the built-in lead fields from LeadRef, and
// every merged field typed per `fields`. A value that does not parse is absent,
// with a warning once per field.
func (r *Rubric) leadValues(l *api.LeadRef, w *evalRun) map[string]any {
	m := map[string]any{}
	for name, raw := range l.Fields {
		if strings.HasPrefix(name, "company.") {
			continue // company facts reach the company map through CompanyFacts
		}
		t := ftype{kind: kText}
		if f, ok := r.leadFields[name]; ok {
			t = f.typ
		}
		r.put(m, name, t, raw, w)
	}
	set := func(name, v string) {
		if strings.TrimSpace(v) != "" {
			m[name] = strings.TrimSpace(v)
		}
	}
	if len(l.Emails) > 0 {
		set("email", l.Emails[0])
	}
	if len(l.LinkedInURLs) > 0 {
		set("linkedin_url", l.LinkedInURLs[0])
	}
	set("full_name", l.FullName)
	set("title", l.Title)
	m["sources_seen"] = float64(l.SourcesSeen)
	m["receiver_only"] = l.ReceiverOnly
	return m
}

// companyValues builds a company's facts: the built-in facts from
// CompanyFacts, leads_seen, and every Extra fact typed per `fields`.
func (r *Rubric) companyValues(domain string, in Input, w *evalRun) map[string]any {
	m := map[string]any{"domain": domain}
	f, ok := in.Companies[domain]
	if ok {
		for key, raw := range f.Extra {
			name, exact := strings.CutPrefix(key, "company.")
			if _, both := f.Extra["company."+name]; !exact && both {
				continue // an exact company.<name> key wins over <name>
			}
			t := ftype{kind: kText}
			if def, ok := r.companyFields[name]; ok {
				if def.builtin {
					continue // the typed CompanyFacts field wins
				}
				t = def.typ
			}
			r.put(m, name, t, raw, w)
		}
		for name, v := range map[string]string{"name": f.Name, "region": f.Region, "funding_stage": f.FundingStage} {
			if s := strings.TrimSpace(v); s != "" {
				m[name] = s
			}
		}
		if f.Employees != nil {
			m["employees"] = float64(*f.Employees)
		}
	}
	if n, ok := in.LeadsSeen[domain]; ok {
		m["leads_seen"] = float64(n)
	}
	return m
}

// put stores raw as type t, warning once per field when it does not parse.
func (r *Rubric) put(m map[string]any, name string, t ftype, raw string, w *evalRun) {
	v, ok, bad := parseValue(t, raw)
	if bad {
		w.add("parse:"+name, "field %s: a value is not a %s, so it is treated as missing (reported once per run)", name, t)
	}
	if ok {
		m[name] = v
	}
}

// detectorMap is the `detector` variable: every detector, true when it fired
// for the lead (lead subject) or its company (company subject).
func (r *Rubric) detectorMap(company, lead map[string]bool) map[string]bool {
	m := make(map[string]bool, len(r.detectors))
	for _, d := range r.detectors {
		if d.Subject == "company" {
			m[d.Name] = company[d.Name]
		} else {
			m[d.Name] = lead[d.Name]
		}
	}
	return m
}

func (r *Rubric) rollupValue(ro *rollup, idx []int, in Input, leadVals []map[string]any, facts map[string]any, w *evalRun) (any, bool) {
	var count float64
	var best any
	for _, i := range idx {
		l := &in.Leads[i]
		switch ro.op {
		case "any", "all", "count":
			act := map[string]any{
				"lead":     leadVals[i],
				"company":  facts,
				"detector": r.detectorMap(in.Detectors.Companies[l.Domain], in.Detectors.Leads[l.ID]),
				"status":   l.Status,
				"settings": r.settings,
			}
			if ro.when.eval(act, w) {
				count++
			}
		default:
			v, ok := leadVals[i][ro.field.name]
			if !ok {
				continue
			}
			switch {
			case best == nil:
				best = v
			case ro.op == "first":
			case ro.op == "max" && less(best, v), ro.op == "min" && less(v, best):
				best = v
			}
		}
	}
	switch ro.op {
	case "any":
		return count > 0, true
	case "all":
		return count == float64(len(idx)), true
	case "count":
		return count, true
	}
	return best, best != nil
}

func less(a, b any) bool {
	switch x := a.(type) {
	case float64:
		return x < b.(float64)
	case time.Time:
		return x.Before(b.(time.Time))
	}
	return false
}

// deriveInto runs one derive block: the first matching rule wins; `then: null`,
// or no rule matching and no else, is "no value" (the key is removed, so a
// shadowed input column does not show through). It returns the reason.
func (r *Rubric) deriveInto(d *deriveBlock, act map[string]any, into map[string]any, w *evalRun) string {
	for i, rule := range d.rules {
		if rule.when != nil && !rule.when.eval(act, w) {
			continue
		}
		why := "else"
		if rule.when != nil {
			why = fmt.Sprintf("rule %d: %s", i+1, rule.when.text)
		}
		if rule.value == nil {
			delete(into, d.name)
			return fmt.Sprintf("%s: no value (%s)", d.name, why)
		}
		into[d.name] = rule.value
		return fmt.Sprintf("%s = %s (%s)", d.name, renderValue(rule.value), why)
	}
	delete(into, d.name)
	return d.name + ": no value (no rule matched)"
}

// scoreHalf sums one half's rules and says why each point was added.
func (r *Rubric) scoreHalf(rules []*scoreRule, half string, act map[string]any, w *evalRun) (float64, []string) {
	var total float64
	var why []string
	for _, rule := range rules {
		if rule.band != nil {
			vals, _ := act["lead"].(map[string]any)
			if rule.band.company {
				vals, _ = act["company"].(map[string]any)
			}
			v, ok := vals[rule.band.name].(float64)
			if !ok {
				continue
			}
			th, pts, ok := bandPoints(rule.bands, v)
			if !ok || pts == 0 {
				continue
			}
			total += pts
			why = append(why, fmt.Sprintf("%s %s: %s is %s (band %s)", signed(pts), half, rule.band.display, renderValue(v), renderValue(th)))
			continue
		}
		if rule.when.eval(act, w) {
			total += rule.points
			why = append(why, fmt.Sprintf("%s %s: %s", signed(rule.points), half, rule.when.text))
		}
	}
	return total, why
}

func (r *Rubric) matchLanes(act map[string]any, w *evalRun) []string {
	type hit struct {
		id       string
		priority int
	}
	var hits []hit
	for _, l := range r.lanes {
		if l.when == nil || l.when.eval(act, w) {
			hits = append(hits, hit{l.ID, l.Priority})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].priority > hits[j].priority })
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids
}

// eval runs a condition. A failure (only a raw expression can fail, for
// example on a key a lead lacks) counts as false, with a warning once.
func (c *condition) eval(act map[string]any, w *evalRun) bool {
	out, _, err := c.prg.ContextEval(w.ctx, act)
	if err != nil {
		if w.ctx.Err() != nil {
			return false // cancelled; EvaluateContext returns the error
		}
		w.add("eval:"+c.expr, "condition %q failed (%s), so it counts as false", c.text, errorCategory(err))
		return false
	}
	b, ok := out.Value().(bool)
	if !ok {
		w.add("eval:"+c.expr, "condition %q gave a %s, not true or false, so it counts as false", c.text, out.Type().TypeName())
		return false
	}
	return b
}

// errorCategory names a CEL evaluation error without its text, which can
// quote a lead's values.
func errorCategory(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such key"):
		return "it reads a value the lead does not have; test it with has() first"
	case strings.Contains(msg, "cost limit"):
		return "it ran over the cost limit"
	case strings.Contains(msg, "no such overload"), strings.Contains(msg, "no matching overload"):
		return "a value has the wrong type for the operation"
	}
	return "an evaluation error"
}
