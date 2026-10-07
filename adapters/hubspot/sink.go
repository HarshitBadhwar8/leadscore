package hubspot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Sink is the `hubspot` sink: `hubspot:contacts` has the step contact and
// `hubspot:deals` the steps contact and deal (contracts section 6). Both
// steps find before they create, so a step replayed after a crash, or called
// twice, leaves one record.
//
// The engine builds one Sink per run, so the pipeline names are resolved once
// per run, and the company-to-deal map below lasts one run.
type Sink struct {
	s settings

	mu    sync.Mutex
	pipes *pipelines
	deals map[string]string // company domain -> the deal a deal step settled this run
}

// NewSink builds the sink from its sinks.hubspot block.
func NewSink(cfg api.Config) (*Sink, error) {
	s, err := parse(cfg)
	if err != nil {
		return nil, fmt.Errorf("hubspot sink: %w", err)
	}
	return &Sink{s: s, deals: map[string]string{}}, nil
}

// Steps returns the destination's steps; an unknown destination has none
// (the engine raises a problem and the lane waits).
func (k *Sink) Steps(dest string) []string {
	switch dest {
	case destContacts:
		return []string{stepContact}
	case destDeals:
		return []string{stepContact, stepDeal}
	}
	return nil
}

// Do runs one step.
func (k *Sink) Do(ctx context.Context, req api.StepRequest) (string, error) {
	switch {
	case req.Key.Step == stepContact && (req.Dest == destContacts || req.Dest == destDeals):
		return k.contact(ctx, req)
	case req.Key.Step == stepDeal && req.Dest == destDeals:
		return k.deal(ctx, req)
	}
	return "", fmt.Errorf("hubspot sink: no step %q for destination %q", req.Key.Step, req.Dest)
}

// contact finds the lead's contact by the lead id property, then by email
// (every email the lead has, primary first), and reuses it untouched; else it
// creates one with the lead id and the context properties. A 409 (another
// contact holds the email, which the search had not shown yet) counts as
// found.
func (k *Sink) contact(ctx context.Context, req api.StepRequest) (string, error) {
	leadProp := k.s.prop("lead_id")
	found, err := k.s.c.search(ctx, "contacts",
		[]filterGroup{{Filters: []filter{{PropertyName: leadProp, Operator: "EQ", Value: string(req.Lead.ID)}}}}, []string{leadProp})
	if err != nil {
		return "", fmt.Errorf("hubspot: finding the contact by lead id: %w", err)
	}
	if id := lowestID(found); id != "" {
		return id, nil
	}
	emails := cleanEmails(req.Lead.Emails)
	if len(emails) == 0 {
		return "", fmt.Errorf("hubspot: the lead has no email to create a contact with: %w", api.ErrRefused)
	}
	if id, err := k.contactByEmail(ctx, emails); err != nil || id != "" {
		return id, err
	}
	id, err := k.s.c.create(ctx, "contacts", k.contactProps(req, emails[0]), nil)
	if err == nil {
		return id, nil
	}
	if statusOf(err) == http.StatusConflict {
		if id := existingID(err); id != "" {
			return id, nil
		}
		if id, ferr := k.contactByEmail(ctx, emails); ferr != nil || id != "" {
			return id, ferr
		}
		return "", fmt.Errorf("hubspot: the contact already exists but was not found yet: %w", api.ErrTransient)
	}
	if errors.Is(err, api.ErrRefused) {
		return "", fmt.Errorf("hubspot: the email was refused as invalid: %w", err)
	}
	return "", fmt.Errorf("hubspot: creating the contact: %w", err)
}

// contactByEmail returns the contact holding the earliest of the emails.
func (k *Sink) contactByEmail(ctx context.Context, emails []string) (string, error) {
	for start := 0; start < len(emails); start += maxGroups {
		chunk := emails[start:min(start+maxGroups, len(emails))]
		var groups []filterGroup
		for _, e := range chunk {
			groups = append(groups, filterGroup{Filters: []filter{{PropertyName: "email", Operator: "EQ", Value: e}}})
		}
		found, err := k.s.c.search(ctx, "contacts", groups, []string{"email"})
		if err != nil {
			return "", fmt.Errorf("hubspot: finding the contact by email: %w", err)
		}
		for _, e := range chunk {
			var match []object
			for _, o := range found {
				if strings.EqualFold(strings.TrimSpace(o.prop("email")), e) {
					match = append(match, o)
				}
			}
			if id := lowestID(match); id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

// contactProps are a new contact's properties: identity, the lead id, and
// the context properties salespeople read (set on create only). Empty values
// are not sent: HubSpot would store an empty string over its own data.
func (k *Sink) contactProps(req api.StepRequest, email string) map[string]string {
	first, last := splitName(req.Lead.FullName)
	props := map[string]string{
		"email":             email,
		"firstname":         first,
		"lastname":          last,
		"jobtitle":          req.Lead.Title,
		"company":           req.Lead.Fields["company.name"],
		k.s.prop("lead_id"): string(req.Lead.ID),
		k.s.prop("lane"):    req.Key.LaneID,
	}
	if v := req.Lead.Verdict; v != nil {
		props[k.s.prop("tier")] = valueText(v.Values["tier"])
		props[k.s.prop("priority")] = valueText(v.Values["priority"])
		props[k.s.prop("score")] = valueText(v.AccountScore + v.ContactScore)
		props[k.s.prop("reasons")] = clipText(strings.Join(v.Reasons, "; "), maxTextLen)
	}
	for key, val := range props {
		if strings.TrimSpace(val) == "" {
			delete(props, key)
		}
	}
	return props
}

// maxTextLen is HubSpot's limit for a text property value.
const maxTextLen = 65536

// deal settles the company's one open deal and associates the lead's
// contact with it. It reuses, in this order, a deal it can read as open:
// another lead's done deal step (Related), the deal a deal step settled
// earlier this run, the company's stored deal (CompanyDealID), a deal
// already associated with this contact that carries the company's domain
// (a retry of this step whose earlier call created it, read through the
// association, which does not lag), and a deal carrying the company's
// domain found by search. Only then does it create one, named by the
// domain, with the domain property and the association in the same call,
// so a deal never exists without them.
//
// Any read that fails stops the step before a create: a deal is never
// opened on an incomplete answer.
func (k *Sink) deal(ctx context.Context, req api.StepRequest) (string, error) {
	contactID := req.Prior[stepContact]
	if contactID == "" {
		return "", errors.New("hubspot: the deal step has no contact id from the contact step")
	}
	domain := strings.ToLower(strings.TrimSpace(req.Lead.Domain))
	if domain == "" {
		return "", fmt.Errorf("hubspot: a deal is per company, and the lead has no company domain: %w", api.ErrRefused)
	}
	pipes, err := k.pipelines(ctx)
	if err != nil {
		return "", err
	}
	if pipes.pipelineID == "" {
		return "", fmt.Errorf("hubspot: sinks.hubspot.pipeline and stage are not set, so the deal step waits: %w", api.ErrTransient)
	}

	domainProp := k.s.prop("company_domain")
	var known []string // reused whatever their domain property says
	for _, r := range req.Related {
		if r.Key.Step == stepDeal && r.Dest == destDeals && r.State == "done" && r.VendorID != "" {
			known = append(known, r.VendorID)
		}
	}
	k.mu.Lock()
	if id := k.deals[domain]; id != "" {
		known = append(known, id)
	}
	k.mu.Unlock()
	if req.Lead.CompanyDealID != "" {
		known = append(known, req.Lead.CompanyDealID)
	}
	assoc, err := k.s.c.associations(ctx, "contacts", "deals", []string{contactID})
	if err != nil {
		return "", fmt.Errorf("hubspot: reading the contact's deals: %w", err)
	}
	found, err := k.s.c.search(ctx, "deals",
		[]filterGroup{{Filters: []filter{{PropertyName: domainProp, Operator: "EQ", Value: domain}}}}, []string{domainProp})
	if err != nil {
		return "", fmt.Errorf("hubspot: finding the company's deal: %w", err)
	}
	byDomain := sortedIDs(append(assoc[contactID], idsOf(found)...))

	ids := dedupe(append(append([]string(nil), known...), byDomain...))
	deals := map[string]object{}
	if len(ids) > 0 {
		read, err := k.s.c.batchRead(ctx, "deals", "", ids, []string{"dealstage", "pipeline", domainProp})
		if err != nil {
			return "", fmt.Errorf("hubspot: reading candidate deals: %w", err)
		}
		for _, o := range read {
			deals[o.ID] = o
		}
	}
	open := func(id string) bool {
		o, ok := deals[id]
		if !ok {
			return false // deleted
		}
		c, known := pipes.of(o.prop("pipeline"), o.prop("dealstage"))
		return known && c == classOpen
	}
	reuse := ""
	for _, id := range known {
		if open(id) {
			reuse = id
			break
		}
	}
	if reuse == "" {
		for _, id := range byDomain {
			if open(id) && strings.EqualFold(deals[id].prop(domainProp), domain) {
				reuse = id
				break
			}
		}
	}
	if reuse != "" {
		path := fmt.Sprintf("/crm/v4/objects/deals/%s/associations/default/contacts/%s", reuse, contactID)
		if err := k.s.c.call(ctx, http.MethodPut, path, nil, nil); err != nil {
			return "", fmt.Errorf("hubspot: associating the contact with the company's deal: %w", err)
		}
		k.remember(domain, reuse)
		return reuse, nil
	}

	props := map[string]string{
		"dealname":       domain,
		"pipeline":       pipes.pipelineID,
		"dealstage":      pipes.stageID,
		domainProp:       domain,
		k.s.prop("lane"): req.Key.LaneID,
	}
	assocs := []any{map[string]any{
		"to":    map[string]string{"id": contactID},
		"types": []map[string]any{{"associationCategory": "HUBSPOT_DEFINED", "associationTypeId": dealToContact}},
	}}
	id, err := k.s.c.create(ctx, "deals", props, assocs)
	if err != nil {
		return "", fmt.Errorf("hubspot: creating the company's deal: %w", err)
	}
	k.remember(domain, id)
	return id, nil
}

// dealToContact is HubSpot's built-in association type from a deal to a
// contact (S0 confirms the id).
const dealToContact = 3

func (k *Sink) remember(domain, id string) {
	k.mu.Lock()
	k.deals[domain] = id
	k.mu.Unlock()
}

// pipelines resolves the configured pipeline and stage once per sink (one
// run). A name that does not resolve makes the step wait; the next run
// resolves again.
func (k *Sink) pipelines(ctx context.Context) (*pipelines, error) {
	k.mu.Lock()
	p := k.pipes
	k.mu.Unlock()
	if p != nil {
		return p, nil
	}
	p, err := readPipelines(ctx, k.s)
	if errors.Is(err, errConfig) {
		return nil, fmt.Errorf("hubspot: %w; fix sinks.hubspot or run `leadscore setup hubspot`: %w", err, api.ErrTransient)
	}
	if err != nil {
		return nil, fmt.Errorf("hubspot: %w", err)
	}
	k.mu.Lock()
	k.pipes = p
	k.mu.Unlock()
	return p, nil
}

// splitName splits a display name: the first word is the first name, the
// rest the last name.
func splitName(full string) (first, last string) {
	f := strings.Fields(full)
	switch len(f) {
	case 0:
		return "", ""
	case 1:
		return f[0], ""
	}
	return f[0], strings.Join(f[1:], " ")
}

// valueText writes a derived value: whole numbers without a decimal point.
func valueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// clipText cuts s to at most n bytes without splitting a character.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func cleanEmails(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func idsOf(objs []object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.ID)
	}
	return out
}

// lowestID picks the oldest record (HubSpot ids grow) among several matches,
// so the same answer is chosen every time.
func lowestID(objs []object) string {
	ids := sortedIDs(idsOf(objs))
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// sortedIDs sorts numeric ids by value, oldest first, without duplicates.
func sortedIDs(ids []string) []string {
	ids = dedupe(ids)
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) < len(ids[j])
		}
		return ids[i] < ids[j]
	})
	return ids
}

func dedupe(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
