# S0 answers so far

The RFC's section 10 table ("Check before building") is at its size limit, so
the evidence behind its **Answer** and **How checked** columns lives here.

Every answer below comes from core's written records, read only. Core saved no
raw vendor response, so no fixture becomes a real capture: the fixtures these
answers touch keep `"provisional": true` and gain a `confirmed_from` field
naming the record and the fields it covers.

How sure each kind of check is:

- **Live API test:** a record of real calls with the results quoted.
- **Our account:** a record of what our own Apollo account's workflow setup
  shows.
- **Production:** code or a setup that runs in production sends or reads it,
  so it works, but nobody saved the answer.

## Answered (9)

| Question | Answer | How checked | Core record |
|---|---|---|---|
| Where does the Apollo key go? | The `X-Api-Key` header. A key in the query is refused with 422. | Live API test (2026-08-10) | "Apollo transport spike — what Apollo can and cannot do" |
| Enrichment field names | `organization.name`, `estimated_num_employees`, `latest_funding_stage`, `latest_funding_round_date` (a plain date, not a timestamp), `country`, as the adapter reads them | Production | Core's GTM Apollo enrichment client |
| Contact create call and its id | `POST` with `run_dedupe: true`; answer 200 or 201; id at `contact.id` | Production | Core's GTM Apollo contact push |
| Reply search: does the replied filter work? | Yes, on Apollo's side: it returned 9 records, every one replied | Live API test | "Apollo transport spike — what Apollo can and cannot do" |
| Reply search: label field and values | `reply_class`. Seen live: `null` (6 of 9) and `person_referral` (3 of 9). The other values are from Apollo's docs. | Live API test (values seen); Apollo's docs (the rest) | "Apollo transport spike — what Apollo can and cannot do" |
| Reply search: is `to_email` on a message? | Yes, with `contact_id`, `emailer_campaign_id`, `to_name`, `status`, `completed_at` | Live API test | "Apollo transport spike — what Apollo can and cannot do" |
| Can a workflow send a custom header? | Yes, a fixed value. It cannot compute a signature, which leadscore does not need. | Production | "GTM: the Apollo engagement workflows" |
| Visit body shape | Trigger `website_visited` at contact level, one workflow per domain; the body has contact fields (email, first and last name, title, LinkedIn URL) and an `account` block. Open: the account block's domain, website and name, the contact id, and `visited_at` | Our account | "RFC: GTM Website Visitor Capture" |
| HubSpot v4 PUT association | `PUT /crm/v4/objects/deals/{id}/associations/default/contacts/{id}` works (production). Repeating it is a no-op: production code (stated in a comment) | Production | Core's GTM HubSpot deal push |

## Partly answered, and what changed because of it

- **The reply search is a POST.** The live API test called
  `POST /v1/emailer_messages/search` (body encoding not recorded). The poller
  now sends the same filters and paging as a POST with a JSON body, like the
  other searches, instead of a GET with a query. The
  same test reached `POST /v1/contacts/search`, `POST
  /v1/emailer_campaigns/search` and `GET /v1/email_accounts`, which the adapter
  already uses. Enrichment answers on `/api/v1/`; the others were seen on
  `/v1/`. Leadscore keeps `/api/v1/`.
- **Reply bodies carry no contact id.** The variable catalogue seen on
  2026-08-19 lists no contact id (nor `reply_class` or an event-kind
  variable); another token is not ruled out. The reply templates
  in `setup/apollo/` drop `contact_id`. A reply is matched by email, and its
  key is the conversation link, else the email.
- **Endpoints only:** the three searches and the mailbox list above answer, but
  their filters and reply shapes are still unconfirmed.

## Apollo workflow retries: unverified

Core's own design says whether Apollo's workflow action retries a failed
request, and for how long, is unverified ("RFC: GTM Outreach — Lead Identity &
Reply Pipeline", decision 22). So leadscore does not lean on it: the receiver
never answers 200 before the event is stored, which keeps it from claiming an
event it lost, and silence detection notices a workflow that goes quiet. If
Apollo does not retry, an event answered 503 is lost.

## Answered by the read-only live check (2026-10-08)

`TestLiveApolloReadOnly` (`adapters/apollo/live_test.go`) ran 25 read-only
calls against a real account: no write, no credit-spending call. Raw replies
stayed outside the repo; the fixtures it touched now carry `recorded_from`.

| Question | Answer | What changed |
|---|---|---|
| Reply search: filter by date? | Yes, on the send date (`completed_at`), with the key `emailer_message_date_range`. `emailerMessageDateRange` is silently ignored (every reply comes back). | The poller sends the working key. The engine's window already reaches back by `sequence_length` + `window_margin`. |
| Reply search: paging | No `pagination` record; `page` works. | The poller reads until an empty page (a short page may be a silent per_page cap). |
| Reply time field | None: no `replied_at`. Times are `completed_at`, `created_at`, `due_at`, `failed_at`. | A polled reply is timed at its send (`completed_at`). |
| Reply fields | `to_email` always set; `contact_id` sometimes null; `reply_class` seen as null, `willing_to_meet`, `person_referral`, `follow_up_question`. | A reply with no contact id is matched by email. |
| Bad key | Auth health: 200 with `is_logged_in: false`. Elsewhere: 401 with `{error, error_details: {code, context, message, suggestions}}`, code `AUTH.AUTHENTICATION.API_KEY_INVALID`. | Refusal text also reads `error_details.code`; the prose is never read. |
| Other error shapes | An unknown contact id and bad parameters: 422 `{error}`. | Fixture `contacts_get/not_found` is 422. |
| Rate limits | Headers `X-Rate-Limit-{Minute,Hourly,24-Hour}`, `X-{Minute,Hourly,24-Hour}-Requests-Left`, `X-...-Usage`, per endpoint: 200 a minute, 400 an hour, 2,000 a day. A 429 was not provoked. | `rate_limited` fixtures carry them. |
| Sequence search | `pagination {page, per_page, total_entries, total_pages}`; `q_name` filters. | None. |
| Mailbox list | One reply, no paging; the address is at `email`, the id at `id`. | `mailbox_id` may be the id or the address. |
| Path prefix | `/api/v1/` answers for every call, auth health included. | None. |

## Still open

Enrollment (the call, its flags, re-enrolling, skip reasons), the opt-out
flag on a contact, whether the contact search spends credits, a real 429
body and `Retry-After`, the full set of eight documented reply labels (only
three were seen, plus `null`), whether a label can be added to a reply after
it first appears, HubSpot reads, and all the Cloud Run checks. These
need a write-side check on test accounts (a paused test sequence, test
contacts).
