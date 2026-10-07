package hubspot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// optOutProperty is the contact's "unsubscribed from all email" flag (S0
// confirms the name and that "true" is its set value).
const optOutProperty = "hs_email_optout"

// Lookup is the HubSpot Lookup (RFC 6.9 step 8, contracts sections 1, 5.3
// and 6). For the leads it is given it reports:
//
//   - an `optout` for every email of a lead whose contact has opted out:
//     each email of the lead's family is read by email, and the lead's own
//     contact (found by the lead id property) is read too, so an address a
//     salesperson changed in HubSpot still counts;
//   - one deal event per company (lead domain): the strongest stage (won,
//     then open, then lost) among the deals associated with the contacts at
//     the company, with the company records found through those contacts
//     (or by the domain when none is), with the deals carrying the company's
//     domain property, and with the company's stored deal (CompanyDealID). A
//     stored deal that no longer exists is `deal_lost` naming it; a company
//     with no deal at all is `deal_lost` with no deal id.
//
// A read that fails, or that could not see every result, fails every lead it
// was for (contracts section 1, `failed`), and a company any failed read
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

func domainOf(l api.LeadRef) string { return strings.ToLower(strings.TrimSpace(l.Domain)) }

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
// contact has is simply absent).
func (r *lookupRun) byEmailRead(ctx context.Context) {
	emails := make([]string, 0, len(r.byEmail))
	for e := range r.byEmail {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	for start := 0; start < len(emails); start += batchSize {
		chunk := emails[start:min(start+batchSize, len(emails))]
		found, err := r.s.c.batchRead(ctx, "contacts", "email", chunk, []string{"email", optOutProperty})
		if err != nil {
			var idx []int
			for _, e := range chunk {
				idx = append(idx, r.byEmail[e]...)
			}
			r.fail(idx, fmt.Errorf("hubspot: reading contacts by email: %w", err))
			continue
		}
		for _, o := range found {
			e := strings.ToLower(strings.TrimSpace(o.prop("email")))
			holders := r.byEmail[e]
			if len(holders) == 0 {
				// The answer names an email we did not send (HubSpot matched
				// another address of the contact): S0 confirms this cannot
				// happen; fail every lead in the chunk rather than guess.
				var idx []int
				for _, c := range chunk {
					idx = append(idx, r.byEmail[c]...)
				}
				r.fail(idx, errors.New("hubspot: a contact read by email came back under another address"))
				continue
			}
			for _, i := range holders {
				r.contact[i] = append(r.contact[i], o.ID)
			}
			if optedOut(o) {
				r.optout(e)
			}
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
					// The contact is this lead's, whatever address it now
					// shows: the opt-out goes to the lead's own email.
					if es := cleanEmails(r.leads[i].Emails); len(es) > 0 {
						r.optout(es[0])
					} else if len(r.leads[i].LinkedInURLs) > 0 {
						r.events = append(r.events, api.Event{Kind: "optout", LinkedInURL: r.leads[i].LinkedInURLs[0]})
					}
				}
			}
		}
	}
}

// candidate is one deal a company may have, and where it was found.
type candidate struct {
	id     string
	stored bool // the company's stored deal (CompanyDealID)
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
				add(d, cdeals[c]...)
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
			d := strings.ToLower(strings.TrimSpace(o.prop("domain")))
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
			add(strings.ToLower(strings.TrimSpace(o.prop(domainProp))), o.ID)
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
		objs, err := r.s.c.batchRead(ctx, "deals", "", all, []string{"dealstage", "pipeline"})
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

	for _, d := range domains {
		if r.badDom[d] {
			continue
		}
		r.events = append(r.events, companyEvent(d, cands[d], read, pipes))
	}
}

// companyEvent is the one deal event for a company: the strongest stage
// among its deals (won, then open, then lost), naming that deal; the
// company's stored deal is preferred within a stage. A stored deal that no
// longer exists counts as lost. No deal at all is deal_lost with no deal id
// (contracts section 5.3).
func companyEvent(domain string, cands []candidate, read map[string]object, pipes *pipelines) api.Event {
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
