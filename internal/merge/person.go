package merge

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// newLeadID mints a lead id: a UUIDv7, so ids sort by creation time. A
// variable so tests can make ids predictable.
var newLeadID = func() api.LeadID { return api.LeadID(uuid.Must(uuid.NewV7()).String()) }

// maxChain bounds a merged_into walk. Merges are written only by Apply, which
// never makes a cycle, but a hand-edited store could; a bounded walk stops
// there instead of hanging the run.
const maxChain = 64

// Live follows merged_into from a lead to the lead that absorbed it, and
// returns the lead itself when it was never merged. An unknown id is returned
// unchanged.
func Live(m *model.Model, id api.LeadID) api.LeadID {
	for i := 0; i < maxChain; i++ {
		p, ok := m.People[model.Key(id)]
		if !ok || p.MergedInto == "" || p.MergedInto == id {
			return id
		}
		id = p.MergedInto
	}
	return id
}

// FindPerson matches an event's person, with no writes (contracts 12.5): first
// by Attrs["contact_id"] through Applied rows (source `receiver`, row id), then
// by email, then by LinkedIn URL, then follows merged_into to the live lead.
// The event's keys must already be normalized (NormalizeEventKeys).
//
// An event that carries an email matches by that email only, never by its
// LinkedIn URL: an unknown email whose LinkedIn URL belongs to someone else is
// a new person (RFC 6.5), so one wrong URL can never pin a stranger's event,
// such as an opt-out, on an existing lead.
func FindPerson(m *model.Model, e api.Event) (api.LeadID, bool) {
	if cid := strings.TrimSpace(e.Attrs[FieldContactID]); cid != "" {
		if ar, ok := m.AppliedRows[model.K(ReceiverSource, cid)]; ok && ar.LeadID != "" {
			return Live(m, ar.LeadID), true
		}
	}
	if e.Email != "" {
		if id, ok := m.Identities[model.Key(e.Email)]; ok {
			return Live(m, id.LeadID), true
		}
		return "", false
	}
	if e.LinkedInURL != "" {
		if id, ok := m.Identities[model.Key(e.LinkedInURL)]; ok {
			return Live(m, id.LeadID), true
		}
	}
	return "", false
}

// ApplyEventPerson resolves an event's person like FindPerson and, when no
// lead matches, creates one under source `receiver` from the event's keys and
// person attributes (contracts 12.5). It returns "" for an event that names
// no usable person (a company-only event, or one whose only keys cannot be
// written). A LinkedIn URL another lead already holds is not written and
// counts as a key conflict, as for input rows.
func ApplyEventPerson(m *model.Model, e api.Event) api.LeadID {
	if id, ok := FindPerson(m, e); ok {
		return id
	}
	at := e.ReceivedAt
	if at.IsZero() {
		at = e.At
	}
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	email := e.Email
	if email != "" && ValidateEmailShape(email) != nil {
		email = ""
	}
	li := e.LinkedInURL
	conflict := false
	if li != "" {
		if _, taken := m.Identities[model.Key(li)]; taken {
			li, conflict = "", true
		}
	}
	if email == "" && li == "" {
		return ""
	}

	id := newLeadID()
	p := model.Person{LeadID: id, CreatedAt: at, Fields: map[string]model.Field{}}
	set := func(name, v string) {
		if v = strings.TrimSpace(v); v != "" {
			p.Fields[name] = model.Field{Value: v, SourceID: ReceiverSource, At: at}
		}
	}
	set(FieldEmail, email)
	set(FieldLinkedIn, li)
	set(FieldFullName, e.Attrs["full_name"])
	set("title", e.Attrs["title"])
	set("company.name", e.Attrs["company"])
	cid := strings.TrimSpace(e.Attrs[FieldContactID])
	set(FieldContactID, cid)
	if e.Domain != "" {
		set(FieldDomain, e.Domain)
	} else if d, ok := DeriveCompanyDomain(email); ok {
		p.Fields[FieldDomain] = model.Field{Value: d, SourceID: ReceiverSource, At: at, Derived: true}
	}
	m.Put(model.TablePeople, p)
	if email != "" {
		m.Put(model.TableIdentities, model.Identity{Key: email, Kind: "email", LeadID: id, SourceID: ReceiverSource, FirstSeenAt: at})
	}
	if li != "" {
		m.Put(model.TableIdentities, model.Identity{Key: li, Kind: "linkedin", LeadID: id, SourceID: ReceiverSource, FirstSeenAt: at})
	}
	if cid != "" {
		// So the contact's later webhooks and receiver rows find this lead by
		// contact id. The empty hash makes the first real receiver row apply.
		if _, ok := m.AppliedRows[model.K(ReceiverSource, cid)]; !ok {
			m.Put(model.TableAppliedRows, model.AppliedRow{SourceID: ReceiverSource, RowID: cid, LeadID: id, FirstAppliedAt: at})
		}
	}
	if conflict {
		countKeyConflict(m)
	}
	return id
}

// countKeyConflict adds one to State.key_conflicts, the running count the
// `duplicates` check reports.
func countKeyConflict(m *model.Model) {
	n, _ := strconv.Atoi(m.StateValue(StateKeyConflicts))
	m.SetState(StateKeyConflicts, strconv.Itoa(n+1))
}

// StateKeyConflicts is the State key holding the running key-conflict count.
const StateKeyConflicts = "key_conflicts"

// Resolve finds the live lead a person names: a lead id, an email or a
// LinkedIn URL, normalized as Identities are keyed.
func Resolve(m *model.Model, person string) (api.LeadID, bool) {
	p := NormalizePerson(person)
	if p == "" || p == "*" {
		return "", false
	}
	for _, cand := range []string{p, strings.ToLower(p)} {
		if _, ok := m.People[model.Key(cand)]; ok {
			return Live(m, api.LeadID(cand)), true
		}
	}
	if id, ok := m.Identities[model.Key(p)]; ok {
		return Live(m, id.LeadID), true
	}
	return "", false
}

// Index is a read-only view over a model for reading many leads at once:
// families, emails, sources and company counts, each built in one pass. Build
// it after merging; it does not see later changes.
type Index struct {
	m        *model.Model
	families map[api.LeadID][]api.LeadID
	sources  map[api.LeadID]map[string]bool // live lead -> source ids
}

// NewIndex builds the view.
func NewIndex(m *model.Model) *Index {
	x := &Index{m: m, families: map[api.LeadID][]api.LeadID{}, sources: map[api.LeadID]map[string]bool{}}
	ids := make([]string, 0, len(m.People))
	for k := range m.People {
		ids = append(ids, string(k))
	}
	sort.Strings(ids)
	for _, s := range ids {
		id := api.LeadID(s)
		live := Live(m, id)
		x.families[live] = append(x.families[live], id)
	}
	for live, fam := range x.families {
		// The live lead first, then the leads it absorbed in id order.
		sort.SliceStable(fam, func(i, j int) bool { return fam[i] == live && fam[j] != live })
	}
	add := func(id api.LeadID, src string) {
		if id == "" || src == "" {
			return
		}
		live := Live(m, id)
		if x.sources[live] == nil {
			x.sources[live] = map[string]bool{}
		}
		x.sources[live][src] = true
	}
	for _, ar := range m.AppliedRows {
		add(ar.LeadID, ar.SourceID)
	}
	for _, idn := range m.Identities {
		add(idn.LeadID, idn.SourceID)
	}
	return x
}

// Family returns a live lead and every lead whose merged_into chain ends at
// it: the live lead first. A lead that was merged away has no family of its
// own; ask for its live lead (Live).
func (x *Index) Family(id api.LeadID) []api.LeadID {
	return append([]api.LeadID(nil), x.families[id]...)
}

// LiveLeads returns every live lead, sorted.
func (x *Index) LiveLeads() []api.LeadID {
	out := make([]api.LeadID, 0, len(x.families))
	for id := range x.families {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// familyIdentities returns the identities of a family of one kind, the live
// lead's own first, each lead's oldest first.
func (x *Index) familyIdentities(id api.LeadID, kind string) []model.Identity {
	var out []model.Identity
	for _, f := range x.families[id] {
		for _, idn := range x.m.IdentitiesOf(f) {
			if idn.Kind == kind {
				out = append(out, idn)
			}
		}
	}
	return out
}

// PrimaryEmail is the live lead's primary email (contracts section 4): its
// newest email from a same-source correction, else its oldest email, from its
// own identities before those of leads it absorbed. Empty when it has none.
func (x *Index) PrimaryEmail(id api.LeadID) string {
	return x.primary(id, "email", FieldEmail)
}

// PrimaryLinkedIn is the live lead's first LinkedIn URL, or empty.
func (x *Index) PrimaryLinkedIn(id api.LeadID) string {
	return x.primary(id, "linkedin", FieldLinkedIn)
}

// primary reads People.fields[field], which merge keeps on the first key of
// that kind and moves only on a same-source email correction, when it is one
// of the family's identities; else the family's first identity of that kind.
func (x *Index) primary(id api.LeadID, kind, field string) string {
	ids := x.familyIdentities(id, kind)
	if v := x.m.People[model.Key(id)].Fields[field].Value; v != "" {
		for _, idn := range ids {
			if idn.Key == v {
				return v
			}
		}
	}
	if len(ids) > 0 {
		return ids[0].Key
	}
	return ""
}

// Emails returns every email of the live lead and the leads it absorbed,
// primary first (LeadRef.Emails).
func (x *Index) Emails(id api.LeadID) []string { return x.keys(id, "email", x.PrimaryEmail(id)) }

// LinkedInURLs returns every LinkedIn URL of the live lead and the leads it
// absorbed, its first one first.
func (x *Index) LinkedInURLs(id api.LeadID) []string {
	return x.keys(id, "linkedin", x.PrimaryLinkedIn(id))
}

func (x *Index) keys(id api.LeadID, kind, first string) []string {
	var out []string
	if first != "" {
		out = append(out, first)
	}
	for _, idn := range x.familyIdentities(id, kind) {
		if idn.Key != first {
			out = append(out, idn.Key)
		}
	}
	return out
}

// PersonKey is how Overrides rows written by the CLI name a lead (contracts
// section 7): its primary email, else its LinkedIn URL, else its id.
func (x *Index) PersonKey(id api.LeadID) string {
	if e := x.PrimaryEmail(id); e != "" {
		return e
	}
	if l := x.PrimaryLinkedIn(id); l != "" {
		return l
	}
	return string(id)
}

// SourcesSeen is the built-in field sources_seen: the number of distinct
// channels (contracts section 3, sources[].channel) that reported the live
// lead or a lead it absorbed. A source no longer configured counts under its
// id; the receiver is its own channel.
func (x *Index) SourcesSeen(id api.LeadID, sources []config.Source) int {
	channel := map[string]string{}
	for _, s := range sources {
		channel[s.ID] = s.Channel
	}
	seen := map[string]bool{}
	for src := range x.sources[id] {
		c := channel[src]
		if c == "" {
			c = src
		}
		seen[c] = true
	}
	return len(seen)
}

// ReceiverOnly is the built-in field receiver_only: true when the receiver is
// the only source that reported the live lead or a lead it absorbed.
func (x *Index) ReceiverOnly(id api.LeadID) bool {
	s := x.sources[id]
	return len(s) == 1 && s[ReceiverSource]
}

// LeadsSeen is the built-in company field leads_seen: the number of live leads
// at each company domain.
func (x *Index) LeadsSeen() map[string]int {
	out := map[string]int{}
	for id := range x.families {
		if d := x.m.People[model.Key(id)].Fields[FieldDomain].Value; d != "" {
			out[d]++
		}
	}
	return out
}
