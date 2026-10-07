# Provisional Apollo fixtures

These files are **provisional**. They are built from core's Apollo client
tests and Apollo's public API docs, with made-up companies and example
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

A `note` field says what S0 must confirm about a file. Assumptions taken from
Apollo's docs rather than seen: the `X-Api-Key` header, 401 (or 403) for a bad
key, the organization field names, and what an unknown domain gets back (a 200
with no organization is not-found; a 404 is treated as a failure until S0 says
otherwise).
