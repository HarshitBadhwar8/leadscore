// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package detect evaluates the rubric's detectors: the built-in kinds
// count_in_window, first_seen and change, and kinds a build registers, over
// each lead's and each company's Window events. Windows are (now - window, now]
// in UTC. Event kinds, and the kind names themselves, are compared lowercased.
package detect

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// Built-in kinds.
const (
	KindCount     = "count_in_window"
	KindFirstSeen = "first_seen"
	KindChange    = "change"
)

// Evaluate works out which detectors fired for each lead (lead subject) and
// each company (company subject) and returns them as the evaluator's
// DetectorResults: only fired detectors are listed. leads are the live leads,
// with their company domains. A lead's events are the Window events of its
// whole merge family; a company's are every Window event at its domain, lead
// events included.
//
// A registered kind that is missing from the build, or whose factory fails,
// is skipped (it does not fire) and named in errs.
func Evaluate(specs []rules.DetectorSpec, m *model.Model, leads []api.LeadRef, now time.Time) (rules.DetectorResults, []error) {
	res := rules.DetectorResults{Leads: map[api.LeadID]map[string]bool{}, Companies: map[string]map[string]bool{}}
	if len(specs) == 0 {
		return res, nil
	}
	now = now.UTC()

	byLead := map[api.LeadID][]model.WindowEvent{}
	byDomain := map[string][]model.WindowEvent{}
	keys := make([]string, 0, len(m.WindowEvents))
	for k := range m.WindowEvents {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := m.WindowEvents[model.Key(k)]
		if w.LeadID != "" {
			live := merge.Live(m, w.LeadID)
			byLead[live] = append(byLead[live], w)
		}
		if w.Domain != "" {
			byDomain[w.Domain] = append(byDomain[w.Domain], w)
		}
	}
	domains := map[string]bool{}
	for d := range byDomain {
		domains[d] = true
	}
	for _, l := range leads {
		if l.Domain != "" {
			domains[l.Domain] = true
		}
	}
	for k := range m.CompanyFacts {
		domains[string(k)] = true
	}

	var errs []error
	custom := map[string]api.Detector{}
	for _, s := range specs {
		if builtIn(s.Kind) {
			continue
		}
		f, ok := api.DetectorFactory(s.Kind)
		if !ok {
			errs = append(errs, fmt.Errorf("detector %s has kind %q, which this build does not register; it does not fire", s.Name, s.Kind))
			continue
		}
		d, err := f(s.Params)
		if err != nil {
			errs = append(errs, fmt.Errorf("detector %s: building kind %q: %v; it does not fire", s.Name, s.Kind, err))
			continue
		}
		custom[s.Name] = d
	}

	fire := func(into map[string]map[string]bool, at, name string) {
		if into[at] == nil {
			into[at] = map[string]bool{}
		}
		into[at][name] = true
	}
	for _, s := range specs {
		if !builtIn(s.Kind) && custom[s.Name] == nil {
			continue
		}
		if s.Subject == "company" {
			for d := range domains {
				cf := m.CompanyFacts[model.Key(d)]
				subj := api.Subject{Domain: d}
				if eval(s, custom[s.Name], subj, byDomain[d], cf.FirstSeen, cf, now) {
					fire(res.Companies, d, s.Name)
				}
			}
			continue
		}
		for i := range leads {
			l := &leads[i]
			p := m.People[model.Key(l.ID)]
			cf := m.CompanyFacts[model.Key(l.Domain)]
			subj := api.Subject{Lead: l, Domain: l.Domain}
			if eval(s, custom[s.Name], subj, byLead[l.ID], p.FirstSeen, cf, now) {
				if res.Leads[l.ID] == nil {
					res.Leads[l.ID] = map[string]bool{}
				}
				res.Leads[l.ID][s.Name] = true
			}
		}
	}
	return res, errs
}

func builtIn(kind string) bool {
	switch kind {
	case KindCount, KindFirstSeen, KindChange:
		return true
	}
	return false
}

// eval runs one detector for one subject: its window events, its first-seen
// times by kind, and its company's facts.
func eval(s rules.DetectorSpec, custom api.Detector, subj api.Subject, ws []model.WindowEvent,
	firstSeen map[string]time.Time, cf model.CompanyFact, now time.Time) bool {
	switch s.Kind {
	case KindCount:
		return countInWindow(ws, s.Event, s.Window, now) >= s.Min
	case KindFirstSeen:
		first := firstSeenOf(firstSeen, s.Event)
		return !first.IsZero() && inWindow(first, s.Within, now)
	case KindChange:
		return changed(cf, s.Field, s.Within, s.From, s.To, now)
	}
	evs := make([]api.Event, 0, len(ws))
	for _, w := range ws {
		evs = append(evs, api.Event{ID: api.EventID(w.EventKey), Kind: w.Kind, Domain: w.Domain, At: w.At, Attrs: w.Attrs})
	}
	fired, _ := custom.Evaluate(subj, evs, now)
	return fired
}

// matches reports whether an event kind matches a detector's `event`: equal,
// or, for a pattern ending in `*`, starting with the part before it. Both are
// compared lowercased.
func matches(pattern, kind string) bool {
	p, k := strings.ToLower(pattern), strings.ToLower(kind)
	if pre, ok := strings.CutSuffix(p, "*"); ok {
		return strings.HasPrefix(k, pre)
	}
	return p == k
}

// inWindow reports at in (now - window, now].
func inWindow(at time.Time, window time.Duration, now time.Time) bool {
	return at.After(now.Add(-window)) && !at.After(now)
}

// countInWindow counts the events of a matching kind in (now - window, now].
func countInWindow(ws []model.WindowEvent, event string, window time.Duration, now time.Time) int {
	n := 0
	for _, w := range ws {
		if matches(event, w.Kind) && inWindow(w.At, window, now) {
			n++
		}
	}
	return n
}

// firstSeenOf is the earliest first-seen time over the kinds matching event, or
// the zero time.
func firstSeenOf(firstSeen map[string]time.Time, event string) time.Time {
	var first time.Time
	for kind, t := range firstSeen {
		if matches(event, kind) && !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first
}

// changed reports whether a stored company fact changed within `within`: the
// fact holds a value set in the window (facts.<f>.at) that replaced a
// different one (previous.<f>), matching from and to when given. A fact set
// for the first time is not a change.
func changed(cf model.CompanyFact, field string, within time.Duration, from, to *string, now time.Time) bool {
	cur, ok := cf.Facts[field]
	prev, had := cf.Previous[field]
	if !ok || !had || cur.Value == "" || !inWindow(cur.At, within, now) || sameText(cur.Value, prev.Value) {
		return false
	}
	if from != nil && !sameText(*from, prev.Value) {
		return false
	}
	if to != nil && !sameText(*to, cur.Value) {
		return false
	}
	return true
}

// sameText compares text as the rubric does: trimmed,
// Unicode NFC, ignoring case.
func sameText(a, b string) bool {
	return rules.NormText(a) == rules.NormText(b)
}
