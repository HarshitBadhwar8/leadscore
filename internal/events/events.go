// Package events holds the event rules (RFC 6.7, contracts sections 5.3 and
// 12.7): the de-duplication key of every event, parsing stored receiver
// requests, and applying an event's effect to a lead's Outcomes and People.
//
// Apply is the only code that writes event effects. Losing an effect means an
// opt-out that never lands, so every rule here errs towards recording: an
// opt-out time is kept at its earliest, an automated opt-out always wins the
// origin, an opt-out reaches every live lead holding one of the event's
// identity keys, and nothing here ever clears one.
package events

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// Event origins the engine sets (api.Event.Origin); any other origin is a
// source id. Config refuses these as source ids.
const (
	OriginReceiver     = apollo.OriginReceiver // merge.ReceiverSource
	OriginPolling      = "polling"
	OriginHubSpot      = "hubspot"
	OriginApolloLookup = "apollo_lookup"
)

// LookupOrigin is the Origin of an event a Lookup of type typ returned:
// `hubspot` and `apollo_lookup` for the built-in types, else
// `lookup:<type>`, so a plug-in lookup can never pose as a source or the
// receiver. Its opt-outs are stored with unsubscribed_origin `lookup`, which
// a `resubscribe` never clears.
func LookupOrigin(typ string) string {
	switch typ {
	case "hubspot":
		return OriginHubSpot
	case "apollo":
		return OriginApolloLookup
	}
	return "lookup:" + typ
}

// Unsubscribed origins (Outcomes.unsubscribed_origin).
const (
	UnsubEvent  = "event"
	UnsubLookup = "lookup"
)

// Attrs keys the engine sets on an event that is not applied (Kind empty).
const (
	AttrReject  = "reject"  // cannot be applied: the reason
	AttrIgnored = "ignored" // a kind we do not act on: the reason
)

// Time is when an event happened: At, else its received time, in UTC.
func Time(e api.Event) time.Time {
	if !e.At.IsZero() {
		return e.At.UTC()
	}
	return e.ReceivedAt.UTC()
}

// Key is an event's de-duplication key (RFC 6.7). The event's keys must
// already be normalized (merge.NormalizeEventKeys). Every free-text part is
// length-prefixed, so two different events never flatten to one key.
//
//   - A polled `reply`: its message id plus label (apollo.PolledReplyKey).
//     With no message id, the person key (contact id first), label and time
//     (else received time) instead, so two people's replies never collapse;
//     with no vendor time either (no_reply_time), the person key and label.
//   - A receiver visit: the person key (contact id, else email, else LinkedIn
//     URL), the kind and the vendor's visit time as sent (before the clamp to
//     the received time, so a retry keys the same). With no usable visit time,
//     the person key, the page and the UTC day it was received, so a
//     redelivery the same day is one event and a visit on another day is a new
//     one. A company-only visit uses the employer domain in place of the
//     person.
//   - Any other receiver event (sent, replied, replied_positive,
//     unsubscribed): the kind, the conversation link (else the email) and the
//     stage (trimmed, lowercased), so a reply that moves the stage is a new
//     event and a redelivery of the same state is not.
//   - A lookup event: its kind, deal, person or company and time; lookup events
//     are not de-duplicated through Seen events, so this only names them.
//   - A source event row: the source id, the person key (email, else LinkedIn
//     URL, else domain), the kind and the event time.
func Key(e api.Event) api.EventID {
	kind := strings.ToLower(e.Kind)
	at := model.FormatTime(e.At)
	switch {
	case kind == "reply":
		label := strings.ToLower(strings.TrimSpace(e.Attrs[apollo.AttrLabel]))
		if id := e.Attrs[apollo.AttrMessageID]; id != "" {
			return apollo.PolledReplyKey(id, label)
		}
		if e.Attrs[apollo.AttrNoReplyTime] != "" {
			// Timed at the poll only: the poll's time would make every poll
			// a new reply.
			return join("polled", "person", person(e), label, "untimed")
		}
		return join("polled", "person", person(e), label, model.FormatTime(Time(e)))
	case e.Origin == OriginReceiver && strings.HasPrefix(kind, "visit_"):
		who := person(e)
		if strings.HasPrefix(who, "domain:") {
			who = ""
		}
		if who == "" {
			who = "domain:" + e.Domain
		}
		if e.Attrs[apollo.AttrNoVisitTime] != "" {
			return join("visit", kind, "day", who, e.Attrs[apollo.AttrPage], e.ReceivedAt.UTC().Format(time.DateOnly))
		}
		// The vendor's own visit time, before any clamp, so a retried
		// delivery keys the same whenever it arrives.
		visited := e.Attrs[apollo.AttrVisitedAt]
		if visited == "" {
			visited = at
		}
		return join("visit", kind, "at", who, visited)
	case e.Origin == OriginReceiver:
		subject := e.Attrs[apollo.AttrConversationLink]
		if subject == "" {
			subject = e.Email
		}
		return join("apollo", kind, subject, strings.ToLower(strings.TrimSpace(e.Attrs[apollo.AttrStage])))
	case e.Origin == OriginHubSpot || e.Origin == OriginApolloLookup || strings.HasPrefix(e.Origin, "lookup:"):
		return join("lookup", e.Origin, kind, e.Attrs["deal_id"], personKey(e), at)
	}
	return join("source", e.Origin, personKey(e), kind, at)
}

// person is a vendor event's person key: the vendor's contact id first, since
// it survives the vendor correcting the person's email, then the email, the
// LinkedIn URL and the domain.
func person(e api.Event) string {
	if id := strings.TrimSpace(e.Attrs[apollo.AttrContactID]); id != "" {
		return "id:" + id
	}
	return personKey(e)
}

func personKey(e api.Event) string {
	switch {
	case e.Email != "":
		return "email:" + e.Email
	case e.LinkedInURL != "":
		return "linkedin:" + e.LinkedInURL
	case e.Domain != "":
		return "domain:" + e.Domain
	}
	return ""
}

// join is a key from its first part (the rule's name) and length-prefixed
// parts.
func join(rule string, parts ...string) api.EventID {
	var b strings.Builder
	b.WriteString(rule)
	for _, p := range parts {
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return api.EventID(b.String())
}

// Parse turns stored receiver requests into events and the receiver input rows
// their persons yield (source id `receiver`). It is pure: no writes. A request
// that cannot be applied comes back as an event with Kind empty and
// Attrs["reject"] naming its sequence and the reason (never a value from the
// body), and one of a kind we do not act on with Attrs["ignored"], so the
// caller logs both rather than dropping them unseen.
func Parse(raw []api.RawEvent) ([]api.Event, []api.InputRow) {
	var evs []api.Event
	var rows []api.InputRow
	for _, r := range raw {
		es, rs, err := apollo.ParseRaw(r)
		if err != nil {
			attr := AttrReject
			if errors.Is(err, apollo.ErrIgnored) {
				attr = AttrIgnored
			}
			evs = append(evs, api.Event{
				Origin: OriginReceiver, ReceivedAt: r.ReceivedAt.UTC(), At: r.ReceivedAt.UTC(),
				Attrs: map[string]string{attr: fmt.Sprintf("stored event %s (%s): %v", r.Seq, r.Kind, err)},
			})
			continue
		}
		evs = append(evs, es...)
		rows = append(rows, rs...)
	}
	return evs, rows
}

// CreatesLead reports whether an event of this kind whose person matches no
// lead creates one (RFC 6.7): a send, reply or unsubscribe, a polled reply,
// and an identified website visit. An opt-out that arrives before the
// person's input row is then never lost; the later row merges into the lead.
func CreatesLead(kind string) bool {
	switch strings.ToLower(kind) {
	case "sent", "replied", "replied_positive", "unsubscribed", "reply":
		return true
	}
	return strings.HasPrefix(strings.ToLower(kind), "visit_")
}

// VendorOnly reports a kind only a vendor may report (contracts section 5.2):
// a send, reply, opt-out or deal. A file or plug-in source claiming one could
// suppress or unblock a person on no evidence.
func VendorOnly(kind string) bool {
	k := strings.ToLower(kind)
	switch k {
	case "sent", "unsubscribed", "reply", "optout":
		return true
	}
	return strings.HasPrefix(k, "replied") || strings.HasPrefix(k, "deal_")
}

// The reply-label map (contracts section 5.5). "none" means no outcome.
const (
	labelUnsubscribe = "unsubscribe"
	labelNone        = "_unlabelled" // the reply_labels key for a reply with no label
	statusNone       = "none"
)

var baseLabels = map[string]string{
	"willing_to_meet":    "replied_positive",
	"unsubscribe":        "unsubscribed",
	"not_interested":     "replied_negative",
	"follow_up_question": "replied_neutral",
	"person_referral":    "replied_neutral",
	"already_left_company_or_not_right_person": "replied_neutral",
	"none_of_the_above":                        "replied_neutral",
	"out_of_office":                            statusNone,
	labelNone:                                  "replied_unlabelled",
}

// ReplyStatus maps a polled reply's label to its status, with the team's
// reply_labels overrides (contracts section 5.5; config lowercases their
// keys). ok is false for no outcome. `unsubscribe` cannot be overridden. A
// label the map does not know is still a reply, and reads as
// replied_unlabelled.
func ReplyStatus(label string, overrides map[string]string) (status string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		l = labelNone
	}
	if l == labelUnsubscribe {
		return "unsubscribed", true
	}
	s, known := baseLabels[l]
	if o, has := overrides[l]; has {
		s, known = o, true
	}
	if !known {
		s = "replied_unlabelled"
	}
	if s == statusNone {
		return "", false
	}
	return s, true
}

// optOutOrigin is the unsubscribed origin an event sets, or "" for an event
// that is not an opt-out.
func optOutOrigin(kind string, e api.Event, labels map[string]string) string {
	switch kind {
	case "unsubscribed":
		return UnsubEvent
	case "optout":
		return UnsubLookup
	case "reply":
		if s, _ := ReplyStatus(e.Attrs[apollo.AttrLabel], labels); s == "unsubscribed" {
			return UnsubEvent
		}
	}
	return ""
}

// Apply applies an event's section 5.3 effect to the live lead (following
// merged_into). The caller resolved the person: Intake through
// merge.ApplyEventPerson, the pre-push and re-read steps with the lead they
// looked up. lead may be empty for a company-level deal event. labels are the
// team's reply_labels overrides. It returns the live leads whose Outcomes row
// or Apollo hold it changed, sorted (the re-read folds them again).
//
// An opt-out (unsubscribed, optout, a polled unsubscribe label) is written on
// every lead holding one of the event's identity keys (its contact id,
// well-formed email or LinkedIn URL) as stored, before following
// merged_into, and on the resolved live lead only when no key has an owner
// (applyOptOutEvent): the fold reads it across the merge family while
// merged, and undoing a wrong merge leaves it with the person who opted out.
// An owner outside the resolved lead's family is logged as key_conflict, with
// lead ids only: wrongly not contacting someone is acceptable, contacting an
// opted-out person is not. A lead in a hand-edited merged_into cycle gets it
// on every member of the cycle. Replies and visits stay on the resolved lead.
//
// A visit or custom kind has no outcome effect (it feeds detectors only).
// Applying an event twice changes nothing the second time.
func Apply(m *model.Model, lead api.LeadID, e api.Event, labels map[string]string) []api.LeadID {
	kind := strings.ToLower(e.Kind)
	at := Time(e)
	received := e.ReceivedAt.UTC()
	if received.IsZero() {
		received = at
	}
	if strings.HasPrefix(kind, "deal_") {
		return applyDeal(m, lead, e, kind, at)
	}
	if lead != "" {
		lead = merge.Live(m, lead)
		if _, ok := m.People[model.Key(lead)]; !ok {
			lead = ""
		}
	}
	if origin := optOutOrigin(kind, e, labels); origin != "" {
		return applyOptOutEvent(m, lead, e, at, received, origin, kind != "optout")
	}
	if lead == "" {
		return nil
	}
	o := m.Outcomes[model.Key(lead)]
	before := o
	o.LeadID = lead
	switch kind {
	case "sent":
		if o.ContactedAt.IsZero() || at.Before(o.ContactedAt) {
			o.ContactedAt = at
		}
	case "replied":
		setReply(&o, "replied_neutral", received)
	case "replied_positive":
		setReply(&o, "replied_positive", received)
	case "reply":
		if s, ok := ReplyStatus(e.Attrs[apollo.AttrLabel], labels); ok {
			setReply(&o, s, received)
		}
	default:
		return nil
	}
	dirty := !reflect.DeepEqual(o, before)
	if dirty {
		m.Put(model.TableOutcomes, o)
	}
	if markHeld(m, lead, at) || dirty {
		return []api.LeadID{lead}
	}
	return nil
}

// applyOptOut records an opt-out on one lead: the time kept at its earliest,
// the origin always the automated one, even over `manual` (contracts section
// 5.3). An Apollo opt-out also marks the lead Apollo-held. It reports whether
// the outcome or the hold changed.
func applyOptOut(m *model.Model, lead api.LeadID, at time.Time, origin string, held bool) bool {
	o := m.Outcomes[model.Key(lead)]
	before := o
	o.LeadID = lead
	if o.UnsubscribedAt.IsZero() || at.Before(o.UnsubscribedAt) {
		o.UnsubscribedAt = at
	}
	o.UnsubscribedOrigin = origin
	changed := !reflect.DeepEqual(o, before)
	if changed {
		m.Put(model.TableOutcomes, o)
	}
	if held && markHeld(m, lead, at) {
		changed = true
	}
	return changed
}

// applyOptOutEvent records an opt-out on the raw owners of the event's
// identity keys: the lead each key names in Identities (its well-formed
// email, its LinkedIn URL) and the lead the receiver contact's Applied rows
// name, before following merged_into, so the opt-out stays with the person
// who opted out if a wrong merge is later undone; the fold reads it across
// the merge family while merged. Only when no key has an owner does it go on
// the resolved live lead. A target in a hand-edited merged_into cycle gets
// it on every member of the cycle too. An owner whose live lead is not the
// resolved one is logged as key_conflict, with lead ids only. It returns the
// live leads it changed.
func applyOptOutEvent(m *model.Model, lead api.LeadID, e api.Event, at, received time.Time, origin string, held bool) []api.LeadID {
	targets := owners(m, e)
	if len(targets) == 0 && lead != "" {
		targets = []api.LeadID{lead}
	}
	set := map[api.LeadID]bool{}
	for _, t := range targets {
		live := merge.Live(m, t)
		changed := applyOptOut(m, t, at, origin, held)
		if cycle := cycleMembers(m, live); len(cycle) > 0 {
			for _, f := range append([]api.LeadID{live}, cycle...) {
				if f != t && applyOptOut(m, f, at, origin, false) {
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		set[live] = true
		if live == lead {
			continue
		}
		msg := fmt.Sprintf("an opt-out that matched no lead carries an identity key of lead %s; the opt-out was applied to it", live)
		if lead != "" {
			msg = fmt.Sprintf("an opt-out resolved to lead %s carries an identity key of lead %s; the opt-out was applied to both", lead, live)
		}
		m.Put(model.TableLog, model.LogEntry{At: received, Level: "warn", LeadID: live, Kind: merge.LogKeyConflict, Message: msg})
	}
	out := make([]api.LeadID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// cycleMembers returns the leads other than lead that resolve to it through a
// merged_into cycle, sorted. A live lead outside a cycle has no merged_into of
// its own, so the common case returns at once without a scan.
func cycleMembers(m *model.Model, lead api.LeadID) []api.LeadID {
	if m.People[model.Key(lead)].MergedInto == "" {
		return nil
	}
	var out []api.LeadID
	for id := range merge.Cycles(m) {
		if id != lead && merge.Live(m, id) == lead {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// owners are the leads holding one of the event's identity keys, as stored
// (merged_into not followed): its contact id (through Applied rows), its
// email when well-formed, its LinkedIn URL. Known leads only, sorted.
func owners(m *model.Model, e api.Event) []api.LeadID {
	set := map[api.LeadID]bool{}
	add := func(id api.LeadID) {
		if _, ok := m.People[model.Key(id)]; ok && id != "" {
			set[id] = true
		}
	}
	if cid := strings.TrimSpace(e.Attrs[apollo.AttrContactID]); cid != "" {
		add(m.AppliedRows[model.K(merge.ReceiverSource, cid)].LeadID)
	}
	if e.Email != "" && merge.ValidateEmailShape(e.Email) == nil {
		add(m.Identities[model.Key(e.Email)].LeadID)
	}
	if e.LinkedInURL != "" {
		add(m.Identities[model.Key(e.LinkedInURL)].LeadID)
	}
	out := make([]api.LeadID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// setReply keeps the latest automated reply by received time; a reply at the
// same time as the stored one replaces it, so the caller's order breaks ties.
func setReply(o *model.Outcome, status string, received time.Time) {
	if o.ReplyAt.IsZero() || !received.Before(o.ReplyAt) {
		o.ReplyStatus, o.ReplyAt = status, received
	}
}

// markHeld sets apollo_held_at on the first Apollo send, reply or unsubscribe
// event (RFC 6.7), kept at its earliest. Nothing clears it. It reports
// whether it changed the lead.
func markHeld(m *model.Model, lead api.LeadID, at time.Time) bool {
	p := m.People[model.Key(lead)]
	if !p.ApolloHeldAt.IsZero() && !at.Before(p.ApolloHeldAt) {
		return false
	}
	p.ApolloHeldAt = at
	m.Put(model.TablePeople, p)
	return true
}

// applyDeal sets the deal on every live lead at the company and every lead
// whose stored deal is this one (contracts section 5.3). The company is the
// event's domain (normalized), else the lead's. deal_stage holds the stage
// class from the kind (open, won or lost), which is what the status fold
// reads; the vendor's own stage name is stored nowhere.
func applyDeal(m *model.Model, lead api.LeadID, e api.Event, kind string, at time.Time) []api.LeadID {
	stage := strings.TrimPrefix(kind, "deal_")
	dealID := e.Attrs["deal_id"]
	domain := merge.NormalizeDomain(e.Domain)
	if lead != "" {
		lead = merge.Live(m, lead)
		if domain == "" {
			domain = m.People[model.Key(lead)].Fields[model.CompanyDomainField].Value
		}
	}
	targets := map[api.LeadID]bool{}
	if lead != "" {
		if _, ok := m.People[model.Key(lead)]; ok {
			targets[lead] = true
		}
	}
	if domain != "" {
		for _, id := range m.PeopleAt(domain) {
			if m.People[model.Key(id)].MergedInto == "" {
				targets[id] = true
			}
		}
	}
	if dealID != "" {
		for _, o := range m.Outcomes {
			if o.DealID == dealID {
				targets[o.LeadID] = true
			}
		}
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	var changed []api.LeadID
	for _, id := range ids {
		o := m.Outcomes[model.Key(id)]
		before := o
		o.LeadID = api.LeadID(id)
		o.DealID, o.DealStage, o.DealCheckedAt = dealID, stage, at
		if !reflect.DeepEqual(o, before) {
			m.Put(model.TableOutcomes, o)
			changed = append(changed, merge.Live(m, api.LeadID(id)))
		}
	}
	return changed
}
