# leadscore reference

The exact formats: the rubric, `leadscore.yml`, the store tables, the
receiver, the `doctor` checks, hosting, and the plug-in interfaces. The
README and `docs/setup.md` walk through setup; this page is what you look
things up in.

**Formats used everywhere.** Times are UTC in the fixed form
`2006-01-02T15:04:05.000Z` (never trimmed, so text order is time order).
Durations are Go durations plus `d` for days (`90d`, `15m`). Numbers are plain
decimal. Booleans in stored rows are `yes` or empty, except `do_not_contact`,
which is `yes` or `no`.

## The rubric

A rubric is one YAML file. Top-level keys, all optional except `version` and
`lanes`: `version` (1), `fields`, `settings`, `company`, `detectors`,
`derive`, `conflicts`, `score`, `limits`, `lanes`. Start from
`examples/rubric.yml` or `examples/rubric.csv-only.yml`, and check a file
with `leadscore rules check rubric.yml`. YAML aliases (`*name`) and a key
repeated in one mapping fail at load, naming the line.

### Headers and built-in fields

Input headers are matched to field names once, before merge, for every
source. A header is first squashed to lowercase `a-z0-9` (`Work Email` and
`work_email` both become `workemail`). When two headers of one row name the
same field, the first in file order wins. Built-in header spellings:

| Field | Header spellings (squashed) |
|---|---|
| `email` | email, emailaddress, workemail |
| `full_name` | fullname, name |
| `company.name` | company, companyname, account |
| `company.domain` | companydomain, domain, website |
| `company.employees` | employees, headcount, companysize, numberofemployees |
| `company.funding_stage` | fundingstage, funding, stage |
| `company.region` | country, region |
| `title` | title, jobtitle, role |
| `linkedin_url` | linkedinurl, linkedin, linkedinprofile |
| `segment` | segment, segmentlabel |
| `trigger_note` | triggersignalnote, triggernote, triggerevent |
| `warm_path` | bestpathin, bestpath, pathin |
| `contact_id` | contactid, apollocontactid, personid |
| `at` (event rows only) | visitedat, visitdate, lastvisited, lastvisitat |

Other built-in fields: `sources_seen` (number: distinct source channels that
reported the lead), `receiver_only` (bool: known only from webhooks),
`company.leads_seen` (number), and the `status` variable (see "Statuses").

A declared field also matches its own squashed name and its `aliases`, which
win over the built-in table. Any other header is kept under its squashed name,
and rules use that name (a header `Main tool` is `maintool`). A rule naming an
undeclared field that is not a squashed name fails at load, and so does a
rule naming a header spelling of another field (`jobtitle` is `title`; write
`title`).

### Fields and settings

`fields: { <name>: { type, level, aliases } }`.

- `type` is `text`, `number`, `date` (ISO 8601; a date with no time is
  midnight UTC), `bool` (`true/false/yes/no/1/0`), or `{ ordered: <settings
  list> }`. A value that does not parse as its type is absent. Undeclared
  columns are `text` at lead level.
- `level` is `lead` (default) or `company`. A non-built-in company field is
  read from the `Companies` tab and from enrichment. An undeclared
  `company.<name>` is such a fact, as text. To lift a lead column to the
  company, use a rollup.
- Declaring a built-in field may add `aliases` but not change its type or
  level. `status` cannot be declared.
- Names in `fields`, `settings`, `company`, `detectors` and `derive` use
  lowercase letters, digits and underscores, starting with a letter.

`settings: { <name>: <list or scalar> }`. A list orders an `ordered` field;
unknown values sort below all. `company.funding_stage` ranks by
`settings.funding_order`, which you declare only to compare it with `lt`,
`lte`, `gt` or `gte`. Conditions refer to a setting as `$<name>`.

**Text matching.** Every text comparison is case-insensitive after Unicode
NFC normalization and trimming. Matching to an `ordered` list also ignores
spaces, hyphens and underscores, so `Series B` matches `series_b`.

### Conditions

| Form | True when |
|---|---|
| `{ field: F, eq: V }`, `ne` | equal, not equal |
| `{ field: F, lt: V }`, `lte`, `gt`, `gte` | for numbers, dates and ordered fields |
| `{ field: F, in: [..] }`, `not_in` | value in or not in the list (a `$setting` list is allowed) |
| `{ field: F, contains: S }` | text contains S |
| `{ field: F, present: true }`, `{ field: F, missing: true }` | the value exists or not |
| `{ all: [..] }`, `{ any: [..] }`, `{ not: C }` | combinators |
| `{ detector: D }` | detector D fired for this lead, or for its company when D's subject is company |
| `{ expr: "<CEL>" }` | a raw CEL expression over the variables below |

- `F` is a built-in field, a declared or input column, `company.<name>` for a
  company field or rollup, or a derived name.
- Any comparison on an absent value is false, so only `missing` (and `not`
  of a comparison) is true for it. `in: []` is never true; `not_in: []` is
  true for any present value.
- A condition on `status` with `eq`, `ne`, `in` or `not_in` may name only real
  statuses, so a typo fails at load.
- A raw `expr:` that fails at run time counts as false, with a warning that
  names the rule but never a lead's values.
- **Cost limit.** One condition may cost at most 1,000,000 CEL cost units
  (about one per operation) for one lead. A condition whose estimated worst
  case is over that fails at load; one that reaches it at run time counts as
  false, with a warning.

CEL variables for `expr:`:

| Variable | CEL type | Holds |
|---|---|---|
| `lead` | `map(string, dyn)` | lead fields and lead-level derived names; test presence with `has(lead.x)` |
| `company` | `map(string, dyn)` | company fields, rollups and company-level derived names |
| `detector` | `map(string, bool)` | every detector, true when fired for the lead or its company |
| `status` | `string` | the lead's status |
| `settings` | `map(string, dyn)` | the `settings` block |

A company-level condition (a company derive block, an account score rule) has
no `lead` and no `status`.

### Rollups and detectors

**Company rollups.** `company: { <name>: <rollup> }`, computed over a
company's leads: `{ any: C }` and `{ all: C }` (bool), `{ count: C }`
(number), `{ max: F }` and `{ min: F }` (number or date), `{ first: F }` (the
first present value, oldest lead first). Read as `company.<name>`.

**Detectors.** `detectors: { <name>: { kind, subject, ... } }`. `subject` is
`lead` (default) or `company`; a company detector sees every event whose
domain is the company. Every `window` and `within` is above zero and at most
90 days.

| Kind | Parameters | Fires when |
|---|---|---|
| `count_in_window` | `event`, `window`, `min` | at least `min` events of that kind in the window |
| `first_seen` | `event`, `within` | the first event of that kind ever was within `within` |
| `change` | `field` (a stored company fact, not a rollup, `leads_seen` or `domain`), `within`, optional `from`, `to` | the fact changed within `within`, matching `from` and `to` when given |
| a registered kind | its own parameters under `params:` | as its `Detector` decides |

`event` may end in `*` to match a prefix (`visit_*`). Event kinds and detector
kinds compare lowercased.

### Derive, conflicts, score, limits

**Derive.** `derive: { <name>: <block> }`, evaluated in file order. A block
is a list of rules (lead level) or `{ level, rules }`. A rule is
`{ when: C, then: V }`; the last may be `{ else: V }`. The first matching rule
wins. `then: null`, or no match and no `else`, gives "no value". All
`then`/`else` values of one name are numbers, or all text, or all booleans.
A company block may read company fields, rollups, company detectors and
earlier company blocks; a lead block may read anything above it. A derived
name shadows an input column of the same name (with a warning). These names
cannot be derived: `status`, `sources_seen`, `receiver_only`, and the fixed
`Ranked` columns. `fit_signal`, when defined, is the verdict's fit signal;
changes of `tier` and `priority`, when defined, are logged.

**Conflicts.** `conflicts: [ { field: F } ]`. A lead whose sources gave
different non-empty values for F is blocked on every lane, with the reason.

**Score.** `score: { account: [ <rule> ], contact: [ <rule> ] }`. A rule is
`{ when: C, points: N }`, or a band rule `{ band: F, points: { <threshold>:
N, ... } }`, which pays the points of the highest threshold at or below F's
number. Account rules read only company-level values. A lead with no company
gets account score 0 (reason "no company domain"). The score is the sum of
both.

**Limits.** `limits: { max_pushes_per_run, max_pushes_per_day, timezone }`,
defaults 100, 200, `UTC`. `timezone` is an IANA name; `Local` is refused.

### Lanes

`lanes: [ { id, name, kind, priority, when, push } ]`.

- `id` is required, unique ignoring case, and must never change. It starts
  with a letter or digit and uses letters, digits, `-` and `_` (it names a
  table and a file). `name` defaults to the id, `priority` to 0 (higher
  wins). `when` is required.
- `kind` is `cold`, `non-cold` or `export`.
- `push` is `<sink>:<destination>`: `apollo:sequence/<sequence name>`,
  `hubspot:contacts`, `hubspot:deals`, or `export:<anything>`. Export lanes,
  and only they, use `export:`.
- A cold lane whose `when` does not require `{ field: receiver_only, eq:
  false }` (alone, or inside a top-level `all`) gets a load warning: a lead
  known only from webhooks can be forged by anyone with the receiver secret.
- A lane whose sink is not in the build raises `lane_sink_unregistered:<lane
  id>` at run start; the run goes on, unhealthy.

**Version.** `r-` plus the first 16 hex characters of the SHA-256 of the YAML
re-marshalled with sorted keys and no comments. Comments do not change it.

## leadscore.yml

Unknown engine keys fail loading, naming the key. Adapter blocks (`store`,
`sources[]`, `enrich`, `sinks.<type>`) reach their adapter as given. Engine
keys are read as text exactly as written (`spreadsheet: 0123` is `"0123"`);
other keys in an adapter block keep YAML's types, so quote ids there.
Relative paths are relative to the folder holding `leadscore.yml`.

| Key | Default | Meaning |
|---|---|---|
| `version` | required | `1` |
| `rubric` | `rubric.yml` beside this file | the rubric file |
| `store.type` | required | `sqlite`, `sheets`, or a plug-in |
| `store.path` | `/data/leadscore.db` | SQLite file |
| `store.spreadsheet` | — | Sheets store: the spreadsheet id |
| `store.lease_bucket` | `<hosting.project>-leadscore-lease` when `hosting.project` is set | Sheets store: the Cloud Storage bucket for the run lease |
| `store.view_spreadsheet` | — | SQLite only: an optional read-only Sheet view |
| `store.credentials` | Google's standard loading | Docker with Sheets: a service-account key file (never commit it; see "Keep keys and data private") |
| `sources[]` | — | each `{ id, type, channel, path or tabs, events, apollo_held, match_domain_name }` (below) |
| `enrich` | — | `{ type, max_age: 30d, max_lookups_per_run: 100, max_lookups_per_day: 400 }` |
| `replies` | `receiver` | where replies come from: `receiver` or `polling` |
| `polling.sequence_length` | `30d` | the longest sequence you run |
| `polling.window_margin` | `7d` | added to `sequence_length` for the poll window |
| `receiver.public_url` | — | where Apollo posts; `doctor` probes its `/healthz` |
| `receiver.visit_events` | `[]` | the `visit_<name>` kinds your workflows send, for silence checks |
| `receiver.port` | `$PORT`, else `8080` | listen port |
| `sinks.apollo.mailbox_id` | — | the sending mailbox for sequences: its id, or its address (looked up once per run and turned into the id) |
| `sinks.hubspot` | — | `{ pipeline, stage, property_prefix: leadscore_ }` |
| `export.dir` | `/out` | SQLite: where export CSVs go |
| `reply_labels` | see "Reply labels" | `{ <label>: <status or none> }` overrides |
| `pushes_enabled` | `false` | cold and non-cold lanes push only when true |
| `schedule` | `15m` | time between runs; at least `1m` |
| `deadline` | `12m` | run deadline |
| `ingest_chunk_rows` | `2000` | input rows merged per run, across all sources |
| `silence_threshold` | `3d` | receiver silence before `Health` flags it |
| `log_retention` | `90d` | how long `Log` rows are kept |
| `hosting` | — | Google Cloud only, written by setup: `{ project, region, run_account, receiver_account, image }` |

**Sources.** `id` and `type` are required. `type` is `csv` (with `path`) or
`sheetsource` (with `tabs`, which reads the store's or view's spreadsheet).
`id` must be unique and not one of `receiver`, `polling`, `hubspot`,
`apollo_lookup`. `channel` defaults to the id and is what `sources_seen`
counts (two conference CSVs with `channel: conference` count once). Flags
default to false:

- `events: true`: each row is an event, not a lead (see "Event rows").
  Event rows do not add to `sources_seen`.
- `apollo_held: true`: leads from this source are treated as already held by
  Apollo, so Apollo sequence lanes skip them.
- `match_domain_name: true`: a row with no email or LinkedIn URL may match a
  lead by company domain plus full name.

**Reply polling window.** A poll reads from the earlier of now minus
(`sequence_length` + `window_margin`) and the last poll minus
`window_margin`, so an outage loses nothing. Polls run at most every 6 hours.
Apollo filters replies by the date the email was sent, not the reply's, so
`sequence_length` must cover your longest sequence: a reply that comes more
than `sequence_length` + `window_margin` after its email was sent is missed.

**Which block each adapter gets.** A source gets its `sources[]` entry; the
enricher gets `enrich`; the store gets `store`; a sink of type T gets
`sinks.T`. A lookup or poller of type T is built from `sinks.T` when that
block exists (the poller only with `replies: polling`).

**Default paths.** Without flags, commands read `/config/bundle.yaml` if it
exists, else `/config/leadscore.yml`, else `./leadscore.yml`. `--config` and
`--rubric` override. `compose.yaml` mounts your folder at `/config`.

**Keys.** API keys come from environment variables, never the file:
`APOLLO_API_KEY`, `HUBSPOT_TOKEN`, `LEADSCORE_RECEIVER_SECRET` and, while
rotating, `LEADSCORE_RECEIVER_SECRET_PREVIOUS`. On Google Cloud, a local
command with an empty key variable reads the key from Secret Manager.

**Keep keys and data private.** `chmod 600 .env`; never commit `.env` or a
service-account key file (`store.credentials`); share a spreadsheet only
with named people, never by link; `chmod 700 out` (the export lists hold
personal data). `docs/setup.md`, "Path 3", has the details.

## Store tables

A tool table is one leadscore creates and writes (every table below except
the people-owned ones). Every tool table has exactly these columns, in this
order. A newer version
only adds tables or columns; every write keeps columns it does not know.
JSON columns hold one JSON object as text. On SQLite, table names are lower
snake case.

**People-owned tables** (you edit them; leadscore only reads, except removing
used `Overrides` rows):

| Table | Columns |
|---|---|
| `Leads` (or any source tab) | any |
| `Companies` | `domain`, then any facts. Wins over enrichment and lead sheets |
| `Overrides` | `person` (email, LinkedIn URL or lead id), `action` (`status`, `same_as`, `distinct`, `retry`), `value`, `note`. Write it with the CLI (see "Statuses") |

**Tables you read:**

| Table | Key | Columns |
|---|---|---|
| `Ranked` | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, one column per derived name, `account_score`, `contact_score`, `score`, `status`, `lane`, `reasons`, `rubric_version` |
| `Export <lane id>` | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, `score`, `reasons`, `first_listed_at`, `status`, `do_not_contact`, `updated_at` |
| `Health` | `kind`, `key` | `kind` (`result` or `problem`), `key`, `value`, `first_seen_at`, `updated_at` |
| `Log` | — | `at`, `run_id`, `level`, `lead_id`, `email`, `kind`, `message`, `rubric_version` |
| `Pushes` | `lead_id`, `lane_id`, `step` | `lead_id`, `lane_id`, `step`, `lane_kind`, `dest`, `state`, `vendor_id`, `attempts`, `called_at`, `intent_run`, `last_error`, `first_started_at`, `updated_at` |
| `Outcomes` | `lead_id` | `lead_id`, `status`, `status_at`, `unsubscribed_at`, `unsubscribed_origin` (`event`, `lookup`, `manual`), `reply_status`, `reply_at`, `contacted_at`, `deal_id`, `deal_stage` (`open`, `won` or `lost`), `deal_checked_at` |
| `Company facts` | `domain` | `domain`, `facts` (fact to {value, origin, at}), `previous` (the value a change replaced), `rollups`, `first_seen`, `enriched_at`, `not_found_at`, `enrich_failed_at` |

**Machine tables** (hidden on Sheets; do not edit):

| Table | Key | Columns |
|---|---|---|
| `People` | `lead_id` | `lead_id`, `created_at`, `apollo_held_at`, `merged_into`, `first_seen`, `fields`, `conflicts` |
| `Identities` | `key` | `key` (lowercased email or canonical LinkedIn URL), `kind`, `lead_id`, `source_id`, `first_seen_at` |
| `Events YYYY-MM` (Sheets) / `Events` (SQLite) | `seq` | `seq`, `received_at`, `kind`, `body` |
| `Window events` | `event_key` | `event_key`, `subject`, `lead_id`, `domain`, `kind`, `at`, `attrs` |
| `Applied rows` | `source_id`, `row_id` | `source_id`, `row_id`, `row_hash`, `lead_id`, `first_applied_at`, `key_conflict_at` |
| `Seen events` | `event_key` | `event_key`, `first_received_at`, `run_id` |
| `Applied overrides` | `row_hash` | `row_hash`, `applied_at`, `run_id` |
| `State` | `key` | `key`, `value` |

**Ranked.** `email` is the primary email and `linkedin_url` is in canonical
form (`linkedin.com/in/<slug>`). `lane` is the lane the lead was pushed to
this run, else the export lane it is listed on, else empty. `reasons` lists
the rules fired, the points added, and why each matching lane was skipped.

**Company facts.** Origins, highest first: `companies_tab`, `enrichment`,
`input`. A lower origin never replaces a higher one. A changed value moves
the old one to `previous`. Emptying a `Companies` cell lets a lower origin
fill it.

**Export rows.**

- A lead is listed once, the first run it matches the lane. A run adds at
  most `ingest_chunk_rows` new rows across all export lanes. `score` and
  `reasons` stay as when listed; `status` and `do_not_contact` are refreshed
  every run, also for lanes since removed.
- `do_not_contact` is `yes` when the lead is blocked on every lane (opted
  out, unresolved duplicate, rubric conflict, Overrides problem), has a status
  that blocks cold lanes, is at a company with an open or won deal, was
  already contacted, or matches a cold lane (even one whose sink is not set
  up yet). When several rows lead to one merged person, only one stays `no`.
- Opt-outs reach the lists from the receiver, polling and `Overrides`. The
  Apollo and HubSpot opt-out lookups run only for leads about to be pushed.
- On SQLite each export lane also has a CSV, `<export.dir>/<lane id>.csv`,
  rewritten after every run (never on dry run), UTF-8, mode 0600. A cell a
  spreadsheet would read as a formula is quoted. A plug-in store gets no CSV
  files: its tables are its lists.

**Retention.** `Window events` keeps 90 days, `Seen events` one year, `Log`
`log_retention`. Statuses never depend on trimmed events: `Outcomes` keeps
opt-outs, replies, contacts and deals for good.

**Health rows.** Results: `last_result` (`healthy` or `unhealthy`),
`last_run_at`, `last_success_at`, `run_id`, `rubric_version`, `schedule`.
Problems have key `<kind>:<id>` and value `<message>. Fix: <fix>.` (prefixed
`warning: ` for a warning). A run keeps `first_seen_at` for a problem still
open and deletes resolved ones; a run that failed or was cut short deletes
none. Besides the `doctor` check keys below, a run can raise:
`source_failed:<source id>`, `step_failed:<step>`, `run_failed`,
`deadline_passed`, `run_stopped` (warning), `commit_too_large`,
`events_shrank`, `ingest_backlog` (warning), `skipped_runs`,
`lane_sink_unregistered:<lane id>`, `rubric_unknown_field:<field>`,
`sink_failed:<sink type>`, `lookup_failed:<lookup type>`,
`poll_failed:<sink type>`, `enrich_failed` (warning),
`receiver_only_push:<lead>` (warning), `export_dir_readable` (warning),
`export_lane_invalid:<lane id>`, `view_write_failed` (warning),
`people_tab_check` (warning).

On Sheets, cell `Health!H1` reads `STALE` when no run has succeeded in three
`schedule` intervals. Setup sets the spreadsheet to UTC so the formula
matches the stored times.

**Sheets size.** A spreadsheet holds 10 million cells. At 20,000 leads and
about 1,000 receiver events a day a Sheets store uses about 6 million.
`doctor` warns at 70%. Above about 1,500 events a day, use SQLite or a
shorter `log_retention`.

## Statuses

Each run works out every lead's status from `Outcomes` and `Overrides`,
taking the first rule that applies, across the lead and every lead merged
into it:

1. `unsubscribed`: any opt-out (an `unsubscribed` webhook, an opt-out found
   by a lookup, a polled `unsubscribe` label, or a manual status). Automation
   never clears it.
2. `blocked`: conflicting status rows in `Overrides`, or an unknown value.
   Blocked on every lane until fixed.
3. Manual: the lead's one status row in `Overrides`.
4. `deal`: a lead at the company has an open or won deal, or a HubSpot deal
   step was called there, until a lookup shows no open or won deal.
5. Reply: `replied_positive`, `replied_negative`, `replied_neutral` or
   `replied_unlabelled`, from the latest reply.
6. `contacted`: a `sent` event or a finished cold push.
7. Otherwise `new`.

**Overrides from the CLI** (same on every store; a person not yet known
waits until they appear):

- `set-status <person> <status>` replaces the lead's status rows;
  `set-status <person> none` removes them; `set-status <person>
  resubscribe` removes them and undoes a manual `unsubscribed` (never an
  automatic one).
- `merge <a> <b>` appends a `same_as` row. Both must be known leads, and a
  merge is permanent: the older lead survives.
- `mark-distinct <a> <b>` appends a `distinct` row (two namesakes at one
  company).
- `retry [--lane <id>] [<person>]` appends a `retry` row (`*` means every
  lead).

**Pushes.** Each `Pushes` row is one step for one lead in one lane. `state`
is `pending`, `done`, `failed` (three counted attempts failed; `retry`
resets it) or `cancelled` (a check failed, the vendor refused, or the lane
was removed). A step stays pending and waits when a lookup failed, pushes are
off, a limit is reached, the deadline passed, or the vendor rate-limited. A
person is never cold-contacted twice: any cold row that was called holds the
cold push for good.

## Receiver

The receiver counts as configured when `replies: receiver` (the default) or
`receiver.visit_events` is non-empty.

| Route | Purpose |
|---|---|
| `POST /apollo/visit` | Apollo website-visit workflow |
| `POST /apollo/reply` | Apollo sent, reply and unsubscribe workflow |
| `GET /healthz` (or `HEAD`) | health check |

| Answer | When |
|---|---|
| 200 | `POST`: the request is stored. `/healthz`: see below |
| 400 | the body could not be read |
| 401 | wrong secret, or no secret set |
| 404 | any other path |
| 405 | wrong method for the path |
| 413 | body over 1 MB (not stored) |
| 503 | not stored within 10 seconds; or no free slot to read a body-secret request; the sender must retry |

`/healthz` with the timer (`serve --every`) is 200 when the last run
succeeded or none is due yet, and 503 when the last run failed, none
succeeded in three intervals, or the store cannot be read. Without the timer
(Cloud Run) it is 200 unless storing events failed.

- **Secret.** Send it in the `X-Leadscore-Secret` header, or in a top-level
  `leadscore_secret` body field when Apollo cannot send headers. Prefer the
  header: a body secret is known only once the body is read, so such
  requests are read at most 16 at a time, 3 seconds each; beyond that they
  get 503 and the sender must retry. Header requests never wait. The secret
  is compared in constant time against `LEADSCORE_RECEIVER_SECRET` and, when
  set, `LEADSCORE_RECEIVER_SECRET_PREVIOUS`, and removed from the body before
  storing. With no secret set, every POST gets 401, while `/healthz` and the
  timer still work.
- **Storing.** Requests are gathered for 2 seconds and stored together. A
  string over 16 KB is shortened; a body still over 50,000 characters keeps
  only the fields leadscore reads. A body that is not JSON is stored as
  `{"__not_json": true, "raw": "<first 16KB>"}`.
- **Shutdown.** On SIGTERM `serve` lets a running run save, stores every
  request it accepted, and exits.
- **Image.** The container runs as a non-root user (uid 10001) on a distroless base with no shell; `/data` and `/out` are owner-only.
  `leadscore healthz` calls the local `/healthz` and exits 0 on 200.

**Body formats** come from the templates in `setup/apollo/`. Event names:
`email_sent` is `sent`, `email_replied` is `replied`,
`email_replied_positive` is `replied_positive`, `email_unsubscribed` is
`unsubscribed`, `website_visited_<name>` is `visit_<name>`. A visit body uses
nested `contact.*` and `account.*` fields; a reply body uses flat fields
(`contact_email`, `contact_stage`, `last_conversation_link`, `contact_name`,
`contact_title`, `contact_linkedin_url`, `account_domain`; `contact_id` is
read when present). Each identified request also counts as an input row from
source `receiver`, so it adds `receiver` to the lead's `sources_seen`.

**Keep the secret private.** It works like a password: anyone who has it can
send fake events. The README's "Keep the receiver secret private" says how to
keep it and rotate it.

### Event rows

A source with `events: true` carries one event per row: `event` (the kind),
`at` (RFC 3339, or `YYYY-MM-DD` as UTC midnight), one of `email`,
`linkedin_url` or `domain`, and any extra columns as attributes.

- A row may carry only `visit_*` or a custom kind. The kind is lowercased and
  may use only `a-z`, `0-9` and `_`.
- Rows with a vendor-only kind (`sent`, `replied*`, `unsubscribed`, `reply`,
  `optout`, `deal_*`), no parseable `at`, or no person key are rejected and
  logged as `row_rejected` (naming the line, never a value).
- A file with no `at` column, or none of `email`, `linkedin_url` and
  `domain`, fails the whole source.
- An Apollo visitor export with no `event` column is read as
  `visit_<source id>` (source `My-Site` gives `visit_my_site`).

### Event kinds

| Kind | From | Effect |
|---|---|---|
| `visit_<name>` | receiver, event rows | detector input; an identified visit creates the lead if unknown |
| `sent` | receiver | sets `contacted_at` |
| `replied`, `replied_positive` | receiver | sets `reply_status` (`replied_neutral` or `replied_positive`) |
| `unsubscribed` | receiver | sets `unsubscribed_at` |
| `reply` | polling | status from the reply-label map |
| `optout` | HubSpot or Apollo lookup | sets `unsubscribed_at` |
| `deal_open`, `deal_won`, `deal_lost` | HubSpot lookup | sets the deal on every lead at the company |

Every Apollo event (`sent`, replies, `unsubscribed`) also marks the lead as
held by Apollo. With `replies: polling`, receiver `replied` and
`replied_positive` events have no effect, so a reply never counts twice. An
opt-out reaches every lead holding one of the event's emails or LinkedIn
URLs.

**Silence.** When `receiver.public_url` is set, each run checks that every
expected kind arrived within `silence_threshold`: `sent` with `replies:
receiver`, and each `receiver.visit_events` kind. A silent kind raises
`silent:<kind>`.

### Reply labels

With `replies: polling`, a polled reply's Apollo label sets its status:

| Apollo label | Status |
|---|---|
| `willing_to_meet` | `replied_positive` |
| `unsubscribe` | `unsubscribed` (fixed) |
| `not_interested` | `replied_negative` |
| `follow_up_question`, `person_referral`, `already_left_company_or_not_right_person`, `none_of_the_above` | `replied_neutral` |
| `out_of_office` | no outcome |
| no label (`_unlabelled`) | `replied_unlabelled` |

`reply_labels` overrides any row except `unsubscribe`. Values are a
`replied_*` status or `none`. Labels compare ignoring case.

## Vendors

**Apollo.**

- Enrichment writes `company.funding_stage` as one of `pre_seed`, `seed`,
  `series_a`, `series_b`, `series_c`, `series_d_plus`, or empty. Region is the
  vendor's country. A personal mail domain, or a name with no dot, is never
  looked up. A failed lookup waits a day; three in a row stop enrichment for
  the run.
- `apollo:sequence/<name>` has steps `contact` and `enroll`. Names resolve to
  ids once per run; an unknown name makes the step wait.
- The sink refuses (never retries) a contact that opted out, is in another
  sequence, or has an invalid email.
- 429, 401 and 403 stop the sink for the run with no attempt counted; 5xx and
  network failures are retried next run.
- Not yet confirmed: whether Apollo's contacts carry an opt-out flag
  leadscore can read. Until then there is no Apollo opt-out lookup, and the
  `apollo-key` check warns Apollo-only teams.

**HubSpot.** `leadscore setup hubspot` creates these properties in a group
named `leadscore` (the `leadscore_` prefix is `property_prefix`):

| Object | Property | Type | Use |
|---|---|---|---|
| Contact | `leadscore_lead_id` | text, unique | find the contact again after a crash |
| Contact | `leadscore_lane`, `leadscore_tier`, `leadscore_priority`, `leadscore_reasons` | text | context for salespeople; set on create only |
| Contact | `leadscore_score` | number | same |
| Deal | `leadscore_company_domain` | text | find the company's deal |
| Deal | `leadscore_lane` | text | which lane opened it |

- `hubspot:contacts` has the step `contact`; `hubspot:deals` has `contact`
  and `deal`. A deal is named by the company domain. An open deal at the
  company is reused; a won or lost deal never is.
- Before every push, the lookup reads each lead's `hs_email_optout` and the
  deals of its company.
- 429, 401 and 403 stop the sink for the run; 5xx and timeouts are retried;
  an invalid email is refused.

## Hosting

### Docker

`compose.yaml` runs `serve --every`: the receiver plus a run every
`schedule` (read at start). Mount your folder (with `leadscore.yml`, the
rubric and `.env`) at `/config`, keep SQLite on a named volume (`/data`),
and bind-mount `./out` for export CSVs. A `leadscore.yml` change is read by
the next run with no restart; a `schedule` change needs `docker compose up
-d`.

**Sheets on Docker.** As the store: one service account with a key file
(`store.credentials`) and a lease bucket. As a read-only view on SQLite
(`store.view_spreadsheet`): `leadscore setup sheet --view` makes a
spreadsheet with only `Ranked`, `Health` and the export tabs. A failed view
write raises `view_write_failed` and the run stays healthy.

### Google Cloud

`setup/gcp.sh` (subcommands `accounts`, `bucket`, `secrets`, `deploy`,
`redeploy`, `schedule`) writes the `hosting` block and creates fixed names:

| Resource | Name |
|---|---|
| Cloud Run service (receiver) | `leadscore-receiver` |
| Cloud Run job (runs) | `leadscore-run` |
| Cloud Scheduler job and its account | `leadscore-schedule`, `leadscore-scheduler` |
| Artifact Registry remote repository for `ghcr.io` | `ghcr-proxy` |
| Secrets | `leadscore-config`, `leadscore-config-version`, `apollo-api-key`, `hubspot-token`, `receiver-secret`, `receiver-secret-previous` |
| Lease bucket | `<project>-leadscore-lease` (`store.lease_bucket`) |

- **Image.** Releases publish `ghcr.io/harshitbadhwar8/leadscore`. Cloud Run
  cannot pull from ghcr.io, so `deploy` pulls any `ghcr.io/` image through
  `ghcr-proxy`, which it creates. Any other image (for example one you built
  and pushed to Artifact Registry in the same project) is deployed as it is.
- **Schedule.** `schedule` must be a whole number of minutes dividing 60
  (`*/N * * * *`) or of hours dividing 24 (`0 */N * * *`; `24h` is
  `0 0 * * *`).
- **Run job.** Task timeout is `deadline` plus a 90-second save budget, and
  `deadline` plus 90 seconds must be below `schedule`. Retries are 0.
- **Config bundle.** `leadscore config push` uploads `leadscore.yml` and the
  rubric as one secret, `leadscore-config` (YAML with keys `config` and
  `rubric`), mounted at `/config/bundle.yaml`, then records its version in
  `leadscore-config-version`. The job reads that version when each run
  starts, so a push needs no redeploy.
- **Rotating keys.** For an API key, set its variable and run
  `setup/gcp.sh secrets`; runs read the newest version. For the receiver
  secret, follow "Rotating a secret" in `docs/setup.md`, in its order:
  `setup/gcp.sh redeploy --finish-rotation` detaches `receiver-secret-previous`
  before its versions are disabled.

**Roles.**

| Account | Roles |
|---|---|
| Run account | Secret Accessor on the key secrets, `leadscore-config` and `leadscore-config-version`; Secret Version Adder on `leadscore-config` and `leadscore-config-version`; Secret Manager Viewer on `leadscore-config-version`; Storage Object Admin on the lease bucket; Cloud Run Viewer and Cloud Scheduler Viewer on the project; Artifact Registry Reader on `ghcr-proxy`; editor on the spreadsheet |
| Receiver account | Secret Accessor on `receiver-secret`, `receiver-secret-previous` and `leadscore-config`; editor on the spreadsheet |
| Scheduler account | Cloud Run Invoker on `leadscore-run` |
| You | the roles to create the above, plus Service Account Token Creator on the run account |

`leadscore setup sheet` runs with your own Google login (`gcloud auth login
--enable-gdrive-access`), so you own the spreadsheet. Other local commands
act as the run account through `gcloud auth application-default login
--impersonate-service-account=<run account>`.

### Upgrading

A new version changes the store only by adding tables or columns, which the
next run creates. An older version runs on a store a newer minor version
extended; only a newer major version is refused. Google Cloud:
`setup/gcp.sh deploy <new image>`, then `doctor`. Docker: change the image
tag in `compose.yaml`, `docker compose up -d`, then `doctor`. Rolling back is
deploying the previous tag. Copy the spreadsheet or the SQLite volume first.

## Doctor checks

`doctor` prints the checks in this order. It exits 0 when none fails
(warnings allowed) and 1 otherwise. It never writes the store or takes the
lease; checks that need a store it cannot open print "skipped". "In run"
checks also run inside every run and make it unhealthy when they fail.
`SKILL.md` has a troubleshooting entry for each.

| Check | Fails when | Fix |
|---|---|---|
| `secrets` (in run) | a configured adapter's key variable is missing | add it to Secret Manager or `.env` |
| `receiver-secret` (`serve` start and doctor) | the receiver is configured and its secret is missing; a previous secret still set (warning) | set it; finish the rotation |
| `hosting` (only with `hosting`) | service, job, scheduler job or `ghcr-proxy` missing; service above 1 instance; job timeout or retries wrong; `deadline` plus save budget not below `schedule`; scheduler account cannot run the job; the version secret out of step with the bundle (an interrupted push); the job not reading `LEADSCORE_CONFIG_VERSION`; the service or job running as another account; the service without `LEADSCORE_RECEIVER_SECRET`; anyone allowed to run the job. Warnings: no run since the last push; the previous receiver secret still attached; the scheduler job paused | `setup/gcp.sh` the missing step, or `leadscore config push` |
| `rubric-version` (warning) | the last run's rubric version differs from the local rubric | `leadscore config push`, or deploy the matching file |
| `sheet-access` (in run) | an account cannot open the Sheet (`sheet-access:<spreadsheet id>`), or on Google Cloud the run or receiver account is not an editor (`sheet-access:<account>`). For the SQLite view, warnings inside a run | share it, or ask the Workspace admin for an exception |
| `sheets` (in run) | not recalculating hourly (`sheets:recalc`) or not on UTC (`sheets:timezone`); cell use past 70% (`sheets:cells`, warning) | `leadscore setup sheet --repair`; shorten `log_retention` or move to SQLite |
| `hubspot` (in run; with `sinks.hubspot` and the token) | missing scopes (`hubspot:scopes`); properties missing or wrong type (`hubspot:properties`); pipeline or stage not found, unset for a deals lane, or closed (`hubspot:pipeline`); a deal step waiting on an unknown stage (`hubspot:unknown_stage`); the block unreadable (`hubspot:config`); HubSpot unreachable (`hubspot:api`) | re-create the private app; `leadscore setup hubspot` |
| `apollo-key` (in run) | Apollo refuses the key (`apollo-key:auth`); no answer (warning `apollo-key:unreachable`); no readable opt-out flag for an Apollo-only team until an unsubscribe webhook arrives (warning `apollo-key:no_optout_flag`); a block cannot build a client (`apollo-key:config`) | check the key |
| `apollo-sequences` (in run) | mailbox id (or address) not found; a lane's sequence name missing or ambiguous; key refused for sequences (`apollo-sequences:key`); no answer (warning `apollo-sequences:unreachable`) | check `sinks.apollo.mailbox_id` and the sequence names; use a master key |
| `receivers` | with the receiver configured: `receiver.public_url`'s `/healthz` unreachable or answering other than 200 or 503 (`receivers:unreachable`), or not http(s) (`receivers:public_url`); a 503 (warning `receivers:unhealthy`); no `receiver.public_url` (warning `receivers:no_public_url`) | re-deploy; on a laptop, `replies: polling` or a tunnel |
| `receiver-silence` (in run; with `receiver.public_url`) | an expected event kind silent past the threshold (`silent:<kind>`) | check the Apollo workflow |
| `lease` | on Sheets, the lease bucket missing or not writable (`lease:bucket`); a held lease shown as a warning (`lease:held`) | `setup/gcp.sh bucket` |
| `pushes` (in run) | failed steps (`push_failed:<lead>:<lane>:<step>`); steps pending over 24 hours (warning `push_pending`) | `leadscore retry` |
| `store` (in run) | store from a newer major version; SQLite or a CSV path on Cloud Run; fewer `Pushes` rows than a run once saved (`ledger_shrank`; pushing is blocked); SQLite on a disk that is not kept, or opened outside the container that last opened it | install a matching version; restore the `Pushes` rows; use a named volume and `docker compose exec` |
| `rubric` (in run) | fails to compile; a rule reads a field that is not built in, declared, or a loaded column | fix the file |
| `overrides` (in run) | unknown value; conflicting status rows (`status_conflict:<lead>`); a row naming no known person (warning `override_unmatched:<row>`); a `retry` row naming an unknown lane (warning `override_unknown_lane:<row>`) | fix the cell |
| `pushes-enabled` | still off (warning `pushes-enabled:off`); a cold lane whose sink has no `sinks.<type>` block (warning `cold_lane_no_sink:<lane id>`) | review `--dry-run`, then set `pushes_enabled: true`; set the sink up or remove the lane |
| `duplicates` (in run) | unresolved namesakes (`namesake:<lead>`); leads in a hand-edited merge cycle (`merge_cycle:<lead>`); key conflicts (warning `key_conflicts`) | resolve in Overrides (`distinct` or `same_as`) |

## Fixed values

Set in code, not configurable: reply polling every 6 hours; pushes in
batches of 25 leads; 3 attempts before a step fails; 30-second vendor call
timeout; pending-push warning after 24 hours; 10% extra leads looked up before
pushing, to replace any a lookup removes; cell-use warning at 70%; one-day
wait after a failed enrichment lookup; a HubSpot "no deal" answer within 15
minutes of a deal call is not trusted; the receiver gathers requests for 2
seconds and answers 503 if one is not stored within 10 seconds; `/healthz`
cache 60 seconds; save budget 90
seconds; lease length `deadline` plus 120 seconds; event window 90 days;
`Seen events` one year.

## Plug-ins

The public package is the module root, `leadscore`. A custom build imports
its adapter package, registers it in `init`, and calls `leadscore.Main()`.
Full types and doc comments are in the package's godoc.
`docs/postgres-store.md` walks through a store.

```go
func RegisterSource(typ string, f func(Config) (Source, error))
func RegisterEnricher(typ string, f func(Config) (Enricher, error))
func RegisterPoller(typ string, f func(Config) (Poller, error))
func RegisterLookup(typ string, f func(Config) (Lookup, error))
func RegisterSink(typ string, f func(Config) (Sink, error))
func RegisterDetector(kind string, f func(params Config) (Detector, error))
func RegisterBackend(typ string, f func(Config) (Backend, EventLog, error))
```

Each panics on an empty type, a nil factory, or a type already registered.
`Config` is the adapter's block from `leadscore.yml` (a `map[string]any`).
These interfaces never gain methods after `v0.1.0`; a new capability is a
separate optional interface.

**Errors.** Classify with `errors.Is`. Any other error counts one attempt.

| Error | Meaning |
|---|---|
| `ErrRateLimited` | stays pending, no attempt counted, the sink stops for this run |
| `ErrTransient` | stays pending, no attempt counted |
| `ErrRefused` | the vendor said no for a reason retrying cannot change; the step is cancelled |

### Sinks

```go
type Sink interface {
    // Steps returns the ordered steps for one destination, for example
    // Steps("sequence/qualified") == {"contact", "enroll"}.
    Steps(dest string) []string
    // Do must be find-or-create by req.Key: calling it twice with the same key
    // leaves one vendor-side object.
    Do(ctx context.Context, req StepRequest) (vendorID string, err error)
}
```

`StepRequest` carries the `Key` (lead, lane, step), `Dest` (the part after
`<sink>:`), the `Lead`, `Prior` (vendor ids from this push's earlier steps)
and `Related` (this sink's done steps for other leads at the same company).

### Stores

```go
type Backend interface {
    ReadTable(ctx context.Context, name string) ([]Row, error)
    Lease(ctx context.Context, owner string, ttl time.Duration) (RunLease, error)
    Commit(ctx context.Context, writes []TableWrite) error
}
type RunLease interface {
    // Check returns ErrLeaseLost if the lease expired or another owner took it.
    Check(ctx context.Context) error
    // Release gives the lease up only if this owner still holds it.
    Release(ctx context.Context) error
}
type EventLog interface {
    AppendEvents(ctx context.Context, events []RawEvent) error
    ReadEvents(ctx context.Context, cursor Cursor) ([]RawEvent, Cursor, error)
    DeleteProcessed(ctx context.Context, committed Cursor, olderThan time.Time) (Cursor, error)
}
```

- `ReadTable` returns every row; a missing table returns no rows and no error.
  Rows are `map[string]string`; keep every value exactly.
- `Lease` takes the run lease or returns `ErrLeaseHeld`, taking over an
  expired one. It must be a real compare-and-swap.
- `Commit` applies every write all-or-nothing. Ops: `OpReplace`, `OpAppend`,
  `OpUpsert`, `OpDelete`, `OpTrim` (delete rows whose `Column` is before
  `Before`). An `OpUpsert` or `OpDelete` with no `Key` is refused before
  anything applies. An `OpAppend` to a keyed table of a key the table
  already holds, or that the same commit already wrote, fails the whole
  commit. It creates a missing table or column the first time a write names
  it, and returns `ErrTooLarge` rather than splitting.
- `ErrCommittedWithProblems` from `Commit` means every write was saved but a
  people-owned table needs a person's look; the engine treats the commit as
  done and never resends it.
- `AppendEvents` stores a batch all-or-nothing, in order, and returns only
  once it is durable. `ReadEvents` returns events after the cursor; sequence
  numbers are never reused. It returns `ErrEventsShrank` when the log holds
  fewer events than a saved cursor says were read.
- `DeleteProcessed` removes events at or below `committed` older than
  `olderThan` and returns the cursor the engine should save.
- Optional: `LeaseInspector` (`LeaseInfo`) lets `doctor` show the lease.

### Conformance suites

A plug-in store or sink passes its suite from an external test package. The
signatures name `api.X`; these are the same types as `leadscore.X` (the root
aliases them), so pass `leadscore` values unchanged.

```go
package storetest
var Schema []Table // every tool table, in order
// Run checks a plug-in store: every table round-trips, Commit is
// all-or-nothing, the lease has exactly one winner, events keep their order,
// and unknown columns are kept.
func Run(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog))

package sinktest
type Harness struct {
    New    func(cfg api.Config) (api.Sink, error)
    Vendor Vendor // a fake the sink is pointed at: Count(step) and Fail(step, kind)
    Dests  []string
}
// Run replays every step after a simulated crash and asserts one vendor-side
// object, calls Do twice with one key, and checks each FailKind maps to its error.
func Run(t *testing.T, h Harness)
```
