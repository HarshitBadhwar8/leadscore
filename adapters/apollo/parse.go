// Package apollo is the Apollo adapter. This part holds the parsers for the
// bodies the receiver stores from Apollo workflows (contracts section 5.1):
// they turn one stored request into events (section 5.3) and the receiver
// input row its person yields. Parsing is pure; the engine keys, resolves and
// applies what comes back (internal/events).
package apollo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// RawEvent kinds: which receiver route stored the body (section 5.1).
const (
	KindVisit = "apollo_visit" // POST /apollo/visit
	KindReply = "apollo_reply" // POST /apollo/reply
)

// OriginReceiver is Event.Origin, and the source id of input rows, for
// everything parsed from a stored body.
const OriginReceiver = "receiver"

// Event Attrs keys the parsers set.
const (
	AttrStage            = "stage"
	AttrConversationLink = "conversation_link"
	AttrContactID        = "contact_id"
	AttrFullName         = "full_name"
	AttrTitle            = "title"
	AttrCompany          = "company"
	// AttrBodyHash is set on a visit with no usable visited_at: the hex SHA-256
	// of its stored body, which keys it instead (RFC 6.7).
	AttrBodyHash  = "body_sha256"
	AttrLabel     = "label"      // polled replies
	AttrMessageID = "message_id" // polled replies
)

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
// (an open, a click) gives nothing and no error. An error means the body
// cannot be applied; its text never quotes an email.
//
// Keys come back trimmed only; the engine normalizes them once.
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
	return nil, nil, fmt.Errorf("unknown stored kind %q", raw.Kind)
}

func parseReplyRaw(raw api.RawEvent) ([]api.Event, []api.InputRow, error) {
	n, err := parseNotification(raw.Body)
	if err != nil {
		return nil, nil, err
	}
	if !n.acts() {
		return nil, nil, nil
	}
	at := raw.ReceivedAt.UTC() // reply bodies carry no vendor time
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
	}
	if t, ok := v.visitedAt(); ok {
		e.At = t
		e.Attrs = map[string]string{}
	} else {
		e.At = received
		e.Attrs = map[string]string{AttrBodyHash: bodyHash(raw.Body)}
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
// plus its label, so a label the team changes later is a new fact (RFC 6.7).
// Both parts are length-prefixed, so no two pairs can flatten to one key.
func PolledReplyKey(messageID, label string) api.EventID {
	return api.EventID("polled:" + strconv.Itoa(len(messageID)) + ":" + messageID + ":" + label)
}

// RequiredPaths are the body fields the parsers read, as dotted JSON paths at
// their original places (contracts section 5.4). A receiver cutting an
// oversized body down keeps exactly these.
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
