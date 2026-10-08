// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package apollo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The Apollo contact Lookup: just before a push, it finds each lead's Apollo
// contacts by email and reports an `optout` for any that carries Apollo's
// opt-out flag. It catches a person who clicked an unsubscribe link without
// replying, which neither polling nor HubSpot may show.
//
// It is registered only when ContactOptOutFlag is true, which it becomes only
// once the flag is confirmed:
// without the flag it could only ever say "not opted out", which is no check
// at all, and the apollo-key check warns Apollo-only teams instead.

// KindOptOut is the kind of a lookup's opt-out event.
const KindOptOut = "optout"

// optOutField is the contact's opt-out flag. Unconfirmed: its name, that it is
// always present (true or false), that the search spends no credits, and
// that q_keywords matches by email.
const optOutField = "email_unsubscribed"

// Lookup is the `apollo` lookup.
type Lookup struct{ c *Client }

// NewLookup builds the lookup from sinks.apollo: the key from APOLLO_API_KEY
// and the test keys base_url and _http_client.
func NewLookup(cfg api.Config) (api.Lookup, error) {
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Lookup{c: c}, nil
}

// Lookup checks every email of every lead. A lead whose search failed is in
// failed, and the engine holds it back; one opted-out contact is enough for
// an `optout` event, which carries the email and the contact id.
//
// A rate limit, or the context ending, fails the lead being looked up and
// every lead after it (they were not looked up), and keeps what was learned
// so far. A key Apollo refuses fails the whole lookup.
func (l *Lookup) Lookup(ctx context.Context, leads []api.LeadRef) ([]api.Event, map[api.LeadID]error, error) {
	var evs []api.Event
	failed := map[api.LeadID]error{}
	for i, lead := range leads {
		seen := map[string]bool{}
	emails:
		for _, email := range lead.Emails {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" || seen[email] {
				continue
			}
			seen[email] = true
			found, err := l.optedOut(ctx, email)
			switch {
			case err == nil:
				evs = append(evs, found...)
				continue
			case KeyRefused(err):
				return nil, nil, err
			case errors.Is(err, api.ErrRateLimited) || ctx.Err() != nil:
				for _, rest := range leads[i:] {
					failed[rest.ID] = err
				}
				return evs, failed, nil
			}
			failed[lead.ID] = err // its other emails are not read: the lead waits anyway
			break emails
		}
	}
	return evs, failed, nil
}

// optedOut searches the team's contacts for one email and returns an optout
// event per matching contact with the flag set. A matching contact without
// the flag is an error: an answer that cannot say "not opted out" must not
// read as one.
func (l *Lookup) optedOut(ctx context.Context, email string) ([]api.Event, error) {
	var out []api.Event
	err := eachPage(maxContactPages, func(page int) (int, pagination, error) {
		var reply struct {
			Contacts   []map[string]json.RawMessage `json:"contacts"`
			Pagination pagination                   `json:"pagination"`
		}
		body := map[string]any{"q_keywords": email, "page": page, "per_page": perPage}
		if err := l.c.Do(ctx, Request{Method: http.MethodPost, Path: contactsSearchPath, Body: body}, &reply); err != nil {
			return 0, pagination{}, err
		}
		for _, c := range reply.Contacts {
			var got, id string
			_ = json.Unmarshal(c["email"], &got)
			if !strings.EqualFold(strings.TrimSpace(got), email) {
				continue // a keyword search also finds near matches
			}
			_ = json.Unmarshal(c["id"], &id)
			var flag *bool
			if raw, ok := c[optOutField]; !ok || json.Unmarshal(raw, &flag) != nil || flag == nil {
				return 0, pagination{}, fmt.Errorf("apollo: a matching contact has no readable %s flag", optOutField)
			}
			if *flag {
				out = append(out, api.Event{Kind: KindOptOut, Email: email, Attrs: attrs(AttrContactID, id)})
			}
		}
		return len(reply.Contacts), reply.Pagination, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
