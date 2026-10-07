package apollo

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The reply Poller: with
// `replies: polling`, the engine asks it for the replies to sequence emails
// since a time it works out, and keys, de-duplicates and applies them itself.

// KindPolledReply is the kind of a polled reply event.
const KindPolledReply = "reply"

// Poller reads replied sequence emails from Apollo's email search.
type Poller struct {
	c   *Client
	now func() time.Time
}

// NewPoller builds the poller from sinks.apollo: the key from APOLLO_API_KEY
// and the test keys base_url and _http_client.
func NewPoller(cfg api.Config) (api.Poller, error) {
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Poller{c: c, now: time.Now}, nil
}

// Poll returns one `reply` event per replied email sent on or after since, with
// the reply's label and the email's message id in Attrs, and the contact id
// when Apollo gives one. It uses since as given: the window is the engine's to
// choose, and Apollo's date filter is by day, so the poll starts at since's UTC
// day and may return a little more, which the engine's keys de-duplicate. A
// label Apollo adds later is a new event, since the key is the message id plus
// the label (events.Key).
//
// Every page is read; any failure (a 429 included) fails the whole poll, so
// the engine keeps last_poll_at and reads the same window next time.
//
// A live API test called this search as a POST with a JSON body and showed
// that the replied filter is applied by Apollo (every record it returned had
// replied true), that the label field is reply_class (null on most replies),
// and that each message carries to_email. S0 confirms: the date filter by day
// on completed_at, and the reply time's field (replied_at, else the send's
// completed_at).
func (p *Poller) Poll(ctx context.Context, since time.Time) ([]api.Event, error) {
	var out []api.Event
	err := eachPage(maxPollPages, func(page int) (int, pagination, error) {
		body := map[string]any{
			"emailer_message_stats":           []string{"replied"},
			"emailer_message_date_range_mode": "completed_at",
			"emailerMessageDateRange":         map[string]string{"min": since.UTC().Format(time.DateOnly)},
			"page":                            page,
			"per_page":                        perPage,
		}
		var reply struct {
			Messages   []polledMessage `json:"emailer_messages"`
			Pagination pagination      `json:"pagination"`
		}
		if err := p.c.Do(ctx, Request{Method: http.MethodPost, Path: messagesSearchPath, Body: body}, &reply); err != nil {
			return 0, pagination{}, err
		}
		for _, m := range reply.Messages {
			if e, ok := m.event(p.now().UTC()); ok {
				out = append(out, e)
			}
		}
		return len(reply.Messages), reply.Pagination, nil
	})
	if err != nil {
		return nil, fmt.Errorf("apollo: reading replies: %w", err)
	}
	return out, nil
}

// maxPollPages bounds one poll (Apollo's search stops at 50,000 records:
// 500 pages of 100).
const maxPollPages = 500

// polledMessage is the part of an emailer message a reply event reads.
type polledMessage struct {
	ID          string `json:"id"`
	ContactID   string `json:"contact_id"`
	ToEmail     string `json:"to_email"`
	ReplyClass  string `json:"reply_class"`
	RepliedAt   string `json:"replied_at"`
	CompletedAt string `json:"completed_at"`
}

// event is the message as a polled `reply`. A message naming no person
// (neither an email nor a contact id) is dropped: there is no lead to apply it
// to. With no usable time it is timed at the poll and marked no_reply_time.
func (m polledMessage) event(now time.Time) (api.Event, bool) {
	email := strings.ToLower(strings.TrimSpace(m.ToEmail))
	contactID := strings.TrimSpace(m.ContactID)
	if email == "" && contactID == "" {
		return api.Event{}, false
	}
	e := api.Event{
		Kind:   KindPolledReply,
		Email:  email,
		Origin: "polling", // the engine forces this whatever is set
		Attrs: attrs(AttrMessageID, strings.TrimSpace(m.ID), AttrLabel, strings.ToLower(strings.TrimSpace(m.ReplyClass)),
			AttrContactID, contactID),
	}
	for _, s := range []string{m.RepliedAt, m.CompletedAt} {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
			e.At, e.ReceivedAt = t.UTC(), t.UTC()
			return e, true
		}
	}
	// No time Apollo gave: timed at the poll for its effects, and marked so
	// that a reply with no message id is keyed without the poll's time
	// (events.Key), else every poll would read it as a new reply.
	e.At, e.ReceivedAt = now, now
	e.Attrs[AttrNoReplyTime] = "yes"
	return e, true
}
