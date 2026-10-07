// Package apollo is the Apollo adapter. This part holds the parsers for the
// bodies the receiver stores from Apollo workflows: they turn one stored
// request into events and the receiver
// input row its person yields. Parsing is pure; the engine keys, resolves and
// applies what comes back (internal/events).
//
// The body shapes are built from our workflow templates; which fields
// Apollo's workflow variables can actually fill is marked "S0 confirms"
// where it matters.
package apollo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
)

// RawEvent kinds: which receiver route stored the body (section 5.1).
const (
	KindVisit = "apollo_visit" // POST /apollo/visit
	KindReply = "apollo_reply" // POST /apollo/reply
)

// OriginReceiver is Event.Origin, and the source id of input rows, for
// everything parsed from a stored body: merge's receiver source.
const OriginReceiver = merge.ReceiverSource

// MaxKindLen bounds an event kind. Kinds name State keys and first-seen map
// keys, so a body cannot grow them without limit.
const MaxKindLen = 64

// Event Attrs keys the parsers set.
const (
	AttrStage            = "stage"
	AttrConversationLink = "conversation_link"
	AttrContactID        = "contact_id"
	AttrFullName         = "full_name"
	AttrTitle            = "title"
	AttrCompany          = "company"
	AttrPage             = "page" // the visited page, when a body carries one (S0 confirms)
	// AttrNoVisitTime is "yes" on a visit with no usable visited_at: it is
	// timed at receipt and keyed by person, page and received day.
	AttrNoVisitTime = "no_visited_at"
	// AttrVisitedAt is the vendor's visit time as sent (UTC), before it is
	// clamped to the received time; the de-duplication key reads it.
	AttrVisitedAt = "visited_at"
	AttrLabel     = "label"      // polled replies
	AttrMessageID = "message_id" // polled replies
	// AttrNoReplyTime is "yes" on a polled reply Apollo gave no time for: it
	// is timed at the poll, and with no message id keyed without a time.
	AttrNoReplyTime = "no_reply_time"
)

// ErrIgnored marks a body of an engagement kind we do not act on (an open, a
// click, a bounce): not a reject, but logged so a workflow whose event literal
// is mistyped does not look like silence.
var ErrIgnored = errors.New("ignored")

// Receiver input row columns (section 5.1), in this order.
const (
	colContactID = "contact_id"
	colEmail     = "email"
	colLinkedIn  = "linkedin_url"
	colFullName  = "full_name"
	colTitle     = "title"
	colCompany   = "company.name"
	colDomain    = "company.domain"
)

// ParseRaw parses one stored request into its events and the receiver input
// row its person yields (none for a company-only visit). An unacted reply kind
// gives an error wrapping ErrIgnored. Any other error means the body cannot be
// applied; its text never quotes an email.
//
// Emails come back trimmed and lowercased and domains as lowercase hosts;
// the engine still normalizes every key once (merge.NormalizeEventKeys).
func ParseRaw(raw api.RawEvent) ([]api.Event, []api.InputRow, error) {
	var probe struct {
		NotJSON bool `json:"__not_json"`
	}
	if err := json.Unmarshal(raw.Body, &probe); err != nil {
		return nil, nil, errors.New("the stored body is not a JSON object")
	}
	if probe.NotJSON {
		return nil, nil, errors.New("the request body was not JSON")
	}
	switch raw.Kind {
	case KindVisit:
		return parseVisitRaw(raw)
	case KindReply:
		return parseReplyRaw(raw)
	}
	return nil, nil, fmt.Errorf("unknown stored kind %q", short(raw.Kind))
}

func parseReplyRaw(raw api.RawEvent) ([]api.Event, []api.InputRow, error) {
	n, err := parseNotification(raw.Body)
	if err != nil {
		return nil, nil, err
	}
	if !n.acts() {
		return nil, nil, fmt.Errorf("%w: engagement kind %q is not one we act on", ErrIgnored, short(n.Event))
	}
	at := raw.ReceivedAt.UTC() // reply bodies carry no vendor time (S0 confirms)
	e := api.Event{
		Kind:        replyKinds[n.Event],
		Email:       n.Email,
		LinkedInURL: n.LinkedinURL,
		Domain:      n.Domain,
		At:          at,
		ReceivedAt:  at,
		Origin:      OriginReceiver,
		Attrs:       attrs(AttrStage, n.Stage, AttrConversationLink, n.ConversationLink, AttrContactID, n.ContactID, AttrFullName, n.Name, AttrTitle, n.Title),
	}
	row := receiverRow(n.ContactID, n.Email, n.LinkedinURL, n.Name, n.Title, "", n.Domain)
	return []api.Event{e}, []api.InputRow{row}, nil
}

func parseVisitRaw(raw api.RawEvent) ([]api.Event, []api.InputRow, error) {
	v, err := parseVisit(raw.Body)
	if err != nil {
		return nil, nil, err
	}
	kind := v.kind()
	if kind == "" {
		return nil, nil, fmt.Errorf("a visit body's event must be %s<name>", visitEventPrefix)
	}
	if len(kind) > MaxKindLen {
		return nil, nil, fmt.Errorf("the visit's event name is longer than %d characters", MaxKindLen-len("visit_"))
	}
	employer := v.employerDomain()
	if !v.identifiable() && employer == "" {
		return nil, nil, errors.New("the visit names neither a contact nor a company")
	}
	received := raw.ReceivedAt.UTC()
	company := strings.TrimSpace(v.Contact.Company)
	if company == "" {
		company = strings.TrimSpace(v.Account.Name)
	}
	e := api.Event{
		Kind:       kind,
		Domain:     employer,
		ReceivedAt: received,
		Origin:     OriginReceiver,
		Attrs:      map[string]string{},
	}
	if t, ok := v.visitedAt(); ok {
		// A visit cannot happen after we received it: a clock ahead of ours
		// would otherwise park the visit in the future, outside every window.
		e.At = t
		e.Attrs[AttrVisitedAt] = t.Format("2006-01-02T15:04:05.000Z")
		if t.After(received) {
			e.At = received
		}
	} else {
		e.At = received
		e.Attrs[AttrNoVisitTime] = "yes"
	}
	if !v.identifiable() {
		if company != "" {
			e.Attrs[AttrCompany] = company
		}
		return []api.Event{e}, nil, nil
	}
	id := strings.TrimSpace(v.Contact.ID)
	li := strings.TrimSpace(v.Contact.LinkedinURL)
	e.Email, e.LinkedInURL = v.email(), li
	for k, val := range attrs(AttrContactID, id, AttrFullName, v.fullName(), AttrTitle, strings.TrimSpace(v.Contact.Title), AttrCompany, company) {
		e.Attrs[k] = val
	}
	row := receiverRow(id, v.email(), li, v.fullName(), strings.TrimSpace(v.Contact.Title), company, employer)
	return []api.Event{e}, []api.InputRow{row}, nil
}

// short caps a body value quoted in an error, so a log line stays small.
func short(s string) string {
	if len(s) > MaxKindLen {
		return s[:MaxKindLen] + "..."
	}
	return s
}

// attrs builds an Attrs map from key, value pairs, leaving empty values out.
func attrs(kv ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

// receiverRow is the input row a body's person yields under source
// `receiver`: only the columns the body carries, in section 5.1's order.
func receiverRow(contactID, email, linkedin, name, title, company, domain string) api.InputRow {
	row := api.InputRow{SourceID: OriginReceiver, Columns: map[string]string{}}
	for _, c := range [][2]string{
		{colContactID, contactID}, {colEmail, email}, {colLinkedIn, linkedin}, {colFullName, name},
		{colTitle, title}, {colCompany, company}, {colDomain, domain},
	} {
		if c[1] != "" {
			row.Headers = append(row.Headers, c[0])
			row.Columns[c[0]] = c[1]
		}
	}
	return row
}

// PolledReplyKey is the de-duplication key of a polled reply: its message id
// plus its label (lowercased), so a label the team changes later is a new
// fact. Both parts are length-prefixed, so no two pairs can flatten
// to one key. A reply with no message id is keyed by events.Key instead.
func PolledReplyKey(messageID, label string) api.EventID {
	label = strings.ToLower(strings.TrimSpace(label))
	return api.EventID("polled:" + strconv.Itoa(len(messageID)) + ":" + messageID + ":" + label)
}

// RequiredPaths are the body fields the parsers read, as dotted JSON paths at
// their original places (the oversized-body rule). A receiver cutting an
// oversized body down keeps exactly these: the keys, the times and the
// person and company fields that make the receiver row.
func RequiredPaths() []string {
	return []string{
		"event",
		// Reply workflow.
		"contact_email", "contact_stage", "last_conversation_link", "contact_id",
		"contact_name", "contact_title", "contact_linkedin_url", "account_domain",
		// Visit workflow.
		"visited_at", "contact.id", "contact.email", "contact.linkedin_url",
		"contact.first_name", "contact.last_name", "contact.title", "contact.company",
		"account.domain", "account.website_url", "account.name",
	}
}
