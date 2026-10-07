package apollo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The `apollo` sink (contracts sections 1 and 6, RFC 6.11): a destination
// `sequence/<name>` has two steps, `contact` (create the contact, with
// Apollo's own de-duplication on) and `enroll` (add it to the sequence).
//
// Everything below about Apollo's calls is taken from its public API docs, not
// from S0's captures: the paths, the field names, and how a refusal looks are
// marked "S0 confirms".

func init() {
	api.RegisterSink("apollo", NewSink)
	api.RegisterPoller("apollo", NewPoller)
	if ContactOptOutFlag {
		api.RegisterLookup("apollo", NewLookup)
	}
}

// Steps of a sequence destination.
const (
	StepContact = "contact"
	StepEnroll  = "enroll"
)

// SequencePrefix starts every Apollo destination: apollo:sequence/<name>.
const SequencePrefix = "sequence/"

// Paths of the outreach calls. S0 confirms each one.
const (
	contactsPath        = "/api/v1/contacts"                 // POST: create; GET /<id>: read one
	contactsSearchPath  = "/api/v1/contacts/search"          // POST: search the team's contacts (no credits)
	sequencesSearchPath = "/api/v1/emailer_campaigns/search" // POST: find sequences by name
	emailAccountsPath   = "/api/v1/email_accounts"           // GET: the team's sending mailboxes
	messagesSearchPath  = "/api/v1/emailer_messages/search"  // GET: sent emails, filtered to replied
	addContactsPathFmt  = "/api/v1/emailer_campaigns/%s/add_contact_ids"
)

// perPage is the page size asked of every search.
const perPage = 100

// SequenceName returns the sequence name a destination names, and whether
// the destination is a sequence destination at all.
func SequenceName(dest string) (string, bool) {
	name, ok := strings.CutPrefix(dest, SequencePrefix)
	if !ok || strings.TrimSpace(name) == "" {
		return "", false
	}
	return name, true
}

// MailboxID reads sinks.apollo.mailbox_id: the sending mailbox (an email
// account id) enrollment sends from. It must be text; an unquoted id that
// YAML read as a number is refused rather than guessed back into text.
func MailboxID(cfg api.Config) (string, error) {
	v, ok := cfg["mailbox_id"]
	if !ok || v == nil {
		return "", errors.New("apollo: sinks.apollo.mailbox_id is not set; enrollment needs the sending mailbox's id")
	}
	s, isStr := v.(string)
	if !isStr {
		return "", errors.New("apollo: sinks.apollo.mailbox_id must be text; quote it in leadscore.yml")
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", errors.New("apollo: sinks.apollo.mailbox_id is empty")
	}
	return s, nil
}

// Sink is the `apollo` sink. The engine builds one per run, so sequence ids
// resolved by name are cached for the run and a renamed sequence is seen by
// the next one.
type Sink struct {
	c       *Client
	mailbox string

	mu        sync.Mutex
	sequences map[string]string // name -> id, for names resolved this run
}

// NewSink builds the sink from sinks.apollo: the key from APOLLO_API_KEY,
// mailbox_id, and the test keys base_url and _http_client.
func NewSink(cfg api.Config) (api.Sink, error) {
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	mailbox, err := MailboxID(cfg)
	if err != nil {
		return nil, err
	}
	return &Sink{c: c, mailbox: mailbox, sequences: map[string]string{}}, nil
}

// Steps returns {contact, enroll} for sequence/<name>, and nothing for any
// other destination.
func (s *Sink) Steps(dest string) []string {
	if _, ok := SequenceName(dest); !ok {
		return nil
	}
	return []string{StepContact, StepEnroll}
}

// Do runs one step. Both are find-or-create: the contact step lets Apollo
// match its own record for the email, and the enroll step reads the
// contact's sequences first and does nothing when it is already in this one.
func (s *Sink) Do(ctx context.Context, req api.StepRequest) (string, error) {
	name, ok := SequenceName(req.Dest)
	if !ok {
		return "", fmt.Errorf("apollo: destination %q is not sequence/<name>", req.Dest)
	}
	switch req.Key.Step {
	case StepContact:
		return s.contact(ctx, req.Lead)
	case StepEnroll:
		contactID := req.Prior[StepContact]
		if contactID == "" {
			return "", errors.New("apollo: the enroll step needs the contact step's id")
		}
		return s.enroll(ctx, name, contactID)
	}
	return "", fmt.Errorf("apollo: unknown step %q", req.Key.Step)
}

// NewContact is a lead being created as an Apollo contact.
type NewContact struct {
	Email      string
	FullName   string
	Title      string
	Company    string
	WebsiteURL string
}

func (s *Sink) contact(ctx context.Context, lead api.LeadRef) (string, error) {
	nc := NewContact{FullName: lead.FullName, Title: lead.Title, Company: lead.Fields["company.name"]}
	if len(lead.Emails) > 0 {
		nc.Email = lead.Emails[0]
	}
	if lead.Domain != "" {
		nc.WebsiteURL = "https://" + lead.Domain
	}
	id, err := s.c.CreateContact(ctx, nc)
	return id, classify(err)
}

// CreateContact creates a contact and returns Apollo's id for it.
//
// run_dedupe is always on, so a step replayed after a crash (the create went
// out, the id was never saved) gets the contact Apollo already holds for the
// email rather than a second one. S0 confirms that run_dedupe returns the
// existing contact's id.
func (c *Client) CreateContact(ctx context.Context, contact NewContact) (string, error) {
	email := strings.TrimSpace(contact.Email)
	if email == "" {
		return "", fmt.Errorf("apollo: a contact needs an email: %w", api.ErrRefused)
	}
	first, last := splitName(contact.FullName)
	body := map[string]any{
		"email":      email,
		"run_dedupe": true,
	}
	// Sent only when present: an empty string is a value Apollo would store,
	// overwriting a name it may already hold for this person.
	for key, value := range map[string]string{
		"first_name":        first,
		"last_name":         last,
		"title":             contact.Title,
		"organization_name": contact.Company,
		"website_url":       contact.WebsiteURL,
	} {
		if value != "" {
			body[key] = value
		}
	}
	var reply struct {
		Contact struct {
			ID string `json:"id"`
		} `json:"contact"`
	}
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: contactsPath, Body: body}, &reply); err != nil {
		return "", err
	}
	if reply.Contact.ID == "" {
		// Without an id there is nothing to save, so the next run would
		// create the contact again: fail loudly rather than duplicate.
		return "", errNoID
	}
	return reply.Contact.ID, nil
}

// errNoID is a 2xx create with no contact id in it: counted as an attempt,
// not retried for free, since the next reply will likely be the same.
var errNoID = errors.New("apollo: creating the contact returned no id")

// splitName splits a display name into the two fields Apollo takes: the first
// word is the given name and the rest the family name, which keeps multi-part
// surnames ("van der Berg") whole and leaves a mononym's family name empty.
// It is wrong for name orders it cannot know, which is why the lead's own
// full name stays the record of truth.
func splitName(full string) (first, last string) {
	fields := strings.Fields(full)
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], strings.Join(fields[1:], " ")
	}
}

// enroll adds the contact to the named sequence. The vendor id is the
// sequence id and the contact id, so a repeat returns the same id.
//
// S0 confirms whether Apollo's add call is itself a no-op for a contact
// already in the sequence. Until then the step reads the contact first: in
// this sequence already is a no-op; in any other sequence (active, paused or
// finished) is a refusal, since Apollo already works or worked that person.
// The add call also asks Apollo to skip contacts active or finished in other
// sequences, so a race between the read and the add still refuses.
func (s *Sink) enroll(ctx context.Context, name, contactID string) (string, error) {
	seqID, err := s.sequenceID(ctx, name)
	if err != nil {
		return "", err
	}
	vendorID := seqID + ":" + contactID
	statuses, err := s.c.contactSequences(ctx, contactID)
	if err != nil {
		return "", classify(err)
	}
	if _, in := statuses[seqID]; in {
		return vendorID, nil
	}
	if len(statuses) > 0 {
		return "", fmt.Errorf("apollo: the contact is already in another sequence: %w", api.ErrRefused)
	}
	skipped, err := s.c.addToSequence(ctx, seqID, contactID, s.mailbox)
	if err != nil {
		return "", classify(err)
	}
	if skipped != nil {
		switch reason := refusalOf(skipped.reason); reason {
		case reasonInSequence:
			return vendorID, nil
		case "":
			return "", fmt.Errorf("apollo: the contact was skipped for an unrecognized reason: %w", api.ErrRefused)
		default:
			return "", fmt.Errorf("apollo: the contact was skipped (%s): %w", reason, api.ErrRefused)
		}
	}
	return vendorID, nil
}

// sequenceID resolves a sequence name once per run. An unresolved name
// (missing, or more than one sequence by that name) is ErrTransient: the
// step waits until the name is fixed, and the apollo-sequences check says
// which lane names it.
func (s *Sink) sequenceID(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	id, ok := s.sequences[name]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	id, err := s.c.ResolveSequence(ctx, name)
	switch {
	case errors.Is(err, ErrSequenceNotFound), errors.Is(err, ErrSequenceAmbiguous):
		return "", fmt.Errorf("%w: %w", api.ErrTransient, err)
	case err != nil:
		return "", classify(err)
	}
	s.mu.Lock()
	s.sequences[name] = id
	s.mu.Unlock()
	return id, nil
}

// Sequence name resolution errors.
var (
	ErrSequenceNotFound  = errors.New("apollo: no sequence has that name")
	ErrSequenceAmbiguous = errors.New("apollo: more than one sequence has that name")
)

// maxSearchPages bounds a paged search; past it the answer is incomplete, and
// an incomplete answer is reported as an error rather than read as complete.
const maxSearchPages = 50

type pagination struct {
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

// ResolveSequence finds the id of the one sequence whose name is exactly
// name. Apollo's name filter is a keyword search, so every page is read and
// matched exactly. S0 confirms the call and the q_name filter.
func (c *Client) ResolveSequence(ctx context.Context, name string) (string, error) {
	var found []string
	for page := 1; ; page++ {
		if page > maxSearchPages {
			return "", fmt.Errorf("apollo: the sequence search for a lane's name has more than %d pages", maxSearchPages)
		}
		var reply struct {
			Sequences []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"emailer_campaigns"`
			Pagination pagination `json:"pagination"`
		}
		body := map[string]any{"q_name": name, "page": page, "per_page": perPage}
		if err := c.Do(ctx, Request{Method: http.MethodPost, Path: sequencesSearchPath, Body: body}, &reply); err != nil {
			return "", err
		}
		for _, sq := range reply.Sequences {
			if sq.Name == name && sq.ID != "" {
				found = append(found, sq.ID)
			}
		}
		if page >= reply.Pagination.TotalPages {
			break
		}
	}
	switch len(found) {
	case 0:
		return "", ErrSequenceNotFound
	case 1:
		return found[0], nil
	}
	return "", ErrSequenceAmbiguous
}

// EmailAccountIDs lists the ids of the team's sending mailboxes. S0 confirms
// the call and that one page holds them all.
func (c *Client) EmailAccountIDs(ctx context.Context) ([]string, error) {
	var reply struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"email_accounts"`
	}
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: emailAccountsPath}, &reply); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(reply.Accounts))
	for _, a := range reply.Accounts {
		ids = append(ids, a.ID)
	}
	return ids, nil
}

// contactSequences reads a contact's sequence memberships: sequence id to
// status (active, paused, finished, ...). S0 confirms the call and the
// contact_campaign_statuses field.
func (c *Client) contactSequences(ctx context.Context, contactID string) (map[string]string, error) {
	var reply struct {
		Contact struct {
			Statuses []struct {
				SequenceID string `json:"emailer_campaign_id"`
				Status     string `json:"status"`
			} `json:"contact_campaign_statuses"`
		} `json:"contact"`
	}
	path := contactsPath + "/" + url.PathEscape(contactID)
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path}, &reply); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, st := range reply.Contact.Statuses {
		if st.SequenceID != "" {
			out[st.SequenceID] = st.Status
		}
	}
	return out, nil
}

// skip is Apollo's answer that it did not add the contact, with its reason
// (empty when it gave none).
type skip struct{ reason string }

// addToSequence adds one contact to a sequence, sending from the mailbox. A
// 2xx that names the contact among the skipped ones returns that skip.
//
// The flags ask Apollo to skip, not enroll, a contact active or finished in
// another sequence, with no email, or with an unverified email. S0 confirms
// the call, the flags and the skipped_contact_ids shape (taken here as a map
// of contact id to reason, or a plain list of ids).
func (c *Client) addToSequence(ctx context.Context, seqID, contactID, mailbox string) (*skip, error) {
	body := map[string]any{
		"emailer_campaign_id":                  seqID,
		"contact_ids":                          []string{contactID},
		"send_email_from_email_account_id":     mailbox,
		"sequence_active_in_other_campaigns":   false,
		"sequence_finished_in_other_campaigns": false,
		"sequence_no_email":                    false,
		"sequence_unverified_email":            false,
	}
	var reply struct {
		Skipped any `json:"skipped_contact_ids"`
	}
	path := fmt.Sprintf(addContactsPathFmt, url.PathEscape(seqID))
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: body}, &reply); err != nil {
		return nil, err
	}
	switch sk := reply.Skipped.(type) {
	case map[string]any:
		if r, ok := sk[contactID]; ok {
			reason, _ := r.(string)
			return &skip{reason: reason}, nil
		}
	case []any:
		for _, id := range sk {
			if id == contactID {
				return &skip{}, nil
			}
		}
	}
	return nil, nil
}

// Refusal reasons, as named in errors (never Apollo's own text, which may
// quote the person).
const (
	reasonOtherSequence = "already in another sequence"
	reasonOptedOut      = "opted out"
	reasonInvalidEmail  = "invalid email"
	reasonInSequence    = "already in this sequence" // not a refusal: the enroll already happened
)

// refusalMarkers tell Apollo's refusal reasons apart, by words in its reply
// (a skip reason, or a 4xx body), lowercased. S0 confirms the real wording;
// these come from Apollo's public docs and UI. Order matters: the first
// match wins, and "this sequence" is checked before "another".
var refusalMarkers = []struct{ marker, reason string }{
	{"already_in_campaign", reasonInSequence},
	{"already in this sequence", reasonInSequence},
	{"contact_already_exists_in_campaign", reasonInSequence},
	{"other_campaign", reasonOtherSequence},
	{"other campaign", reasonOtherSequence},
	{"another sequence", reasonOtherSequence},
	{"other sequence", reasonOtherSequence},
	{"unsubscribe", reasonOptedOut},
	{"opted out", reasonOptedOut},
	{"opted_out", reasonOptedOut},
	{"do_not_contact", reasonOptedOut},
	{"do not contact", reasonOptedOut},
	{"invalid_email", reasonInvalidEmail},
	{"invalid email", reasonInvalidEmail},
	{"email is invalid", reasonInvalidEmail},
	{"no_email", reasonInvalidEmail},
	{"without_email", reasonInvalidEmail},
	{"unverified_email", reasonInvalidEmail},
	{"bounced", reasonInvalidEmail},
}

// refusalOf names the refusal a reply's text describes, or "".
func refusalOf(text string) string {
	text = strings.ToLower(text)
	for _, m := range refusalMarkers {
		if strings.Contains(text, m.marker) {
			return m.reason
		}
	}
	return ""
}

// classify maps a call's error to section 1's classes (contracts section 6):
// a 429 is already ErrRateLimited; a 5xx, a timeout or a transport failure is
// ErrTransient; a 4xx whose body names a refusal (another sequence, opted
// out, invalid email) is ErrRefused; any other error counts one attempt.
//
// None of these says Apollo did nothing: the ledger records that the call
// went out, so a timeout still holds the person's one cold push.
func classify(err error) error {
	if err == nil || errors.Is(err, api.ErrRateLimited) || errors.Is(err, api.ErrRefused) || errors.Is(err, api.ErrTransient) {
		return err
	}
	var se *StatusError
	if !errors.As(err, &se) {
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		if errors.Is(err, errNoID) || errors.As(err, &syn) || errors.As(err, &typ) {
			return err // a 2xx we cannot read: likely the same next time, so it counts
		}
		return fmt.Errorf("%w: %w", api.ErrTransient, err)
	}
	if se.Status >= 500 {
		return fmt.Errorf("%w: %w", api.ErrTransient, err)
	}
	if se.Status >= 400 {
		switch reason := refusalOf(string(se.Body())); reason {
		case "", reasonInSequence:
			// "Already in this sequence" as an error reply is left to count
			// an attempt: the enroll step reads the contact first, so it
			// should never get here, and S0 confirms the shape.
		default:
			return fmt.Errorf("apollo: refused (%s), status %d: %w", reason, se.Status, api.ErrRefused)
		}
	}
	return err
}
