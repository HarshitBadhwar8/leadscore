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

What S0 must confirm (also marked "S0 confirms" in `adapters/hubspot`):

- `hs_email_optout` is the opt-out flag, and `"true"` its set value.
- Batch read by `idProperty: email` matches only the address given, and
  answers a missing email with a 207 `OBJECT_NOT_FOUND` error entry.
- v4 association batch reads answer a record with no associations with a
  `NO_ASSOCIATIONS_FOUND` error entry.
- The 409 message carries `Existing ID: <id>`.
- How long search lags a create (the engine waits 15 minutes before it
  trusts a "no deal" answer after a deal step was called).
- The search rate limit (the adapter spaces searches 250 ms apart).
- The private-app token-info call and its `scopes` list; deal-to-contact
  association type id 3.
