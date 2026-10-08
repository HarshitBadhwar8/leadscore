// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// optOutProperty is the contact's "unsubscribed from all email" flag
// (unconfirmed: the name and that "true" is its set value).
const optOutProperty = "hs_email_optout"

// Lookup is the HubSpot Lookup, run just before a push. For the leads it is
// given it reports:
//
//   - an `optout` for every email of a lead whose contact has opted out:
//     each email of the lead's family is read by email, and the lead's own
//     contacts are read too (found by the lead id property, and by the
//     contact ids the ledger holds, in LeadRef.Done, which follow a merge in
//     HubSpot), so an address a salesperson changed still counts;
//   - one deal event per company (lead domain): the strongest stage (won,
//     then open, then lost) among the deals of the company records found
//     through its contacts (or by the domain when none is), the deals
//     carrying the company's domain property, the deals linked to its
//     contacts (unless shown to be another company's), and the company's
//     stored deal
//     (CompanyDealID). A stored deal that no longer exists is `deal_lost`
//     naming it; a company with no deal at all is `deal_lost` with no deal
//     id.
//
// A read that fails, or that could not see every result, fails every lead it
// was for (its `failed` list), and a company any failed read
// touched gets no deal event at all: no company is released on an
// incomplete answer. Only when every lead failed and no opt-out was learned
// is the whole lookup failed.
type Lookup struct{ s settings }

// NewLookup builds the lookup from the sinks.hubspot block.
func NewLookup(cfg api.Config) (*Lookup, error) {
	s, err := parse(cfg)
	if err != nil {
		return nil, fmt.Errorf("hubspot lookup: %w", err)
	}
	return &Lookup{s: s}, nil
}

// lookupRun is one Lookup call's working state.
type lookupRun struct {
	s       settings
	leads   []api.LeadRef
	failed  map[api.LeadID]error
	badDom  map[string]bool
	events  []api.Event
	optOut  map[string]bool  // emails already reported
	byEmail map[string][]int // email -> lead indexes
	byDom   map[string][]int // domain -> lead indexes
	contact map[int][]string // lead index -> contact ids
	first   error            // the first failure, for a whole-lookup error
}

// Lookup reads opt-outs and company deals for the leads.
func (l *Lookup) Lookup(ctx context.Context, leads []api.LeadRef) ([]api.Event, map[api.LeadID]error, error) {
	r := &lookupRun{
		s: l.s, leads: leads, failed: map[api.LeadID]error{}, badDom: map[string]bool{},
		optOut: map[string]bool{}, byEmail: map[string][]int{}, byDom: map[string][]int{}, contact: map[int][]string{},
	}
	for i, lead := range leads {
		for _, e := range cleanEmails(lead.Emails) {
			r.byEmail[e] = append(r.byEmail[e], i)
		}
		if d := domainOf(lead); d != "" {
			r.byDom[d] = append(r.byDom[d], i)
		}
	}
	r.byEmailRead(ctx)
	r.byLeadID(ctx)
	r.byStoredID(ctx)
	r.deals(ctx)

	for d := range r.badDom {
		for _, i := range r.byDom[d] {
			if _, ok := r.failed[leads[i].ID]; !ok {
				r.failed[leads[i].ID] = r.first
			}
		}
	}
	ids := map[api.LeadID]bool{}
	for _, lead := range leads {
		ids[lead.ID] = true
	}
	// Every lead failed: the whole lookup failed, unless it still learned
	// opt-outs, which are returned so none is lost.
	if len(ids) > 0 && len(r.failed) == len(ids) && len(r.events) == 0 {
		return nil, nil, fmt.Errorf("hubspot lookup: every lead failed: %w", r.first)
	}
	return r.events, r.failed, nil
}

func domainOf(l api.LeadRef) string { return normDomain(l.Domain) }

// fail records a failed read for some leads, and fails their companies.
func (r *lookupRun) fail(idx []int, err error) {
	if r.first == nil {
		r.first = err
	}
	for _, i := range idx {
		if _, ok := r.failed[r.leads[i].ID]; !ok {
			r.failed[r.leads[i].ID] = err
		}
		if d := domainOf(r.leads[i]); d != "" {
			r.badDom[d] = true
		}
	}
}

// failDomains fails companies (and so every lead at them).
func (r *lookupRun) failDomains(domains []string, err error) {
	if r.first == nil {
		r.first = err
	}
	for _, d := range domains {
		r.badDom[d] = true
	}
}

func (r *lookupRun) optout(email string) {
	if email == "" || r.optOut[email] {
		return
	}
	r.optOut[email] = true
	r.events = append(r.events, api.Event{Kind: "optout", Email: email})
}

func optedOut(o object) bool {
	return strings.EqualFold(strings.TrimSpace(o.prop(optOutProperty)), "true")
}

// byEmailRead reads every email's contact (batch read by email; an email no
// contact has is simply absent). Unconfirmed: whether a read by email also
// matches a contact's secondary addresses: if an answer comes back under an
// address we did not send, the emails left unmatched in that batch are read
// one at a time, so each answer belongs to the email asked for.
func (r *lookupRun) byEmailRead(ctx context.Context) {
	emails := make([]string, 0, len(r.byEmail))
	for e := range r.byEmail {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	props := []string{"email", optOutProperty}
	for start := 0; start < len(emails); start += batchSize {
		chunk := emails[start:min(start+batchSize, len(emails))]
		found, err := r.s.c.batchRead(ctx, "contacts", "email", chunk, props)
		if err != nil {
			var idx []int
			for _, e := range chunk {
				idx = append(idx, r.byEmail[e]...)
			}
			r.fail(idx, fmt.Errorf("hubspot: reading contacts by email: %w", err))
			continue
		}
		matched := map[string]bool{}
		stray := false
		for _, o := range found {
			e := strings.ToLower(strings.TrimSpace(o.prop("email")))
			if len(r.byEmail[e]) == 0 {
				stray = true
				continue
			}
			matched[e] = true
			r.emailContact(e, o)
		}
		if !stray {
			continue
		}
		for _, e := range chunk {
			if matched[e] {
				continue
			}
			one, err := r.s.c.batchRead(ctx, "contacts", "email", []string{e}, props)
			if err != nil {
				r.fail(r.byEmail[e], fmt.Errorf("hubspot: reading a contact by email: %w", err))
				continue
			}
			for _, o := range one {
				r.emailContact(e, o)
			}
		}
	}
}

// emailContact records the contact found for an email: its id for every
// lead holding the email, and an opt-out under that email.
func (r *lookupRun) emailContact(email string, o object) {
	for _, i := range r.byEmail[email] {
		r.contact[i] = append(r.contact[i], o.ID)
	}
	if optedOut(o) {
		r.optout(email)
	}
}

// leadOptOut reports an opt-out on a contact that is the lead's own (found
// by lead id or by the contact id stored in the ledger), under the lead's
// primary email, whatever address the contact now shows.
func (r *lookupRun) leadOptOut(i int) {
	if es := cleanEmails(r.leads[i].Emails); len(es) > 0 {
		r.optout(es[0])
	} else if len(r.leads[i].LinkedInURLs) > 0 {
		r.events = append(r.events, api.Event{Kind: "optout", LinkedInURL: r.leads[i].LinkedInURLs[0]})
	}
}

// byStoredID reads the contacts the ledger already holds for each lead (the
// contact step's vendor ids, in LeadRef.Done). A contact merged into another
// in HubSpot answers with the surviving contact (unconfirmed), so an
// opt-out on the survivor counts for the lead. An id the batch read does
// not answer under its own id is read on its own; one that no longer
// exists is skipped.
func (r *lookupRun) byStoredID(ctx context.Context) {
	idx := map[string][]int{}
	var ids []string
	for i, lead := range r.leads {
		for _, d := range lead.Done {
			if d.Key.Step != stepContact || d.VendorID == "" {
				continue
			}
			if _, ok := idx[d.VendorID]; !ok {
				ids = append(ids, d.VendorID)
			}
			idx[d.VendorID] = append(idx[d.VendorID], i)
		}
	}
	if len(ids) == 0 {
		return
	}
	ids = sortedIDs(ids)
	props := []string{"email", optOutProperty}
	found, err := r.s.c.batchRead(ctx, "contacts", "", ids, props)
	if err != nil {
		var all []int
		for _, id := range ids {
			all = append(all, idx[id]...)
		}
		r.fail(all, fmt.Errorf("hubspot: reading stored contacts: %w", err))
		return
	}
	answered := map[string]bool{}
	use := func(id string, o object) {
		for _, i := range idx[id] {
			r.contact[i] = append(r.contact[i], o.ID)
			if optedOut(o) {
				r.leadOptOut(i)
			}
		}
	}
	for _, o := range found {
		if _, ok := idx[o.ID]; ok {
			answered[o.ID] = true
			use(o.ID, o)
		}
	}
	for _, id := range ids {
		if answered[id] {
			continue
		}
		var o object
		err := r.s.c.call(ctx, http.MethodGet, "/crm/v3/objects/contacts/"+id+"?properties=email,"+optOutProperty, nil, &o)
		switch {
		case statusOf(err) == http.StatusNotFound:
			// Deleted in HubSpot: nothing to read.
		case err != nil:
			r.fail(idx[id], fmt.Errorf("hubspot: reading a stored contact: %w", err))
		default:
			use(id, o)
		}
	}
}

// byLeadID finds each lead's own contact by the lead id property.
func (r *lookupRun) byLeadID(ctx context.Context) {
	leadProp := r.s.prop("lead_id")
	idx := map[string][]int{}
	var ids []string
	for i, lead := range r.leads {
		id := string(lead.ID)
		if _, ok := idx[id]; !ok {
			ids = append(ids, id)
		}
		idx[id] = append(idx[id], i)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += maxInValues {
		chunk := ids[start:min(start+maxInValues, len(ids))]
		found, err := r.s.c.search(ctx, "contacts",
			[]filterGroup{{Filters: []filter{{PropertyName: leadProp, Operator: "IN", Values: chunk}}}},
			[]string{leadProp, "email", optOutProperty})
		if err != nil {
			var all []int
			for _, id := range chunk {
				all = append(all, idx[id]...)
			}
			r.fail(all, fmt.Errorf("hubspot: finding contacts by lead id: %w", err))
			continue
		}
		for _, o := range found {
			for _, i := range idx[o.prop(leadProp)] {
				r.contact[i] = append(r.contact[i], o.ID)
				if optedOut(o) {
					r.leadOptOut(i)
				}
			}
		}
	}
}

// candidate is one deal a company may have, and where it was found.
type candidate struct {
	id         string
	stored     bool // the company's stored deal (CompanyDealID)
	viaContact bool // linked to a contact at the company: it counts unless there is evidence it is another company's (elsewhere)
}

// deals reads every company's deals and emits one event per company that no
// failed read touched.
func (r *lookupRun) deals(ctx context.Context) {
	domains := make([]string, 0, len(r.byDom))
	for d := range r.byDom {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	if len(domains) == 0 {
		return
	}
	pipes, err := readPipelines(ctx, r.s)
	if err != nil && !errors.Is(err, errConfig) {
		r.failDomains(domains, fmt.Errorf("hubspot: %w", err))
		return
	}

	// The contacts at each company, and every contact id once.
	contactsAt := map[string][]string{}
	var cids []string
	seen := map[string]bool{}
	for _, d := range domains {
		for _, i := range r.byDom[d] {
			for _, c := range r.contact[i] {
				contactsAt[d] = append(contactsAt[d], c)
				if !seen[c] {
					seen[c] = true
					cids = append(cids, c)
				}
			}
		}
	}
	withContacts := func() []string {
		var out []string
		for _, d := range domains {
			if len(contactsAt[d]) > 0 {
				out = append(out, d)
			}
		}
		return out
	}
	cands := map[string][]candidate{}
	add := func(d string, ids ...string) {
		for _, id := range ids {
			cands[d] = append(cands[d], candidate{id: id})
		}
	}
	companiesAt := map[string][]string{}
	if len(cids) > 0 {
		comps, err := r.s.c.associations(ctx, "contacts", "companies", cids)
		if err != nil {
			r.failDomains(withContacts(), fmt.Errorf("hubspot: reading contacts' companies: %w", err))
		}
		cdeals, err2 := r.s.c.associations(ctx, "contacts", "deals", cids)
		if err2 != nil {
			r.failDomains(withContacts(), fmt.Errorf("hubspot: reading contacts' deals: %w", err2))
		}
		for _, d := range domains {
			for _, c := range contactsAt[d] {
				companiesAt[d] = append(companiesAt[d], comps[c]...)
				for _, id := range cdeals[c] {
					cands[d] = append(cands[d], candidate{id: id, viaContact: true})
				}
			}
		}
	}

	// A company no contact led to is looked up by its domain.
	var unlinked []string
	for _, d := range domains {
		if len(companiesAt[d]) == 0 && !r.badDom[d] {
			unlinked = append(unlinked, d)
		}
	}
	for start := 0; start < len(unlinked); start += maxInValues {
		chunk := unlinked[start:min(start+maxInValues, len(unlinked))]
		found, err := r.s.c.search(ctx, "companies",
			[]filterGroup{{Filters: []filter{{PropertyName: "domain", Operator: "IN", Values: chunk}}}}, []string{"domain"})
		if err != nil {
			r.failDomains(chunk, fmt.Errorf("hubspot: finding companies by domain: %w", err))
			continue
		}
		for _, o := range found {
			d := normDomain(o.prop("domain"))
			companiesAt[d] = append(companiesAt[d], o.ID)
		}
	}

	var compIDs []string
	compSeen := map[string]bool{}
	for _, d := range domains {
		for _, c := range companiesAt[d] {
			if !compSeen[c] {
				compSeen[c] = true
				compIDs = append(compIDs, c)
			}
		}
	}
	if len(compIDs) > 0 {
		cdeals, err := r.s.c.associations(ctx, "companies", "deals", compIDs)
		if err != nil {
			var hit []string
			for _, d := range domains {
				if len(companiesAt[d]) > 0 {
					hit = append(hit, d)
				}
			}
			r.failDomains(hit, fmt.Errorf("hubspot: reading companies' deals: %w", err))
		}
		for _, d := range domains {
			for _, c := range companiesAt[d] {
				add(d, cdeals[c]...)
			}
		}
	}

	// The deals the sink opened carry the company's domain.
	domainProp := r.s.prop("company_domain")
	for start := 0; start < len(domains); start += maxInValues {
		chunk := domains[start:min(start+maxInValues, len(domains))]
		found, err := r.s.c.search(ctx, "deals",
			[]filterGroup{{Filters: []filter{{PropertyName: domainProp, Operator: "IN", Values: chunk}}}}, []string{domainProp})
		if err != nil {
			r.failDomains(chunk, fmt.Errorf("hubspot: finding deals by company domain: %w", err))
			continue
		}
		for _, o := range found {
			add(normDomain(o.prop(domainProp)), o.ID)
		}
	}
	for _, d := range domains {
		for _, i := range r.byDom[d] {
			if id := r.leads[i].CompanyDealID; id != "" {
				cands[d] = append(cands[d], candidate{id: id, stored: true})
			}
		}
	}

	var all []string
	for _, d := range domains {
		if r.badDom[d] {
			continue
		}
		for _, c := range cands[d] {
			all = append(all, c.id)
		}
	}
	all = sortedIDs(all)
	read := map[string]object{}
	if len(all) > 0 {
		objs, err := r.s.c.batchRead(ctx, "deals", "", all, []string{"dealstage", "pipeline", domainProp})
		if err != nil {
			var hit []string
			for _, d := range domains {
				if len(cands[d]) > 0 {
					hit = append(hit, d)
				}
			}
			r.failDomains(hit, fmt.Errorf("hubspot: reading deals: %w", err))
		}
		for _, o := range objs {
			read[o.ID] = o
		}
	}

	// The companies of the deals linked to contacts, to tell a deal of
	// another company (the person's old employer) from this one's.
	var linked []string
	for _, d := range domains {
		if r.badDom[d] {
			continue
		}
		for _, c := range cands[d] {
			if c.viaContact {
				linked = append(linked, c.id)
			}
		}
	}
	coDomains, err := dealCompanyDomains(ctx, r.s.c, sortedIDs(linked))
	if err != nil {
		var hit []string
		for _, d := range domains {
			for _, c := range cands[d] {
				if c.viaContact {
					hit = append(hit, d)
					break
				}
			}
		}
		r.failDomains(hit, fmt.Errorf("hubspot: %w", err))
	}

	for _, d := range domains {
		if r.badDom[d] {
			continue
		}
		r.events = append(r.events, companyEvent(d, cands[d], read, pipes, domainProp, coDomains))
	}
}

// companyEvent is the one deal event for a company: the strongest stage
// among its deals (won, then open, then lost), naming that deal; the
// company's stored deal is preferred within a stage. A deal linked to a
// contact at the company counts unless it is shown to be another company's
// (elsewhere). A stored deal that no
// longer exists counts as lost. No deal at all is deal_lost with no deal
// id.
func companyEvent(domain string, cands []candidate, read map[string]object, pipes *pipelines, domainProp string, coDomains map[string][]string) api.Event {
	type pick struct {
		id, stage string
		stored    bool
	}
	best := map[stageClass]*pick{}
	consider := func(c stageClass, p pick) {
		cur := best[c]
		switch {
		case cur == nil, p.stored && !cur.stored:
			best[c] = &p
		case p.stored == cur.stored && lessID(p.id, cur.id) == (c != classLost):
			// Within a stage: the oldest open or won deal, the newest lost one.
			best[c] = &p
		}
	}
	for _, c := range cands {
		o, ok := read[c.id]
		if !ok {
			if c.stored {
				consider(classLost, pick{id: c.id, stored: true}) // deleted
			}
			continue
		}
		if c.viaContact && elsewhere(domain, o.prop(domainProp), coDomains[c.id]) {
			// A deal linked to a contact here but shown to be another
			// company's (the person's old employer). A deal of this
			// company's own company record is a separate candidate.
			continue
		}
		class, _ := pipes.of(o.prop("pipeline"), o.prop("dealstage"))
		consider(class, pick{id: c.id, stage: o.prop("dealstage"), stored: c.stored})
	}
	for _, c := range []stageClass{classWon, classOpen, classLost} {
		if p := best[c]; p != nil {
			attrs := map[string]string{"deal_id": p.id}
			if p.stage != "" {
				attrs["stage"] = p.stage
			}
			return api.Event{Kind: "deal_" + string(c), Domain: domain, Attrs: attrs}
		}
	}
	return api.Event{Kind: "deal_lost", Domain: domain, Attrs: map[string]string{}}
}

func lessID(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
