// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package merge

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// ApplyCtx is what Apply needs from the run.
type ApplyCtx struct {
	Now     time.Time
	RunID   string
	Sources []config.Source // channel, apollo_held and match_domain_name per source id
	// Aliases are the rubric's aliases (the same map given to Normalize); they
	// resolve the Companies tab's headers to fact names.
	Aliases map[string]string
}

// Company fact origins, highest first.
const (
	OriginCompaniesTab = "companies_tab"
	OriginEnrichment   = "enrichment"
	OriginInput        = "input"
)

// Log kinds merge writes.
const (
	LogRowRejected = "row_rejected"
	LogKeyConflict = "key_conflict"
	LogMerged      = "merged"
)

// inputFacts maps the built-in company columns of an input row to the fact
// names Company facts uses.
var inputFacts = map[string]string{
	"company.name":          "name",
	"company.employees":     "employees",
	"company.funding_stage": "funding_stage",
	"company.region":        "region",
}

// Apply merges rows into the model, in order. Rows are applied by
// row group (Group): every row of a source that shares a row id, as one unit
// under one hash, so a group applied before and unchanged is skipped. Each row
// of a changed group is matched to a lead, or makes a new one, and fills that
// lead's empty fields. Apply then applies the Companies tab, the Overrides
// `same_as` merges, and marks every lead an
// `apollo_held` source supplied.
//
// Matching, in order: the same source and row id; the row's email; with no
// email, its LinkedIn URL; with neither matching and the source's
// match_domain_name on, the company domain and normalized full name when
// exactly one live lead has them. A row whose email matches no lead is a new
// lead even when its LinkedIn URL belongs to someone: one wrong URL must never
// join two people.
//
// Pass every row Normalize returned, rejected ones included: a reject is
// recorded here, once per group version, with no lead.
func Apply(m *model.Model, rows []Normalized, c ApplyCtx) {
	a := &applier{m: m, c: c, src: map[string]config.Source{}}
	for _, s := range c.Sources {
		a.src[s.ID] = s
	}
	for _, g := range Group(rows) {
		a.group(g)
	}
	a.companiesTab()
	a.sameAs()
	a.apolloHeld()
}

type applier struct {
	m   *model.Model
	c   ApplyCtx
	src map[string]config.Source
}

func (a *applier) log(level, kind string, lead api.LeadID, msg string) {
	a.m.Put(model.TableLog, model.LogEntry{At: a.c.Now, RunID: a.c.RunID, Level: level, LeadID: lead, Kind: kind, Message: msg})
}

// group applies one row group and records it in Applied rows.
func (a *applier) group(g RowGroup) {
	prev, had := a.m.AppliedRows[model.K(g.SourceID, g.RowID)]
	if had && prev.RowHash == g.Hash {
		return // applied before, unchanged
	}
	first := a.c.Now
	if had && !prev.FirstAppliedAt.IsZero() {
		first = prev.FirstAppliedAt
	}
	// A group that applied before keeps its lead, even when every row is now
	// rejected, so the lead keeps everything it had and the same row id still
	// finds it.
	lead := prev.LeadID
	var rejects []string
	conflict := false
	for _, r := range g.Rows {
		if reason := a.rejectReason(r); reason != "" {
			rejects = append(rejects, reason)
			continue
		}
		var c bool
		lead, c = a.row(r, lead)
		conflict = conflict || c
	}
	ar := model.AppliedRow{SourceID: g.SourceID, RowID: g.RowID, RowHash: g.Hash, LeadID: lead,
		FirstAppliedAt: first, KeyConflictAt: prev.KeyConflictAt}
	countIt := conflict && ar.KeyConflictAt.IsZero()
	if countIt {
		ar.KeyConflictAt = a.c.Now
	}
	a.m.Put(model.TableAppliedRows, ar)
	if len(rejects) > 0 {
		a.log("warn", LogRowRejected, "", fmt.Sprintf("source %s: %d row(s) rejected: %s (row hash %s)",
			g.SourceID, len(rejects), strings.Join(rejects, "; "), shortHash(g.Hash)))
	}
	if countIt {
		// Counted once per source and row id, so re-applying the row (an edit,
		// an alias change) does not inflate the count.
		countKeyConflict(a.m)
		a.log("warn", LogKeyConflict, lead, fmt.Sprintf("source %s: a row's email or LinkedIn URL belongs to another lead, or differs from this lead's; the key was not written (row hash %s)",
			g.SourceID, shortHash(g.Hash)))
	}
}

// rejectReason is why a row cannot be applied, or empty.
func (a *applier) rejectReason(r Normalized) string {
	if r.Reject != "" {
		return r.Reject
	}
	if r.Fields[FieldEmail] == "" && r.Fields[FieldLinkedIn] == "" && !a.source(r.SourceID).MatchDomainName {
		return "no email or LinkedIn URL, and domain + name matching is off for this source"
	}
	return ""
}

// row applies one valid row. known is the lead its group already resolved to
// (the same source and row id), or empty. It returns the row's lead and
// whether one of its keys conflicted.
func (a *applier) row(r Normalized, known api.LeadID) (api.LeadID, bool) {
	src := a.source(r.SourceID)
	email, li := r.Fields[FieldEmail], r.Fields[FieldLinkedIn]
	domain, name := r.Fields[FieldDomain], NormalizeName(r.Fields[FieldFullName])

	// Match.
	var lead api.LeadID
	sameRow := false
	switch {
	case known != "":
		lead, sameRow = Live(a.m, known), true
	case email != "":
		if idn, ok := a.m.Identities[model.Key(email)]; ok {
			lead = Live(a.m, idn.LeadID)
		}
	default:
		if li != "" {
			if idn, ok := a.m.Identities[model.Key(li)]; ok {
				lead = Live(a.m, idn.LeadID)
			}
		}
		if lead == "" && src.MatchDomainName && domain != "" && name != "" {
			lead = a.namesake(domain, name)
		}
	}

	p, exists := a.m.People[model.Key(lead)]
	if !exists {
		lead = newLeadID()
		p = model.Person{LeadID: lead, CreatedAt: a.c.Now}
	}
	p = clonePerson(p)
	conflict := false

	// Every column but the identity keys fills if empty.
	names := make([]string, 0, len(r.Fields))
	for n := range r.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == FieldEmail || n == FieldLinkedIn || n == FieldDomain {
			continue
		}
		fill(&p, n, model.Field{Value: r.Fields[n], SourceID: r.SourceID, At: a.c.Now})
	}

	// Email.
	if email != "" {
		idn, known := a.m.Identities[model.Key(email)]
		switch {
		case !known:
			a.m.Put(model.TableIdentities, model.Identity{Key: email, Kind: "email", LeadID: lead, SourceID: r.SourceID, FirstSeenAt: a.c.Now})
			cur := p.Fields[FieldEmail].Value
			if cur == "" || (sameRow && r.SourceID == ReceiverSource && !a.vouched(lead, cur, r)) {
				// The one allowed overwrite: a source keyed by a vendor contact id
				// correcting the address it gave, when no other source gave it.
				p.Fields[FieldEmail] = model.Field{Value: email, SourceID: r.SourceID, At: a.c.Now}
			}
		case Live(a.m, idn.LeadID) == lead:
			fill(&p, FieldEmail, model.Field{Value: email, SourceID: r.SourceID, At: a.c.Now})
		default:
			conflict = true // the address belongs to another lead; it is not written
		}
	}

	// LinkedIn URL: written only when no other lead holds it and the lead has
	// no other one.
	if li != "" {
		idn, known := a.m.Identities[model.Key(li)]
		switch {
		case known && Live(a.m, idn.LeadID) == lead:
			fill(&p, FieldLinkedIn, model.Field{Value: li, SourceID: r.SourceID, At: a.c.Now})
		case known, a.hasLinkedIn(lead, p):
			conflict = true
		default:
			a.m.Put(model.TableIdentities, model.Identity{Key: li, Kind: "linkedin", LeadID: lead, SourceID: r.SourceID, FirstSeenAt: a.c.Now})
			fill(&p, FieldLinkedIn, model.Field{Value: li, SourceID: r.SourceID, At: a.c.Now})
		}
	}

	// Company domain: the row's own, else one derived from a work email. A
	// lead's domain, once set, is never re-pointed.
	rowDomain := domain
	if domain != "" {
		fill(&p, FieldDomain, model.Field{Value: domain, SourceID: r.SourceID, At: a.c.Now})
	} else if d, ok := DeriveCompanyDomain(email); ok {
		rowDomain = d
		if p.Fields[FieldDomain].Value == "" {
			p.Fields[FieldDomain] = model.Field{Value: d, SourceID: r.SourceID, At: a.c.Now, Derived: true}
		}
	}

	if src.ApolloHeld && p.ApolloHeldAt.IsZero() {
		p.ApolloHeldAt = a.c.Now
	}
	a.m.Put(model.TablePeople, p)

	// Company facts from the row's built-in company columns go to the lead's
	// company, and only when the row speaks for that company: a sighting that
	// carries a different domain describes somewhere this lead does not work.
	if d := p.Fields[FieldDomain].Value; d != "" && (rowDomain == "" || rowDomain == d) {
		for col, fact := range inputFacts {
			if v := r.Fields[col]; v != "" {
				a.inputFact(d, fact, v)
			}
		}
	}
	return lead, conflict
}

// source returns a source's settings; an unconfigured one (the receiver, or a
// source since removed) has its id as channel and every flag off.
func (a *applier) source(id string) config.Source {
	if s, ok := a.src[id]; ok {
		return s
	}
	return config.Source{ID: id, Channel: id}
}

// namesake finds the one live lead at a domain with the same normalized full
// name. When several match (namesakes, even a pair marked distinct) the row
// could be any of them, so it matches none and makes a new lead, which
// Duplicates then blocks until a person resolves it.
func (a *applier) namesake(domain, name string) api.LeadID {
	var found api.LeadID
	for _, id := range a.m.PeopleAt(domain) {
		p := a.m.People[model.Key(id)]
		if p.MergedInto != "" || NormalizeName(p.Fields[FieldFullName].Value) != name {
			continue
		}
		if found != "" {
			return ""
		}
		found = id
	}
	return found
}

// older reports whether a was created before b (ties: the lower lead id).
func older(a, b model.Person) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.LeadID < b.LeadID
}

// hasLinkedIn reports whether the lead already has a LinkedIn URL: its field
// (which a same_as merge fills from the absorbed lead) or its own identities.
func (a *applier) hasLinkedIn(lead api.LeadID, p model.Person) bool {
	if p.Fields[FieldLinkedIn].Value != "" {
		return true
	}
	for _, idn := range a.m.IdentitiesOf(lead) {
		if idn.Kind == "linkedin" {
			return true
		}
	}
	return false
}

// vouched reports whether a source other than this row may have reported the
// lead's current email, which makes it not this row's to correct. A row from a
// CSV or Sheet source is keyed by its email, so it vouched exactly when its row
// id is that email. Another receiver contact cannot be told apart, so it counts
// as vouching, as does the email having first come from another source.
func (a *applier) vouched(lead api.LeadID, email string, r Normalized) bool {
	if idn, ok := a.m.Identities[model.Key(email)]; ok && idn.SourceID != r.SourceID {
		return true
	}
	for _, ar := range a.m.AppliedRows {
		if ar.LeadID == "" || Live(a.m, ar.LeadID) != lead || (ar.SourceID == r.SourceID && ar.RowID == r.RowID) {
			continue
		}
		if ar.SourceID == ReceiverSource || ar.RowID == email {
			return true
		}
	}
	return false
}

// fill sets a field when the lead has no value for it. A different non-empty
// value from another source is kept in conflicts; the kept value never
// changes.
func fill(p *model.Person, name string, f model.Field) {
	cur := p.Fields[name]
	if cur.Value == "" {
		p.Fields[name] = f
		return
	}
	if cur.Value == f.Value || cur.SourceID == f.SourceID {
		return
	}
	addConflict(p, name, model.Conflict{Value: f.Value, SourceID: f.SourceID})
}

func addConflict(p *model.Person, name string, c model.Conflict) {
	for _, x := range p.Conflicts[name] {
		if x == c {
			return
		}
	}
	p.Conflicts[name] = append(p.Conflicts[name], c)
}

// clonePerson copies a person's maps, so changing it does not change the row
// the model holds.
func clonePerson(p model.Person) model.Person {
	f := make(map[string]model.Field, len(p.Fields))
	for k, v := range p.Fields {
		f[k] = v
	}
	c := make(map[string][]model.Conflict, len(p.Conflicts))
	for k, v := range p.Conflicts {
		c[k] = append([]model.Conflict(nil), v...)
	}
	s := make(map[string]time.Time, len(p.FirstSeen))
	for k, v := range p.FirstSeen {
		s[k] = v
	}
	p.Fields, p.Conflicts, p.FirstSeen = f, c, s
	return p
}

func cloneCompany(c model.CompanyFact) model.CompanyFact {
	f := make(map[string]model.Fact, len(c.Facts))
	for k, v := range c.Facts {
		f[k] = v
	}
	pr := make(map[string]model.Fact, len(c.Previous))
	for k, v := range c.Previous {
		pr[k] = v
	}
	c.Facts, c.Previous = f, pr
	return c
}

// inputFact sets a company fact from an input row: only when the company has
// no value for it from any origin, so the first input value stands and a
// Companies tab or enrichment value is never replaced.
func (a *applier) inputFact(domain, fact, v string) {
	cf, ok := a.m.CompanyFacts[model.Key(domain)]
	if ok && cf.Facts[fact].Value != "" {
		return
	}
	cf = cloneCompany(cf)
	cf.Domain = domain
	cf.Facts[fact] = model.Fact{Value: v, Origin: OriginInput, At: a.c.Now}
	a.m.Put(model.TableCompanyFacts, cf)
}

// companiesTab applies the people-owned Companies tab, the highest origin. A
// header that resolves to a built-in or rubric company field names that fact
// (`Headcount` is `employees`); any other header names the fact by its
// squashed form. A changed value moves the old one to `previous`; the same
// value from a lower origin is taken over without counting as a change. Only
// the first row for a domain counts, so two rows cannot flip a fact each run.
// A companies_tab fact whose cell was emptied, or whose row is gone, moves to
// `previous` and leaves `facts`, so a lower origin can fill it again.
func (a *applier) companiesTab() {
	table := AliasTable(a.c.Aliases)
	tab := map[string]map[string]string{} // domain -> fact -> value
	var order []string
	for _, row := range a.m.Companies {
		domain := ""
		cols := make([]string, 0, len(row))
		for h := range row {
			cols = append(cols, h)
		}
		sort.Strings(cols)
		facts := map[string]string{}
		for _, h := range cols {
			v := strings.TrimSpace(row[h])
			name := ResolveHeader(table, h)
			switch {
			case name == FieldDomain:
				if domain == "" || api.SquashHeader(h) == "domain" {
					domain = NormalizeDomain(v)
				}
				continue
			case strings.HasPrefix(name, "company."):
				name = strings.TrimPrefix(name, "company.")
			default:
				name = api.SquashHeader(h)
			}
			if name != "" && v != "" {
				if _, dup := facts[name]; !dup {
					facts[name] = v
				}
			}
		}
		if domain == "" {
			continue
		}
		if _, seen := tab[domain]; seen {
			continue
		}
		tab[domain] = facts
		order = append(order, domain)
	}
	for _, domain := range order {
		cf := cloneCompany(a.m.CompanyFacts[model.Key(domain)])
		cf.Domain = domain
		for name, v := range tab[domain] {
			cur, has := cf.Facts[name]
			switch {
			case has && cur.Value == v:
				cur.Origin = OriginCompaniesTab
				cf.Facts[name] = cur
			case has:
				cf.Previous[name] = cur
				cf.Facts[name] = model.Fact{Value: v, Origin: OriginCompaniesTab, At: a.c.Now}
			default:
				cf.Facts[name] = model.Fact{Value: v, Origin: OriginCompaniesTab, At: a.c.Now}
			}
		}
		if len(cf.Facts) > 0 {
			a.m.Put(model.TableCompanyFacts, cf)
		}
	}
	// Facts the tab no longer holds.
	keys := make([]string, 0, len(a.m.CompanyFacts))
	for k := range a.m.CompanyFacts {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		cf := a.m.CompanyFacts[model.Key(k)]
		var gone []string
		for name, f := range cf.Facts {
			if _, still := tab[cf.Domain][name]; f.Origin == OriginCompaniesTab && !still {
				gone = append(gone, name)
			}
		}
		if len(gone) == 0 {
			continue
		}
		cf = cloneCompany(cf)
		for _, name := range gone {
			cf.Previous[name] = cf.Facts[name]
			delete(cf.Facts, name)
		}
		a.m.Put(model.TableCompanyFacts, cf)
	}
}

// sameAs applies every Overrides `same_as` row whose two persons are known
// and are not yet one lead. Merges are permanent.
func (a *applier) sameAs() {
	for _, o := range ParseOverrides(a.m).Rows {
		if o.Action != ActionSameAs || o.Lead == "" || o.Other == "" {
			continue
		}
		x, cx := live(a.m, o.Lead)
		y, cy := live(a.m, o.Other)
		if x == y || cx || cy {
			continue // one lead already, or a hand-edited cycle that blocks both until fixed
		}
		px, okx := a.m.People[model.Key(x)]
		py, oky := a.m.People[model.Key(y)]
		if !okx || !oky {
			continue
		}
		survivor, absorbed := px, py
		if older(py, px) {
			survivor, absorbed = py, px
		}
		a.mergeLeads(clonePerson(survivor), clonePerson(absorbed))
	}
}

// mergeLeads folds absorbed into survivor: survivor fields fill if empty with
// disagreements added to conflicts, apollo_held_at and first_seen take the
// earliest values, and absorbed gets merged_into. Absorbed keeps its
// identities, outcomes and ledger rows under its own id; they count for the
// survivor through merged_into.
func (a *applier) mergeLeads(s, x model.Person) {
	names := make([]string, 0, len(x.Fields))
	for n := range x.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := x.Fields[n]
		cur := s.Fields[n]
		switch {
		case cur.Value == "":
			s.Fields[n] = f
		case cur.Value != f.Value:
			addConflict(&s, n, model.Conflict{Value: f.Value, SourceID: f.SourceID})
		}
	}
	for n, cs := range x.Conflicts {
		for _, c := range cs {
			addConflict(&s, n, c)
		}
	}
	if !x.ApolloHeldAt.IsZero() && (s.ApolloHeldAt.IsZero() || x.ApolloHeldAt.Before(s.ApolloHeldAt)) {
		s.ApolloHeldAt = x.ApolloHeldAt
	}
	for k, t := range x.FirstSeen {
		if cur, ok := s.FirstSeen[k]; !ok || t.Before(cur) {
			s.FirstSeen[k] = t
		}
	}
	x.MergedInto = s.LeadID
	a.m.Put(model.TablePeople, s)
	a.m.Put(model.TablePeople, x)
	// Leads absorbed into x earlier now point at the survivor directly, so every
	// chain stays one step deep however many merges follow.
	var earlier []string
	for k, p := range a.m.People {
		if p.MergedInto == x.LeadID {
			earlier = append(earlier, string(k))
		}
	}
	sort.Strings(earlier)
	for _, k := range earlier {
		p := clonePerson(a.m.People[model.Key(k)])
		p.MergedInto = s.LeadID
		a.m.Put(model.TablePeople, p)
	}
	a.log("info", LogMerged, s.LeadID, fmt.Sprintf("lead %s merged into %s by an Overrides same_as row", x.LeadID, s.LeadID))
}

// apolloHeld marks every lead an apollo_held source supplied, including leads
// from rows applied before the flag was switched on.
func (a *applier) apolloHeld() {
	held := map[string]bool{}
	for _, s := range a.c.Sources {
		if s.ApolloHeld {
			held[s.ID] = true
		}
	}
	if len(held) == 0 {
		return
	}
	for _, ar := range a.m.AppliedRows {
		if ar.LeadID == "" || !held[ar.SourceID] {
			continue
		}
		id := Live(a.m, ar.LeadID)
		p, ok := a.m.People[model.Key(id)]
		if !ok || !p.ApolloHeldAt.IsZero() {
			continue
		}
		p = clonePerson(p)
		p.ApolloHeldAt = a.c.Now
		a.m.Put(model.TablePeople, p)
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
