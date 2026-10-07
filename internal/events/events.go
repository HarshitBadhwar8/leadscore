// Package events holds the event rules (RFC 6.7, contracts sections 5.3 and
// 12.7): the de-duplication key of every event, parsing stored receiver
// requests, and applying an event's effect to a lead's Outcomes and People.
//
// Apply is the only code that writes event effects. Losing an effect means an
// opt-out that never lands, so every rule here errs towards recording: an
// opt-out time is kept at its earliest, an automated opt-out always wins the
// origin, and nothing here ever clears one.
package events

import (
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
// source id.
const (
	OriginReceiver     = apollo.OriginReceiver
	OriginPolling      = "polling"
	OriginHubSpot      = "hubspot"
	OriginApolloLookup = "apollo_lookup"
)

// Unsubscribed origins (Outcomes.unsubscribed_origin).
const (
	UnsubEvent  = "event"
	UnsubLookup = "lookup"
)

// AttrReject marks a rejected event (Kind empty); its value is the reason.
const AttrReject = "reject"

// Key is an event's de-duplication key (RFC 6.7). The event's keys must
// already be normalized (merge.NormalizeEventKeys). Every free-text part is
// length-prefixed, so two different events never flatten to one key.
//
//   - A polled `reply`: its message id plus label (apollo.PolledReplyKey).
//   - A receiver visit: the person key (contact id, else email, else LinkedIn
//     URL), the kind and the vendor's visit time; with no usable visit time, a
//     hash of the stored body instead of the person and time. A company-only
//     visit uses the employer domain in place of the person.
//   - Any other receiver event (sent, replied, replied_positive,
//     unsubscribed): the kind, the conversation link (else the email) and the
//     stage, so a reply that moves the stage is a new event and a redelivery
//     of the same state is not.
//   - A lookup event: its kind, deal, person or company and time; lookup events
//     are not de-duplicated through Seen events, so this only names them.
//   - A source event row: the source id, the person key (email, else LinkedIn
//     URL, else domain), the kind and the event time.
func Key(e api.Event) api.EventID {
	kind := strings.ToLower(e.Kind)
	at := model.FormatTime(e.At)
	switch {
	case kind == "reply":
		return apollo.PolledReplyKey(e.Attrs[apollo.AttrMessageID], e.Attrs[apollo.AttrLabel])
	case e.Origin == OriginReceiver && strings.HasPrefix(kind, "visit_"):
		if h := e.Attrs[apollo.AttrBodyHash]; h != "" {
			return join("visit", kind, "body", h)
		}
		if p := visitPerson(e); p != "" {
			return join("visit", kind, "person", p, at)
		}
		return join("visit", kind, "company", e.Domain, at)
	case e.Origin == OriginReceiver:
		subject := e.Attrs[apollo.AttrConversationLink]
		if subject == "" {
			subject = e.Email
		}
		return join("apollo", kind, subject, e.Attrs[apollo.AttrStage])
	case e.Origin == OriginHubSpot || e.Origin == OriginApolloLookup:
		return join("lookup", e.Origin, kind, e.Attrs["deal_id"], personKey(e), at)
	}
	return join("source", e.Origin, personKey(e), kind, at)
}

// visitPerson is a receiver visit's person key: the vendor's contact id first,
// since it survives the vendor correcting the person's email.
func visitPerson(e api.Event) string {
	if id := strings.TrimSpace(e.Attrs[apollo.AttrContactID]); id != "" {
		return "id:" + id
	}
	if e.Email != "" {
		return "email:" + e.Email
	}
	if e.LinkedInURL != "" {
		return "linkedin:" + e.LinkedInURL
	}
	return ""
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
// body), so the caller logs it rather than dropping it unseen.
func Parse(raw []api.RawEvent) ([]api.Event, []api.InputRow) {
	var evs []api.Event
	var rows []api.InputRow
	for _, r := range raw {
		es, rs, err := apollo.ParseRaw(r)
		if err != nil {
			evs = append(evs, api.Event{
				Origin: OriginReceiver, ReceivedAt: r.ReceivedAt.UTC(), At: r.ReceivedAt.UTC(),
				Attrs: map[string]string{AttrReject: fmt.Sprintf("stored event %s (%s): %v", r.Seq, r.Kind, err)},
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
// reply_labels overrides (contracts section 5.5). ok is false for no outcome.
// `unsubscribe` cannot be overridden. A label the map does not know is still a
// reply, and reads as replied_unlabelled.
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

// Apply applies an event's section 5.3 effect to the live lead (following
// merged_into), and reports whether anything changed. The caller resolved the
// person: Intake through merge.ApplyEventPerson, the pre-push and re-read
// steps with the lead they looked up. lead may be empty for a company-level
// deal event. labels are the team's reply_labels overrides.
//
// A visit or custom kind has no outcome effect (it feeds detectors only).
// Applying an event twice changes nothing the second time.
func Apply(m *model.Model, lead api.LeadID, e api.Event, labels map[string]string) bool {
	kind := strings.ToLower(e.Kind)
	at := e.At.UTC()
	if at.IsZero() {
		at = e.ReceivedAt.UTC()
	}
	received := e.ReceivedAt.UTC()
	if received.IsZero() {
		received = at
	}
	if strings.HasPrefix(kind, "deal_") {
		return applyDeal(m, lead, e, kind, at)
	}
	if lead == "" {
		return false
	}
	lead = merge.Live(m, lead)
	if _, ok := m.People[model.Key(lead)]; !ok {
		return false
	}
	o := m.Outcomes[model.Key(lead)]
	before := o
	o.LeadID = lead
	held := false
	switch kind {
	case "sent":
		if o.ContactedAt.IsZero() || at.Before(o.ContactedAt) {
			o.ContactedAt = at
		}
		held = true
	case "replied":
		setReply(&o, "replied_neutral", received)
		held = true
	case "replied_positive":
		setReply(&o, "replied_positive", received)
		held = true
	case "unsubscribed":
		optOut(&o, at, UnsubEvent)
		held = true
	case "reply":
		if s, ok := ReplyStatus(e.Attrs[apollo.AttrLabel], labels); ok {
			if s == "unsubscribed" {
				optOut(&o, at, UnsubEvent)
			} else {
				setReply(&o, s, received)
			}
		}
		held = true
	case "optout":
		optOut(&o, at, UnsubLookup)
	default:
		return false
	}
	changed := !reflect.DeepEqual(o, before)
	if changed {
		m.Put(model.TableOutcomes, o)
	}
	if held && markHeld(m, lead, at) {
		changed = true
	}
	return changed
}

// optOut records an opt-out: the time kept at its earliest, the origin always
// the automated one, even over `manual` (contracts section 5.3).
func optOut(o *model.Outcome, at time.Time, origin string) {
	if o.UnsubscribedAt.IsZero() || at.Before(o.UnsubscribedAt) {
		o.UnsubscribedAt = at
	}
	o.UnsubscribedOrigin = origin
}

// setReply keeps the latest automated reply by received time; a reply at the
// same time as the stored one replaces it, so the caller's order breaks ties.
func setReply(o *model.Outcome, status string, received time.Time) {
	if o.ReplyAt.IsZero() || !received.Before(o.ReplyAt) {
		o.ReplyStatus, o.ReplyAt = status, received
	}
}

// markHeld sets apollo_held_at on the first Apollo send, reply or unsubscribe
// event (RFC 6.7). Nothing clears it.
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
// event's domain, else the lead's. deal_stage holds the stage class from the
// kind (open, won or lost), which is what the status fold reads.
func applyDeal(m *model.Model, lead api.LeadID, e api.Event, kind string, at time.Time) bool {
	stage := strings.TrimPrefix(kind, "deal_")
	dealID := e.Attrs["deal_id"]
	domain := e.Domain
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
	changed := false
	for _, id := range ids {
		o := m.Outcomes[model.Key(id)]
		before := o
		o.LeadID = api.LeadID(id)
		o.DealID, o.DealStage, o.DealCheckedAt = dealID, stage, at
		if !reflect.DeepEqual(o, before) {
			m.Put(model.TableOutcomes, o)
			changed = true
		}
	}
	return changed
}
