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
	"github.com/HarshitBadhwar8/leadscore/internal/vendorhttp"
)

// The `apollo` sink: a destination
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
		return s.enroll(ctx, name, contactID, req.Lead)
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
// Before the add call it reads the contact and searches the team's contacts
// by email, so neither a repeat nor a duplicate contact can enroll anyone
// twice:
//   - the contact already in this sequence is a no-op (S0 confirms whether
//     Apollo's add call is itself one);
//   - the contact in any other sequence (active, paused or finished), or
//     opted out (email_unsubscribed, read whatever ContactOptOutFlag says), is
//     refused;
//   - any other contact with one of the lead's emails that is opted out or in
//     any sequence is refused: Apollo already holds that person.
//
// The add call also asks Apollo to skip contacts active or finished in other
// sequences, so a race between the reads and the add still refuses.
func (s *Sink) enroll(ctx context.Context, name, contactID string, lead api.LeadRef) (string, error) {
	seqID, err := s.sequenceID(ctx, name)
	if err != nil {
		return "", err
	}
	vendorID := seqID + ":" + contactID
	own, err := s.c.readContact(ctx, contactID)
	if err != nil {
		return "", classify(err)
	}
	if _, in := own.sequences[seqID]; in {
		return vendorID, nil
	}
	if own.optedOut {
		return "", fmt.Errorf("apollo: the contact is %s: %w", reasonOptedOut, api.ErrRefused)
	}
	if len(own.sequences) > 0 {
		return "", fmt.Errorf("apollo: the contact is %s: %w", reasonOtherSequence, api.ErrRefused)
	}
	emails := append([]string{own.email}, lead.Emails...)
	seen := map[string]bool{}
	for _, email := range emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		dups, err := s.c.contactsByEmail(ctx, email)
		if err != nil {
			return "", classify(err)
		}
		for _, d := range dups {
			switch {
			case d.id == contactID:
			case d.optedOut:
				return "", fmt.Errorf("apollo: another contact with the lead's email is %s: %w", reasonOptedOut, api.ErrRefused)
			case len(d.sequences) > 0:
				return "", fmt.Errorf("apollo: another contact with the lead's email is in a sequence: %w", api.ErrRefused)
			}
		}
	}
	reply, err := s.c.addToSequence(ctx, seqID, contactID, s.mailbox)
	var se *StatusError
	inSequence := errors.As(err, &se) && se.Status >= 400 && se.Status < 500 && refusalOf(errorFields(se.Body())) == reasonInSequence
	if err != nil && !inSequence {
		return "", classify(err)
	}
	if err == nil {
		err = addOutcome(reply, contactID)
		inSequence = errors.Is(err, errSaysInSequence)
	}
	if !inSequence {
		return vendorID, err
	}
	// "Already in this sequence" may be about another sequence (the words
	// are matched loosely): done only when the contact's record shows this
	// one, else one attempt.
	again, err := s.c.readContact(ctx, contactID)
	if err != nil {
		return "", classify(err)
	}
	if _, in := again.sequences[seqID]; in {
		return vendorID, nil
	}
	return "", errors.New("apollo: Apollo said the contact is already in the sequence, but its record does not show it")
}

// errSaysInSequence is addOutcome's answer when the skip says the contact is
// already in the sequence: enroll confirms it by reading the contact.
var errSaysInSequence = errors.New("apollo: the add reply says the contact is already in the sequence")

// addOutcome reads the add call's reply for one contact. The contact was
// added only when the reply's contacts list holds it and no skip payload
// mentions it. A skip for a recognised refusal is ErrRefused; "already in this
// sequence" is errSaysInSequence, for enroll to confirm; any other skip, or a reply that shows neither, counts
// one attempt (a skip means nobody was added, so a retry cannot contact the
// person twice; after three the step fails until a `retry`).
//
// S0 confirms the skip shape; until then a skip is any mention of the contact
// id under a key naming "skip": keyed by id, a plain list, a reason-to-ids
// map, or nested deeper.
func addOutcome(reply map[string]any, contactID string) error {
	var skipped bool
	var reasons []string
	for k, v := range reply {
		if !strings.Contains(strings.ToLower(k), "skip") {
			continue
		}
		if found, rs := mentions(v, contactID, nil); found {
			skipped = true
			reasons = append(reasons, rs...)
		}
	}
	if !skipped {
		if listed(reply["contacts"], contactID) {
			return nil
		}
		return errors.New("apollo: the add reply neither lists nor skips the contact")
	}
	switch reason := refusalOf(strings.Join(reasons, " ")); reason {
	case reasonInSequence:
		return errSaysInSequence
	case "":
		return errors.New("apollo: the contact was skipped for an unrecognised reason")
	default:
		return fmt.Errorf("apollo: the contact was skipped (%s): %w", reason, api.ErrRefused)
	}
}

// mentions reports whether v holds id anywhere (as a map key or a string
// value), with the words around it: the map keys on the way down and, for a
// map keyed by id, its value.
func mentions(v any, id string, path []string) (bool, []string) {
	switch t := v.(type) {
	case string:
		if t == id {
			return true, path
		}
	case []any:
		for _, e := range t {
			if ok, rs := mentions(e, id, path); ok {
				return true, rs
			}
		}
	case map[string]any:
		for k, e := range t {
			if k == id {
				rs := append([]string(nil), path...)
				return true, append(rs, words(e)...)
			}
			if ok, rs := mentions(e, id, append(append([]string(nil), path...), k)); ok {
				return true, rs
			}
		}
	}
	return false, nil
}

// words are the strings in v, for a skip reason given in any shape.
func words(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, words(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range t {
			out = append(append(out, k), words(e)...)
		}
		return out
	}
	return nil
}

// listed reports whether a contacts list holds the contact id.
func listed(v any, id string) bool {
	list, _ := v.([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any); ok && m["id"] == id {
			return true
		}
		if e == id {
			return true
		}
	}
	return false
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

// maxSearchPages bounds the sequence search; past it the answer is
// incomplete, and an incomplete answer is an error, never read as complete.
const maxSearchPages = 50

// ResolveSequence finds the id of the one sequence whose name is exactly
// name. Apollo's name filter is a keyword search, so every page is read and
// matched exactly. S0 confirms the call and the q_name filter.
func (c *Client) ResolveSequence(ctx context.Context, name string) (string, error) {
	var found []string
	err := eachPage(maxSearchPages, func(page int) (int, pagination, error) {
		var reply struct {
			Sequences []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"emailer_campaigns"`
			Pagination pagination `json:"pagination"`
		}
		body := map[string]any{"q_name": name, "page": page, "per_page": perPage}
		if err := c.Do(ctx, Request{Method: http.MethodPost, Path: sequencesSearchPath, Body: body}, &reply); err != nil {
			return 0, pagination{}, err
		}
		for _, sq := range reply.Sequences {
			if sq.Name == name && sq.ID != "" {
				found = append(found, sq.ID)
			}
		}
		return len(reply.Sequences), reply.Pagination, nil
	})
	if err != nil {
		return "", err
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

// apolloContact is what the enroll step reads of a contact.
type apolloContact struct {
	id, email string
	optedOut  bool
	sequences map[string]string // sequence id -> status (active, paused, finished, ...)
}

// contactRecord is the JSON of a contact. Statuses is a pointer so a reply
// without the field is told apart from a contact in no sequence.
type contactRecord struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	OptedOut *bool  `json:"email_unsubscribed"`
	Statuses *[]struct {
		SequenceID string `json:"emailer_campaign_id"`
		Status     string `json:"status"`
	} `json:"contact_campaign_statuses"`
}

func (r *contactRecord) read() (apolloContact, error) {
	if r == nil || r.Statuses == nil {
		return apolloContact{}, errors.New("apollo: a contact record came back without contact_campaign_statuses")
	}
	c := apolloContact{id: r.ID, email: r.Email, optedOut: r.OptedOut != nil && *r.OptedOut, sequences: map[string]string{}}
	for _, st := range *r.Statuses {
		if st.SequenceID != "" {
			c.sequences[st.SequenceID] = st.Status
		}
	}
	return c, nil
}

// readContact reads one contact. S0 confirms the call and the
// contact_campaign_statuses and email_unsubscribed fields.
func (c *Client) readContact(ctx context.Context, contactID string) (apolloContact, error) {
	var reply struct {
		Contact *contactRecord `json:"contact"`
	}
	path := contactsPath + "/" + url.PathEscape(contactID)
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path}, &reply); err != nil {
		return apolloContact{}, err
	}
	return reply.Contact.read()
}

// maxContactPages bounds a contact search for one email. An email matching
// more contacts than that is an error, never read as "no match".
const maxContactPages = 5

// contactsByEmail returns every team contact whose email is exactly email.
// The search is a keyword search, so near matches are dropped here. S0
// confirms the call, that it spends no credits, and the q_keywords filter.
func (c *Client) contactsByEmail(ctx context.Context, email string) ([]apolloContact, error) {
	var out []apolloContact
	err := eachPage(maxContactPages, func(page int) (int, pagination, error) {
		var reply struct {
			Contacts   []contactRecord `json:"contacts"`
			Pagination pagination      `json:"pagination"`
		}
		body := map[string]any{"q_keywords": email, "page": page, "per_page": perPage}
		if err := c.Do(ctx, Request{Method: http.MethodPost, Path: contactsSearchPath, Body: body}, &reply); err != nil {
			return 0, pagination{}, err
		}
		for i := range reply.Contacts {
			r := &reply.Contacts[i]
			if !strings.EqualFold(strings.TrimSpace(r.Email), email) {
				continue
			}
			ac, err := r.read()
			if err != nil {
				return 0, pagination{}, err
			}
			out = append(out, ac)
		}
		return len(reply.Contacts), reply.Pagination, nil
	})
	return out, err
}

// addToSequence adds one contact to a sequence, sending from the mailbox,
// and returns the reply for addOutcome to read.
//
// The flags ask Apollo to skip, not enroll, a contact active or finished in
// another sequence, with no email, or with an unverified email. S0 confirms
// the call and the flags.
func (c *Client) addToSequence(ctx context.Context, seqID, contactID, mailbox string) (map[string]any, error) {
	body := map[string]any{
		"emailer_campaign_id":                  seqID,
		"contact_ids":                          []string{contactID},
		"send_email_from_email_account_id":     mailbox,
		"sequence_active_in_other_campaigns":   false,
		"sequence_finished_in_other_campaigns": false,
		"sequence_no_email":                    false,
		"sequence_unverified_email":            false,
	}
	reply := map[string]any{}
	path := fmt.Sprintf(addContactsPathFmt, url.PathEscape(seqID))
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: body}, &reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// Refusal reasons, as named in errors (never Apollo's own text, which may
// quote the person).
const (
	reasonOtherSequence = "already in another sequence"
	reasonOptedOut      = "opted out"
	reasonInvalidEmail  = "invalid email"
	reasonInSequence    = "already in this sequence" // not a refusal: the enroll already happened
)

// refusalMarkers tell Apollo's refusal reasons apart, by words in a skip
// reason or an error reply's error and error_code fields, lowercased. S0
// confirms the real wording; these come from Apollo's public docs and UI.
// Order matters: the first match wins, and "this sequence" is checked before
// "another".
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

// errorFields is the text of an error reply's top-level error and error_code
// fields: the only part of a body a refusal is read from, since the rest may
// echo the contact back (an email like unsubscribe-me@..., a flag set false).
// S0 confirms Apollo's error shape.
func errorFields(body []byte) string {
	var reply map[string]any
	if json.Unmarshal(body, &reply) != nil {
		return ""
	}
	var parts []string
	for _, k := range []string{"error", "error_code"} {
		if v, ok := reply[k].(string); ok {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// classify maps a call's error to the sink error classes in internal/api (the
// status rule is vendorhttp's): a 429, 401 or 403 is ErrRateLimited (a
// refused key or a key without the scope is no lead's fault: the sink stops
// for the run with no attempt counted); a 5xx, a timeout or a transport
// failure is ErrTransient; a 4xx whose error fields name a refusal (another
// sequence, opted out, invalid email) is ErrRefused; any other error counts
// one attempt.
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
	if vendorhttp.KeyRefused(se.Status) {
		return fmt.Errorf("%w: apollo refused the key (it must be a master key): %w", api.ErrRateLimited, err)
	}
	if class := vendorhttp.Class(se.Status); class != nil {
		return fmt.Errorf("%w: %w", class, err)
	}
	if se.Status >= 400 {
		switch reason := refusalOf(errorFields(se.Body())); reason {
		case "", reasonInSequence:
			// Not a refusal; "already in this sequence" is read by the
			// enroll step itself, and anywhere else counts an attempt.
		default:
			return fmt.Errorf("apollo: refused (%s), status %d: %w", reason, se.Status, api.ErrRefused)
		}
	}
	return err
}
