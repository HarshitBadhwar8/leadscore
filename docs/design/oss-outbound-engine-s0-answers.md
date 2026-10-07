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

## Still open

Everything else in the RFC's table, including enrollment, the opt-out flag,
the reply date filter and reply time field, paging, error shapes, HubSpot
reads, and all the Cloud Run checks. These need a live check on test accounts.
