# HubSpot fixtures (provisional)

These fixtures are **not** S0 captures. They were written before S0 from
HubSpot's public API documentation and from the request and answer shapes of
earlier working code and its tests. Every file carries `"provisional": true`.

When S0 lands, replace each file with the recorded call, or confirm it, and
drop the flag. The fake in `internal/fakes/hubspot` serves the error bodies
here as written, and `internal/fakes/hubspot`'s fixture test replays every
success case against the fake, so a changed shape shows up as a failing test.

Layout: `<call>/<case>.json`, each `{method, path, query, request_headers
(names only), request_body, status, response_headers, response_body,
provisional}`. Names, emails and domains are made up (`example.com`); there
are no keys or tokens.

Some parts are backed by HubSpot code that runs in production (no response
was saved, so every file stays provisional). Those files carry a
`confirmed_from` field: the v4 `PUT` association path and method, and that
repeating it is a no-op (`associations_put/deal_contact`); the contact create
request skeleton (`{"properties": ...}` in, `id` out); the search path
`/crm/v3/objects/{object}/search`; and the pipelines' `id` and `label` for
pipelines and stages.

What S0 must confirm (also marked "S0 confirms" in `adapters/hubspot`):

- `hs_email_optout` is the opt-out flag, and `"true"` its set value.
- Whether batch read by `idProperty: email` also matches a contact's
  secondary addresses (the adapter re-reads one by one if it does), and that
  it answers a missing email with a 207 `OBJECT_NOT_FOUND` error entry.
- That reading a merged-away contact id (batch read or `GET`) answers with
  the surviving contact.
- v4 association batch reads (contact to companies, contact to deals,
  company to deals, deal to companies) and the company batch read by id answer a record with no associations with a
  `NO_ASSOCIATIONS_FOUND` error entry, and whether they lag a fresh create
  (the deal step's retry relies on the contact-to-deals read not lagging).
- The v4 `PUT` association's answer body (its path and no-op repeat are
  confirmed above).
- Stage metadata keys and values (`isClosed`, `probability`).
- The `INVALID_EMAIL` code in a 400, and the 409 message `Existing ID: <id>`.
- How long search lags a create (the engine waits 15 minutes after a deal
  step's latest call before it trusts a "no deal" answer).
- The search rate limit (the adapter spaces searches 250 ms apart).
- The private-app token-info call and its `scopes` list; deal-to-contact
  association type id 3; 401 and 403 answers.
