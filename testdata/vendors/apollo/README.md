# Provisional Apollo fixtures

These files are **provisional**. They are built from earlier working code's
Apollo client tests and Apollo's public API docs, with made-up companies and example
domains, not from S0's real captures. Each file carries `"provisional": true`;
a shape taken from the docs because a real reply cannot be had safely (the
429) also carries `"documented": true`.

Each file is one call and case, in S0's shape: `{method, path, query,
request_headers (names only), request_body, status, response_headers,
response_body}`. `internal/fakes/apollo` serves them.

When S0 lands, replace each file with the real capture (scrubbed: no keys, no
real people or companies), drop `provisional`, and re-run the tests. Code that
depends on an S0 answer says "S0 confirms" next to it.

| Call | Cases |
|---|---|
| `auth_health` (`GET /v1/auth/health`, the `apollo-key` check) | `ok`, `not_logged_in`, `bad_key` |
| `organizations_enrich` (`GET /api/v1/organizations/enrich`, the enricher) | `found`, `found_sparse`, `not_found`, `not_found_null`, `status_404`, `server_error`, `rate_limited`, `bad_key` |
| `contacts_create` (`POST /api/v1/contacts`, the sink's contact step) | `created`, `invalid_email`, `rate_limited`, `server_error`, `forbidden`, `bad_request` |
| `contacts_get` (`GET /api/v1/contacts/<id>`, read before enrolling) | `found`, `not_found` |
| `contacts_search` (`POST /api/v1/contacts/search`, the opt-out lookup) | `found`, `opted_out`, `rate_limited` |
| `emailer_campaigns_search` (`POST /api/v1/emailer_campaigns/search`, sequence names) | `found`, `rate_limited`, `forbidden` |
| `emailer_campaigns_add_contact_ids` (`POST /api/v1/emailer_campaigns/<id>/add_contact_ids`, the enroll step) | `added`, `skipped_other_sequence`, `skipped_unsubscribed`, `skipped_invalid_email`, `rate_limited`, `server_error`, `forbidden`, `bad_request`, `already_in_sequence` |
| `email_accounts` (`GET /api/v1/email_accounts`, the apollo-sequences check) | `list` |
| `emailer_messages_search` (`POST /api/v1/emailer_messages/search`, reply polling) | `replies`, `rate_limited` |

## What is already confirmed

Some parts are backed by a written record of real calls, though no raw
response was saved. Each such file has a `confirmed_from` field naming the
record and the fields it covers, and stays `"provisional": true`, since
nothing it holds is a saved response:

- A live API test (2026-08-10): the `X-Api-Key` header (a key in the query is
  refused with 422); `auth_health/ok` in full (path, method, body); that
  `POST` answers 200 on `contacts/search`, `emailer_campaigns/search` and
  `emailer_messages/search`, and `GET` on `email_accounts`; and on the reply
  search, the replied filter, `reply_class` (mostly `null`), `to_email`,
  `contact_id`, `emailer_campaign_id`, `status` and `completed_at`. That test
  used the `/v1/...` prefix; these files keep `/api/v1/...`.
- Code that runs in production: the enrichment field names (the funding date
  is a plain date), and the contact create (`run_dedupe: true`, 200 or 201,
  `contact.id`).

Everything else below is still unconfirmed.

A `note` field says what S0 must confirm about a file. Assumptions taken from
Apollo's docs rather than seen: 401 (or 403) for a bad key, and what an
unknown domain gets back (a 200
with no organization is not-found; a 404 is treated as a failure until S0 says
otherwise).

The outreach calls (everything after `organizations_enrich`) come from
Apollo's public API docs only: the earlier code never enrolled or polled. Assumed and
unseen (beyond the confirmed parts above): the paths' bodies, `run_dedupe`
returning the existing contact,
`contact_campaign_statuses` on a contact, the `skipped_contact_ids` shape and
its reasons, whether adding a contact already in the sequence is a no-op, the
opt-out flag's name (`email_unsubscribed`) and that the contact search costs
no credits, the reply time field (`replied_at`) and the filter by sent date, the
`pagination` record (`page`,
`total_pages`), the contact search's `q_keywords` filter, and that a key
without master scope gets 401 or 403 on the sequence calls.
