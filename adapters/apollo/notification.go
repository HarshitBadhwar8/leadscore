package apollo

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Engagement kinds the reply workflow sends. These are
// our names, not the vendor's: the workflow body is typed by the team from our
// templates, so the spelling is a contract we set. Each maps to one of the
// event kinds in internal/events.
const (
	kindEmailSent            = "email_sent"
	kindEmailReplied         = "email_replied"
	kindEmailUnsubscribe     = "email_unsubscribed"
	kindEmailRepliedPositive = "email_replied_positive"
)

// replyKinds maps an acted engagement kind to its event kind.
var replyKinds = map[string]string{
	kindEmailSent:            "sent",
	kindEmailReplied:         "replied",
	kindEmailRepliedPositive: "replied_positive",
	kindEmailUnsubscribe:     "unsubscribed",
}

// Reserved stage markers. An unsubscribe and a workflow-judged positive reply
// arrive as their own engagement kinds, never as a stage; a delivered stage
// spelled like one of these markers is a misconfigured workflow or a forgery,
// and is refused so that nothing downstream ever reads it as an opt-out or a
// positive reply.
const (
	stageUnsubscribed    = "__unsubscribed__"
	stageRepliedPositive = "__replied_positive__"
)

// reservedStages is the closed set parseNotification refuses as a delivered
// contact_stage. A third marker must join this set to be refused.
var reservedStages = map[string]struct{}{
	stageUnsubscribed:    {},
	stageRepliedPositive: {},
}

// notification is one reply-workflow body: a sent, reply, positive reply or
// unsubscribe for one contact.
//
// It carries identity, never content: the workflow can fill contact and
// account fields, not the message itself. The conversation summary and
// transcript fields the workflow may add are not read.
//
// Strictness is the point for event and email. A body missing one cannot be
// pinned on the right person, and a lenient fallback would record an opt-out
// or a reply against an empty or wrong key. A loud reject is recoverable; a
// quiet mis-record is not.
type notification struct {
	// Event names the engagement kind: a literal typed into each workflow, since
	// the vendor has no engagement-kind variable.
	Event string `json:"event"`
	// Email is the prospect's address and the match key every reply body carries.
	Email string `json:"contact_email"`
	// Stage is the vendor's own stage for the person, passed through as text. It
	// is part of the de-duplication key and never decides a status.
	Stage string `json:"contact_stage"`
	// ConversationLink identifies the exchange; the de-duplication key is built
	// from it, falling back to the email.
	ConversationLink string `json:"last_conversation_link"`

	// The person and company fields the reply template adds; each is
	// optional, since a workflow may not be able to fill it. ContactID is
	// not in the template: the variable catalogue seen on 2026-08-19 lists
	// no contact id (another token is not ruled out). It is read only if a
	// team adds one; without it the reply is matched by email.
	ContactID   string `json:"contact_id"`
	Name        string `json:"contact_name"`
	Title       string `json:"contact_title"`
	LinkedinURL string `json:"contact_linkedin_url"`
	Domain      string `json:"account_domain"`
}

// parseNotification decodes one reply-workflow body. An unacted kind (an open,
// a click, a bounce) comes back with no error and acts() false.
func parseNotification(body []byte) (notification, error) {
	var n notification
	if err := json.Unmarshal(body, &n); err != nil {
		return notification{}, fmt.Errorf("decoding a reply-workflow body: %w", err)
	}
	n.Event = strings.TrimSpace(n.Event)
	n.Email = strings.ToLower(strings.TrimSpace(n.Email))
	n.Stage = strings.TrimSpace(n.Stage)
	n.ConversationLink = strings.TrimSpace(n.ConversationLink)
	n.ContactID = strings.TrimSpace(n.ContactID)
	n.Name = strings.TrimSpace(n.Name)
	n.Title = strings.TrimSpace(n.Title)
	n.LinkedinURL = strings.TrimSpace(n.LinkedinURL)
	n.Domain = strings.TrimSpace(n.Domain)

	if n.Event == "" {
		return notification{}, fmt.Errorf("missing event")
	}
	if !n.acts() {
		// An unacted kind is ignored, not refused: validating identity on it
		// would reject deliveries nothing reads.
		return n, nil
	}
	// A missing stage is not an error: the engagement is still worth
	// recording. A missing email is, since it is the only key every body has.
	if n.Email == "" {
		return notification{}, fmt.Errorf("missing contact_email on an actionable %q notification", n.Event)
	}
	if _, reserved := reservedStages[n.Stage]; reserved {
		return notification{}, fmt.Errorf("contact_stage %q is reserved and cannot be delivered as a stage", n.Stage)
	}
	return n, nil
}

// acts reports whether the engagement kind is one we record. Opens and clicks
// describe an inbox, bounces an address; neither is a prospect's intent.
func (n notification) acts() bool {
	_, ok := replyKinds[n.Event]
	return ok
}
