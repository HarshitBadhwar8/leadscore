# RFC: Open-Source Outbound Engine (`leadscore`)

- **Status:** Draft
- **Date:** 2026-09-30
- **Author and delivery owner:** Harshit Badhwar
- **PRD:** none. The product brief is the aligned plan doc, [Open-Source Lead Scoring](https://claude.ai/code/artifact/14431c5d-f37d-4039-a2d1-f845918a07b0), which adopts Mohit's [Open-Source Outbound Engine](https://claude.ai/artifact/HZbhar22Yx9ugYh8BZcmJ2) design. Decisions made there are recorded here as `decided (author, from brief)`.
- **Company open-source plan:** this repo follows the company's open-source plan (`docs/open-source/STATE.md` on PR #10818) for licence (MIT, as the plan decided for every repo), copyright holder, org, repo name, maintainer at publish, contributor credit and how a repo is published. Where this RFC and the plan disagree, the plan wins.
- **Related:** [gtm-outreach-lead-pipeline](gtm-outreach-lead-pipeline.md), [gtm-prospect-relevance-engine](gtm-prospect-relevance-engine.md), [gtm-account-model](gtm-account-model.md), [gtm-hubspot-push-properties](gtm-hubspot-push-properties.md), [gtm-website-visitor-capture](gtm-website-visitor-capture.md), [ADR-7627](../adr/7627-gtm-outreach-execution-layer.md), [Apollo transport spike](../research/apollo-transport-spike.md)
- **Contracts:** the exact public types, store tables, receiver interface, HubSpot properties, status precedence, setup runbooks and `doctor` checks are in the companion [contracts doc](oss-outbound-engine-contracts.md).
- **Where the code lives:** development happens in a private working repo (`HarshitBadhwar8/leadscore`). Once it runs end to end, it is carved into the official public repo through the company's open-source process. `leadscore` is a placeholder name.

---

## 1. Terminology

- **ICP (ideal customer profile)** — the description of which companies and people a team wants to sell to.
- **Lead** — one person the engine knows about, merged from every source that has seen them.
- **Company** — the employer a lead belongs to, keyed by its web domain. Company facts (headcount, funding, region) are shared by every lead at that company.
- **Source** — anything that brings leads or events in: a CSV file, a Sheet tab, or the receiver (Apollo's workflow events). Apollo contacts that never engaged come in as a CSV export.
- **Event** — one timestamped thing that happened to a lead or company, such as a website visit or a reply.
- **Detector** — a rule over stored events that produces a named **detector signal** (for example "three pricing-page visits in seven days"). Distinct from the rubric's ICP-fit flag, the **fit signal**.
- **Rubric** — a team's ICP written as rules: one YAML file that declares fields, rolls lead facts up to the company, derives tier and priority, adds up a score, and defines lanes.
- **CEL (Common Expression Language)** — Google's small, sandboxed expression language. The rubric's conditions compile to CEL; an expression cannot touch files or the network and always finishes.
- **Rollup** — a company-level fact computed from its leads, such as "any lead named an AI tool".
- **Verdict** — the rubric's output for one lead: fit signal, tier, priority, the two score halves, and the ordered reasons.
- **Lane** — a named rule that sends matching leads to one destination. A **cold** lane is outreach to someone who has not engaged; a **non-cold** lane acts on engagement (for example a positive reply). Outcomes suppress people only from cold lanes; an **export** lane writes to a list instead of a vendor.
- **Sink** — a destination a lane pushes to: an Apollo sequence, HubSpot contacts or deals, or an export list.
- **Outcome** — something the engine learns back after a push: a reply, an unsubscribe, a deal.
- **Receiver** — the always-on endpoint in `leadscore serve` that accepts events Apollo sends (website visits and reply events) and stores them for the next run.
- **Store** — where the engine keeps everything between runs: Google Sheets or SQLite.
- **Self-hosted** — either Docker path (your own machine, or your own server or VM), as opposed to Google Cloud ("hosted").
- **Cloud Run** — Google Cloud's service for running a container with a public HTTPS address, scaling to zero; a **Cloud Run job** runs one to completion.
- **Cloud Scheduler** — Google Cloud's managed scheduler; it starts a job or calls a URL on a cron-style schedule.
- **Identity key** — an email address or LinkedIn URL that identifies a person; a lead can have several.
- **Push ledger** — the tool's record of every push step, so nothing is pushed twice.
- **`Ranked` tab** — the tool-owned view of every lead's current verdict, rewritten each run.
- **`Overrides` tab** — the people-owned tab where someone sets a lead's status by hand or marks two namesakes as different people.
- **`Health` tab** — the tool-owned record of each run's result and every open problem.

## 2. Problem & Motivation

Our GTM pipeline in core (`backend/workloom/gtm/`) scores, routes and suppresses leads well, but it cannot leave the monorepo. Section 5 maps what is copied, modified, re-implemented and new. It is wired to our Postgres, our gRPC services, our scheduler, our config system and our own Apollo and HubSpot accounts, and its scoring rules are written for our buyer. We want to publish it as a standalone product that an outside team can plug into their own website, Apollo, HubSpot or CSV, announced on LinkedIn.

The gap in numbers: of the roughly 9,300 lines of non-test code in `gtm/`, about 800 are copied near-verbatim and about 2,400 more are reworked (section 5): the vendor HTTP clients, event parsers, email and domain checks, CSV header aliases. Everything that makes it a pipeline (storage, merge, scheduling, pushes, reply handling) is bound to our infrastructure. v1 is therefore mostly a rewrite that reuses core's vendor clients and uses core's tests as its scenario list.

If we do nothing, the GTM know-how stays internal and the launch has nothing runnable to point at.

## 3. Goals, Non-Goals & Success

### Goals

- A single Go binary, `leadscore`, that runs the whole loop end to end: bring leads in, merge them, enrich companies, detect signals, score, route each lead to a lane, push, and read outcomes back.
- Every edge is a plug-in behind a public interface, so a team can add its own source, sink or store without forking the engine.
- Three documented paths, all running the same `leadscore` binary or image: **Docker on your own machine** (try it locally; SQLite on a volume; runs started by a timer built into `serve`), **Google Cloud** (the always-on production path: the receiver as a Cloud Run service, runs as a Cloud Run job started by Cloud Scheduler, Google Sheets as the store, since Cloud Run keeps no files), and **Docker on your own server or VM** (the same compose file, with an HTTPS proxy for webhooks; SQLite by default, Sheets allowed).
- Google Sheets and SQLite are the built-in stores; a team can connect PostgreSQL or another database as a plug-in store, without forking.
- Rules are data, not code: a team with a different ICP changes one YAML file, and any column in its data is usable in rules without declaring it.
- Setup a non-technical person can finish with a coding agent and only the files we ship, on the Google Cloud path; the Docker paths are for technical users. The README is the ordered setup runbook, written so an agent can follow it end to end.

### Non-goals

Scoped out completely (not planned):

- A UI or rule editor; LLM features in the engine; installs serving many organizations.
- A built-in PostgreSQL store. Teams that want one write a plug-in store against the `Backend` interface (section 6.2); we document how, and the shared `storetest` suite checks it.
- Updating CRM status because of a reply (the tools' native integrations do this). A team may still configure a lane that creates a HubSpot deal for positive replies; that is a push, not a status update.
- LinkedIn scraping and Sales Navigator.
- Moving a lead between cold lanes, and reading a reply's mood with an agent.
- Erasing a person from the store. Removing someone's data would also remove their unsubscribe, so they could be contacted again; adopters handle deletion requests in their own tools.

Deferred to community adapters:

- Other outbound tools and CRMs.

### Success signal (Definition of Done)

v1 is done when all of these hold:

- The full test suite passes in CI with no real API keys, including the end-to-end run with fake vendors on both stores.
- Our own ICP, written in the rubric format and run privately, reproduces core's verdicts on every branch.
- A CSV-only team gets ranked leads in the export list, and an Apollo + HubSpot team gets pushes and suppression.
- A non-technical person with a coding agent and only our files reaches a filled `Ranked` tab on a fresh Google Cloud project.
- A rubric for a different ICP runs with no code change.

A CSV-only team sends its emails from its own tool, so the engine cannot see who replied. Someone on the team records each reply or unsubscribe in Overrides (the Overrides tab on Sheets, or `leadscore set-status` on SQLite; for example, `priya@acme.io` set to `unsubscribed`), and the next run keeps that person out of every cold lane, exactly as an Apollo or HubSpot outcome would.

### Room for the next step

The one place rigidity would hurt is the plug-in surface: vendors change and teams use other tools. Interfaces are public and versioned with the module, so a new adapter is additive. v1 is meant to be the final state apart from minor fixes, so nothing here designs a v2, and store changes in v1.x only add tables or columns (section 6.6).

## 4. High-Level Architecture

The engine is one Go binary used two ways: `leadscore serve` is the always-on receiver, which only appends events to the store, and `leadscore run` runs the loop once. On Google Cloud, `serve` is a Cloud Run service and `run` is a Cloud Run job that Cloud Scheduler starts; on Docker, `serve --every` also starts `run` on a built-in timer, every `schedule` (15 minutes by default).

```
                         ICP rubric (YAML, compiled to CEL)
                                      |
 1. INGEST                            v
 CSV / Sheet --+                +-----------+        4. PUSH
 Apollo CSV ---+--> Sources --> |  Engine   | --> Lanes --> Sinks: Apollo sequences
                               | (in memory)|             HubSpot contacts / deals
   ^                            | merge     |              export list
   |                            | enrich    |                    |
   |  2. RECEIVE (always on)    | detect    |                    |
 Apollo workflows               | score     |                    |
 (visits, replies) --> Receiver | route     |                    |
   (leadscore serve)            +-----------+                    |
                                  load ^  | save                 |
                                       |  v                      |
                               +----------------+                |
 3. REMEMBER                   | Store          |  <-- push ledger, outcomes
                               | Sheets / SQLite|                |
 Scheduler or serve timer ----> leadscore run (one execution)    |
                               +----------------+                |
                                     ^                           |
 5. LEARN BACK   Outcome sources: HubSpot pre-push lookup, Apollo reply events
```

The parts compose in number order: receivers (2) only ever append to the store (3); each scheduled run loads the store, ingests (1), computes everything in memory, pushes (4), and saves what it learned (5) for the next run.

### Execution model and trigger

- **Trigger:** a periodic sweep. Google Cloud: Cloud Scheduler starts the Cloud Run job every 15 minutes by default. Docker: `leadscore serve --every` runs the loop on a timer inside the same container, and Docker's restart policy keeps it alive. All paths run the identical loop. Every run re-scores every lead: detector windows expire with no new input, `Ranked` holds every verdict, and at thousands of leads a full pass is cheap. Freshness is the interval.
- **Mid-run arrivals:** receivers keep appending during a run; the run reads events present at its start, and later ones wait for the next run (section 6.7).
- **Idempotency:** every push step is recorded in a ledger and every vendor call is find-or-create, so a re-run or a crashed run never pushes twice (section 6.10).
- **Opt-outs mid-run:** events and Overrides are re-read before each push batch (section 6.9).
- **Backfill:** a first run over a large CSV is the same path; limits cap pushes, not scoring.
- **Staggering:** one install serves one team, so there is no fan-out across tenants.
- **Overlap:** a run takes a lease that lasts its deadline plus a 90-second save budget plus a 30-second margin, below the 15-minute interval, and skips if another run holds it (sections 6.6 and 6.9).

### Where it runs, and why not the alternatives

| Option | Verdict | Why |
|---|---|---|
| **Cloud Run service + Cloud Run job + Cloud Scheduler** (hosted default) | Chosen | The service runs `leadscore serve` with a public HTTPS address for Apollo's webhooks; the job runs `leadscore run`; Cloud Scheduler is the native trigger, and the job's task timeout bounds every run. Both scale to zero and run as the service account the Sheet is shared with, so no key file is needed. The service skips Cloud Run's sign-in check, so Apollo can reach it without the "public to everyone" grant many organizations block. Cost: billing must be on; Cloud Scheduler gives three free jobs per billing account; Cloud Run's free tier covers runs averaging under about a minute at the 15-minute default (S14b measures it) |
| Firebase App Hosting | Rejected | Node.js web frameworks only, per Firebase's docs; cannot run Go. It runs on Cloud Run, which we use directly |
| Cloud Functions for Firebase, including scheduled functions | Rejected | Node.js or Python only, so a second runtime; their scheduler is Cloud Scheduler, which we use directly |
| Cloud Run functions (Go) | Rejected | Needs Functions Framework packaging instead of our plain binary, breaking parity with the Docker paths; it runs on Cloud Run anyway |
| GitHub Actions cron + Apps Script receiver (earlier design) | Rejected | Two runtimes; Apps Script always answers 200, so Apollo never retries; admins often block anonymous web apps |
| Docker on your own machine, server or VM | Supported: the local and own-server paths | One image and a compose file with a built-in timer run anywhere Docker does. Not the default: a laptop sleeps and has no public address, and a server needs HTTPS and patching |
| Render, Fly.io, Railway and similar | Not documented | Another account and scheduler, and Sheets access then needs a key file |
| Kubernetes | Not documented | Far more to operate than one service needs |

## 5. What we copy, modify and add

"Copy" means near-verbatim; "Modify" means copied and reworked; "Reference" means the logic is re-implemented against the rules the file encodes, with its tests used as the scenario list. Paths starting `pkg/` are under `backend/pkg/`; paths starting `processor/` are under `backend/scripts/god-script/processor/`; every other path is under `backend/workloom/gtm/`. leadscore is standalone: it depends on no other company repo. Shared core code is copied in and maintained here.

### Copy (near-verbatim)

| From core | Lines | Lands in | Why it carries over |
|---|---|---|---|
| `pkg/logredact/` (`Redact`, `VendorErrorDetail`) + tests | | `internal/logredact/` | Redacts emails and secrets from logs and vendor errors; not `RedactStruct`, which needs protobuf. Maintained here |
| `outreach/email_shape.go` + test | 106 + 91 | `internal/merge/email.go` | Pure standard library; merge does the shape check (contracts section 12.5) |
| `pkg/emaildomain/public.go` | 72 | `internal/merge/emaildomain.go` | Personal-provider check the domain derivation needs; small, only we need it |
| `pkg/channelauth/` + test | 113 + 79 | `internal/receiver/auth/` | Shared-secret and HMAC checks; small, only we need it |
| `outreach/apollo/client.go` (`CreateContact`) | 151 | `adapters/apollo/contacts.go` | Standalone HTTP client, dedupe on |
| `outreach/apollo/notification.go` + reserved-stage test | 342 + 38 | `adapters/apollo/events.go` | Pure parsing of Apollo workflow reply events, including the event key |
| `outreach/hubspot/client_test.go`, `properties_test.go` | 380 + 418 | `adapters/hubspot/` | Run against httptest fake servers |
| `outreach/apollo/enrichment_test.go` | 240 | `adapters/apollo/` | httptest fake server |
| `relevance/score.go`, threshold-band lookup only | ~12 | `internal/rules/bands.go` | Backs the rubric's `band` score rules (contracts section 2); the rest of the file is core's score formula (Reference) |

### Modify

| From core | Lines | Change |
|---|---|---|
| `outreach/service.go` helpers (`NormalizeContact`, `deriveCompanyDomain`, `canonicalizeLinkedin`, lines 560-644, 734-748) + `derive_domain_test.go` | ~120 + 62 | Return the engine's lead type instead of `dao.UpsertContactParams`; remove the Prometheus counter and our ICP fields. `recognisedAiCodingTools` and `toolEvidenceFrom` (645-672) encode our ICP and do not ship |
| `outreach/hubspot/client.go` | 748 | Look contacts up by the lead id property, then by email (core looks up only by `workloom_lead_id`); make the id property name config; drop `findAdoptOrCreateDeal`'s legacy-deal adoption, which exists only for our migration history; add the outcome reads (section 6.9) |
| `outreach/apollo/enrichment.go` | 214 | Swap our logger for `log/slog`; add a per-run lookup budget |
| `outreach/apollo/website_visit.go` | 331 | Keep the parser, employer derivation and source id; drop gRPC and the proto; becomes the parser in `adapters/apollo` that turns stored raw requests into events |
| `outreach/apollo/service.go` | 275 | Its parse-and-map logic becomes part of the `adapters/apollo` parser for stored reply requests; drop gRPC and the proto |
| `outreach/enrichment_mapping.go` | 113 | Keep the funding map, re-pointed at the fixed stage values in contracts section 6; region becomes the vendor's country with no bucketing; drop the DAO type |
| `processor/gtmleadscsvimport.go` | 531 | Keep the header aliases (lines 58-129) as the built-in alias table in `internal/api`, applied by the engine (contracts section 2); the file-reading part becomes the CSV source. Aliases for our ICP fields do not ship |
| `outreach/apollo/oversize_test.go` | 176 | Re-pointed at the Sheets cell limit (50,000 characters) instead of Postgres's 64KB check |
| `relevance/engine_test.go` | 825 | Re-expressed as conformance tests for our ICP rubric (private) |
| `outreach/{service,account_deal_push,push_resolution,website_visit}_test.go`, `apollo/{service,website_visit}_test.go` | ~2,200 | Fake DAO becomes a fake store; gRPC codes and Prometheus asserts removed |

### Reference (re-implement; use the tests as scenarios)

| From core | What it teaches |
|---|---|
| `relevance/{tier,signal,priority,gate,score,engine,config}.go` | The rules, rule order, weights and score formula our ICP rubric must reproduce; `config.go` and `engine.go` are the parity oracle |
| `dao/lead_repo.go` (1,931) and `dao/lead_dao.go` | Merge rungs, fill-if-empty, company rollups (`aggregateAccountObservations`), source counting, status guards, gate SQL, push sets (section 6.5) |
| `dao/*_test.go` (about 20 files) | The merge and gate scenarios the merge tests must pass |
| `processor/gtmleadingest.go`, `outreach/website_visit.go` | Ingest pipeline shape: normalize, validate, merge, record refs |
| `outreach/contact_push.go`, `processor/gtmleadsapollopush.go` | Sink loops: gate re-check, stop on rate limit, save refs |
| `processor/gtmleadsrescore.go` | Run order: enrich, score leads, score companies |
| `processor/managegtmlead.go` | Manual status override with an audit trail |
| `wire/providers.go` (lines 48-66) | Receiver auth-mode combinations |

### Drop

Postgres DAOs, migrations and mocks; gRPC services; `wire/`; `config/` and generated config; the scheduler adapter; all Prometheus metrics; the quiz packages; `apollosim`; and the purge, parity, cardinality, link-accounts and quiz-backfill jobs. None is useful outside our infrastructure.

### New

| Component | Why it is new |
|---|---|
| Rubric compiler (YAML to CEL), fields, rollups, evaluator, `explain` | Core's rules are Go code over fixed fields |
| Merge engine in Go, with an identity table | Core merges in Postgres SQL |
| Detectors (three built-in kinds, plus custom kinds) and event handling | Core has no detectors |
| Table-level store backends (SQLite, Sheets), codec, schema check | Core has only Postgres |
| Lanes, push ledger, limits, run orchestration, `sinktest` | Core has one fixed gate and scheduler jobs |
| Apollo sequence enrollment | Core only creates contacts |
| HubSpot custom-property creation and outcome reads | Core resolves pipelines and stages but never creates properties or reads HubSpot back |
| Apollo reply polling fallback and reply-label map | Core receives replies by webhook only and maps only its own two markers |
| `leadscore serve` (receiver) and `leadscore run` (one execution), as a Cloud Run service and job hosted | Core's receiver is our apigateway endpoint and its runs are scheduler jobs |
| Export lists (engine-owned) | No equivalent |
| CLI, `doctor`, container image, Cloud Run and Cloud Scheduler setup scripts, README runbook, `SKILL.md` | Core runs through god-scripts and our deploy |
| Example ICP (made up) | Our ICP never ships |

Other dependencies: `github.com/google/cel-go`, `modernc.org/sqlite` (pure Go, keeps one static binary), `google.golang.org/api` (Sheets, Drive and Cloud Storage), the Secret Manager client (config push, key reads), the Cloud Run and Cloud Scheduler admin APIs (for `doctor`), `golang.org/x/text` (Unicode NFC), `gopkg.in/yaml.v3`, `github.com/google/uuid`.

## 6. Detailed Design

### 6.1 Repo layout

| Path | What lives there |
|---|---|
| module root (package `leadscore`) | The public surface: plug-in interfaces, shared types, the adapter registry, and `Run`, the library entry point |
| `cmd/leadscore/` | The CLI; a thin `main` that registers the built-in adapters and calls `leadscore.Main()` |
| `adapters/{apollo,hubspot,csv,sheetsource}/` | One public package per integration: client, source, enricher, sink, poller or lookup, parsers |
| `internal/` | Everything else; the packages and their owning slices are contracts section 12.1 |
| `storetest/`, `sinktest/` (public) | Conformance suites a plug-in store or sink runs against itself |
| `Dockerfile`, `compose.yaml` | One image for Cloud Run and Docker; compose runs `serve --every` (interval from `schedule`) with a named SQLite volume, restart policy, health check and 120-second stop grace, plus optional Caddy |
| `setup/` | Google Cloud setup scripts and Apollo workflow templates |
| `docs/postgres-store.md` | How to write a PostgreSQL plug-in store |
| `examples/` | The made-up ICP, a sample CSV, and one `leadscore.yml` per path (laptop, server, Google Cloud) |
| `README.md`, `SKILL.md`, `docs/` | The README is the setup runbook; `SKILL.md` covers rule changes and troubleshooting |

The root package re-exports `internal/api`. The semver-frozen surface is the root package (contracts section 1); `storetest` and `sinktest` are public too.

### 6.2 Plug-in contract

Adapters register themselves by name; the config's `type:` string selects them. Release binaries contain the built-in adapters only. A team with its own adapter builds a custom binary: a `main` that imports the adapter package (which registers itself) and calls `leadscore.Main()`, the same pattern Caddy and the OpenTelemetry Collector use. This is Decisions Log row 43.

The exact types, with every field, are in section 1 of the [contracts doc](oss-outbound-engine-contracts.md); S1 builds from it. In short:

- **`Source`** returns input rows and events after an opaque `Cursor` the engine saves per source.
- **`Enricher`** returns company facts for a list of domains, within a lookup budget.
- **`Poller`** reads outcomes on a schedule (Apollo reply polling). **`Lookup`** checks push candidates just before pushing (HubSpot opt-out and deals, the Apollo contact opt-out) and reports a failure per lead, so one failed lookup blocks only that lead.
- **`Sink`** declares ordered steps and does each one as find-or-create by its step key (lead id, lane id, step). It receives vendor ids from the push's earlier steps and this sink's done steps for colleagues at the same company in any lane, including steps finished earlier in the same batch.
- **`Detector`** decides from a lead's or a company's window events whether it fired.
- **`Backend`** has three methods: `ReadTable`, `Lease` (a compare-and-swap that returns a lease the run can check and release only while it still owns it), and `Commit`, which applies a list of table writes (replace, append, upsert, delete or trim) all-or-nothing and returns `ErrTooLarge` rather than splitting. Every write, including a one-row ledger update, goes through `Commit`, so a plug-in store has one write path to build and `storetest` one to check.
- **`EventLog`** appends a batch of raw receiver requests all-or-nothing, reads them after a cursor, and deletes processed ones. Sequence numbers are never reused, and an event not yet returned is returned by a later read.

Sinks classify errors with `ErrRateLimited` (stop this sink for the run; the step stays pending), `ErrTransient` (stays pending) and `ErrRefused` (the vendor said no for a reason retrying cannot change); any other error counts one attempt toward `failed`. The first two count no attempt, but neither proves the vendor did nothing, so the ledger records whether each call went out, and that decides the cold push (section 6.10). `sinktest` checks that each sink maps its vendor's responses to these errors. Interfaces never gain methods (the evolution rule is in the contracts doc).

### 6.3 Core types, statuses and identity

Tier and priority values are whatever the rubric derives; the engine only compares them. Statuses are fixed, and the order in which they win is contracts section 7:

| Status | Blocks cold lanes | Blocks non-cold lanes |
|---|---|---|
| `new`, `contacted` | no | no |
| `replied_positive`, `replied_negative`, `replied_neutral`, `replied_unlabelled` | yes | no |
| `deal` | yes | no |
| `unsubscribed` | yes | yes |
| `blocked` (conflicting or unknown Overrides values) | yes | yes |

What sets each one is in contracts sections 5.3 and 5.5. Every input to a status is saved in `Outcomes` when first learned, so a status never depends on events that are later trimmed; `deal` is re-checked each run against the company's deals.

The Overrides tab accepts every status except `new`, `contacted` and `deal`, plus `resubscribe`, which undoes a mistaken manual `unsubscribed`, applies once, and is logged. An unrecognised value blocks every lane for that lead and fails `doctor`, so a typo can never silently drop an opt-out.

**Identity.** A lead id is a UUIDv7 minted when a person is first seen. Every identity key a lead has ever had (each email and each LinkedIn URL, with the source that reported it) is kept in an identity table and never dropped. Events, overrides, the HubSpot lookup and the ledger all match against that table. How ids behave when a person merges two namesake leads is Decisions Log row 32.

### 6.4 Rubric: YAML compiled to CEL

An abridged example (the shipped example is a made-up ICP for a fictional analytics product):

```yaml
version: 1
fields:                       # optional; every input column is already usable as text
  employees: { type: number, level: company, aliases: ["Headcount", "# Employees"] }
  uses_competitor: { type: text, level: lead, aliases: ["Current tool"] }
settings:
  funding_order: [pre_seed, seed, series_a, series_b, series_c, series_d_plus]  # unknown is below all
company:                      # rollups over a company's leads
  competitor_seen: { any: { field: uses_competitor, present: true } }
detectors:
  pricing_interest: { kind: count_in_window, event: visit_pricing, window: 7d, min: 3, subject: company }
derive:                       # ordered blocks; first match wins in each
  needs_review:               # a plain list is a lead-level block
    - when: { all: [ { field: company.employees, missing: true }, { field: trigger_note, missing: true } ] }
      then: true
    - else: false
  tier:                       # the object form sets the level
    level: company
    rules:
    - when: { field: company.employees, lt: 20 }
      then: 4
    - when: { all: [ { field: company.competitor_seen, eq: true }, { field: company.funding_stage, gte: series_b } ] }
      then: 1
    - when: { detector: pricing_interest }      # true when that detector fired
      then: 2
    - else: 3
  priority:
    level: company
    rules:
    - when: { field: tier, lte: 2 }
      then: P1
    - else: P3
conflicts:
  - { field: segment }                          # sources disagreeing on segment block the lead
score:
  account: [ { when: { field: tier, eq: 1 }, points: 40 } ]
  contact: [ { when: { field: sources_seen, gte: 2 }, points: 15 } ]
limits: { max_pushes_per_run: 100, max_pushes_per_day: 200, timezone: UTC }
lanes:
  - { id: qualified, name: Qualified, kind: cold, priority: 10, when: { all: [ { field: tier, lte: 2 }, { field: receiver_only, eq: false } ] }, push: apollo:sequence/qualified }
  - { id: warm-handoff, name: Warm handoff, kind: non-cold, priority: 20, when: { field: status, eq: replied_positive }, push: hubspot:deals }
  - { id: everyone-else, name: Everyone else, kind: export, priority: 1, when: { field: tier, lte: 3 }, push: export:ranked-list }
```

Built-in fields (every other input column is available as text unless declared):

| Field | Type | Level | Produced by |
|---|---|---|---|
| `email`, `linkedin_url`, `full_name`, `title` | text | lead | inputs |
| `company.domain` | text | company | inputs (derived from a work email when absent) |
| `company.employees` | number | company | Companies tab, enrichment, CSV |
| `company.funding_stage` | ordered by `settings.funding_order` | company | Companies tab, enrichment, CSV |
| `company.region`, `company.name` | text | company | Companies tab, enrichment, CSV |
| `company.leads_seen` | number | company | merge |
| `sources_seen` | number | lead | merge |
| `receiver_only` | boolean | lead | merge: true when the receiver is the lead's only source |
| `warm_path`, `trigger_note`, `segment` | text | lead | inputs (optional columns) |
| `status` | status (section 6.3) | lead | status fold |
| `detector.<name>` | boolean | lead or company, per detector `subject` | detectors |
| derived names (`fit_signal`, `tier`, `priority`, `needs_review`, …) | as derived, or no value | per `derive` block `level` | the rubric |

Every `derive` block carries `level: company` or `level: lead` (default lead). A company block may read company fields and rollups, company-subject detectors and earlier company blocks, and yields one value per company, as core tiers the account; account-score rules may read only company-level values, so every lead at a company gets the same account half.

Rules the compiler enforces:

- Each condition becomes one CEL expression over a typed environment (`lead`, `company`, `detector`, `status`, `settings`); raw `expr:` strings compile in the same environment. The full grammar (operators, rollup and detector kinds, how a block yields no value) is contracts section 2.
- A comparison on an absent value is false; `missing:` and `present:` test presence. A declared number or date that does not parse (for example "50-200") is treated as absent and logged once per run per field.
- Every field has one type; `needs_review` is a flag, never a tier.
- Load fails, naming file, line and field, on a syntax error, type mismatch, undeclared detector or setting, unknown lane destination, a lane without an `id:`, or a detector window over 90 days (the longest window events are kept for). A rule naming a field that is neither built in, declared, nor a column in any configured input fails the run before scoring.
- The **rubric version** is a hash of the normalized rubric source, so it changes exactly when the rubric changes, not when someone adds a sheet column.

Evaluation per run: company rollups and company derivations, then per lead the `derive` blocks, then both score halves (the account half from company facts and the count of leads at the company; the contact half from the lead's **warm path** (`warm_path`, a named mutual connection) and its source count), then lane selection. `leadscore explain <person>` renders a verdict's reasons. `--dry-run` scores every row in memory and diffs verdicts and planned lanes against the last run's `Ranked`; it takes no lease, writes nothing, spends no enrichment credits (it uses saved facts), and skips the pre-push lookups. The setup agent writes the first rubric from the team's CSV headers and the example.

### 6.5 Merge

Merge follows `dao/lead_repo.go`'s identity rules, runs in memory, and is the same for both stores. It is incremental over the saved `People` and `Identities` tables: each run applies only input rows that are new or changed since they were last applied (tracked in `Applied rows` by source id and a hash of the row), so sorting a tab changes nothing, and leads whose rows are no longer supplied (for example a visitor whose events were deleted) keep everything they had. `People` keeps every input column raw, and typing happens at evaluation time, so a rubric change that starts using an existing column sees its values without re-importing. Snapshot sources (CSV, Sheet tabs) return all their rows every run; event sources return what is new since their cursor.

- **Source id per input row:** the source's `id:` plus lowercased email, else LinkedIn URL, else company domain plus normalized full name (core's rule in `gtmleadscsvimport.go`). Renaming a tab or file changes nothing, because the `id:` stays.
- **A source switched to `apollo_held: true`** marks every lead it already supplied in the next phase 1 commit, not only leads from new rows.
- **Required key:** an email, a LinkedIn URL, or (with domain + name matching switched on) a company domain and full name. Otherwise the row is rejected with a reason in the Log.
- **Match rungs:** same source and source id; any email in the identity table; any LinkedIn URL in the identity table; domain + normalized full name only when switched on for a CSV import, and only when exactly one live lead fits. As core does, a row whose email matches no lead but whose LinkedIn URL belongs to an existing lead becomes a new lead for that email; the LinkedIn URL is not written and a key conflict is counted, so one wrong URL can never join two people. Names are normalized by lowercasing, trimming, collapsing spaces and Unicode NFC.
- **Key conflict, as core does:** if a row's email matches one lead but its LinkedIn URL belongs to another lead, or differs from the LinkedIn URL the matched lead already has, the row is applied to the email-matched lead, the conflicting LinkedIn URL is not written, and the conflict is logged and counted. Pushing is not blocked. `doctor` reports the count so a person can check it.
- **Fill-if-empty for lead fields:** a filled field is never overwritten, as core's coalesce; within a run rows apply in source order, then row order; a different non-empty value from another source is kept in `People.conflicts` for the rubric's `conflicts`; the one allowed overwrite is a same-source email correction when no other source reported the old email. That applies only to sources keyed by a vendor contact id (Apollo events); a CSV or Sheet row's id contains its email, so an edited email there is a new sighting, as in core.
- **Company facts** have an order: Companies tab (human) first, then the latest enrichment snapshot, then the first CSV value. A refresh replaces the value and keeps the previous one for the `change` detector.
- **Namesakes:** as in core's gate, two live leads with the same company domain and normalized name are an unresolved duplicate, and every lane skips both. Core has no way to clear this; here a person resolves it in Overrides with `distinct` (keep apart; one pair per row, each person by email or LinkedIn URL) or `same_as` (merge). A later third namesake is blocked until paired with each.
- **Leads without a company domain** (personal email, LinkedIn only) have no company: company conditions compare false, the account half is absent, they never enter a `hubspot:deals` lane, and `explain` says "no company domain".
- **Overrides are not merged;** a manual status replaces the automated one, within the status precedence (contracts section 7). A `same_as` merge is permanent and recorded in `People.merged_into`.

### 6.6 Store

The engine loads every table except `Log` (which is only appended and trimmed) at the start of a run, works on one in-memory model, and writes back in two phases. Backends move rows only; one shared codec maps the model to tables, so a new backend is small. Every table's columns, the retention rules and the Sheets cell budget are in contracts section 4.

**Who owns what.** People own the input tabs (`Leads` or one tab per source, `Companies`) and `Overrides`; the tool only reads them. The receiver appends to `Events` (one tab per month in UTC on Sheets, such as `Events 2026-10`; one table on SQLite). The tool owns every other table:

- `People` and `Identities`: leads (including when Apollo first held them) and every identity key ever seen.
- `Company facts`: facts, rollups, and previous values for the `change` detector.
- `Applied rows` and `Applied overrides`: which input rows and one-shot override rows were already applied, so merge is incremental and each retry request runs once.
- `Window events`, `Seen events` and `Outcomes`: parsed events, de-duplication keys, and every status fact for good.
- `Ranked` (every verdict, rewritten each run), `Pushes` (the ledger), `Log` (kept `log_retention`, 90 days by default), `Health`, `State` (cursors, last poll time, versions; the lease on SQLite), and the `Export <lane id>` tables.

**Two-phase writes.**

- **Phase 1**, before any push, is one `Commit`: `People`, `Identities`, `Applied rows`, `Seen events`, `Window events`, `Outcomes`, `Pushes` (load-time fixes, cancels, retry resets), `Applied overrides`, and every source and event cursor and the last poll time in `State`. Saving cursors with the keys means a crash never leaves keys recorded but their events unapplied.
- **During pushing**, each batch's ledger rows are a small `Commit` of their own (section 6.10).
- **Phase 2**, after pushing, is a second `Commit`: `Outcomes` again, `Company facts`, the `Export` tables, `Log`, `Health`, the rest of `State`, and the retention trims.
- **`Ranked`** is written after phase 2, in chunks, within the save budget; it is recomputed every run, so a crash or a skipped write costs nothing.
- **Size.** Growing tables are appended, not rewritten. Input rows are processed in capped chunks (`ingest_chunk_rows`, default 2,000 per run; events are not chunked), with progress saved in the same commit, so a large first import finishes over several runs. While a backlog remains, the run scores and exports but does not push to cold or non-cold lanes, because an unread opt-out or an unmerged duplicate may sit in the backlog. If phase 1 still exceeds one request, the backend returns `ErrTooLarge`; the run reloads and redoes the chunk at half size, and a second `ErrTooLarge` stops pushing for the run and reports it in `Health` with the setting to lower. Any other commit retries once at the same size, then fails the run.

**Sheets specifics.**

- **The write queue.** Only the receiver writes the `Events` tabs, and the run writes every other tool tab. The receiver gathers incoming events for up to a few seconds and appends them in one `AppendEvents` call, retrying when Google answers "slow down", and only then answers Apollo, so no event is acknowledged before it is stored and the receiver stays inside Sheets' limit of about 60 writes a minute per service account.
- **Monthly tabs.** The receiver appends only to the tab for the current UTC month, by its own clock, and creates it on the first append, protected in the same request for the receiver account (which appends) and the run account (which later deletes it). If a second receiver created it first, the duplicate-name error means "already exists": it re-reads and appends. A tab is never written once its month and a one-hour grace have passed, so deleting it later cannot race an append. An event's sequence is its (tab, row) from the append response; the backend keeps a position per tab inside its one opaque cursor, so an append during the grace hour to last month's tab is still read. A tab whose row count drops below its position blocks pushing and shows in `Health`.
- **Commits and protection.** A commit is one `batchUpdate`, which Sheets applies all-or-nothing; rewritten tabs keep their sheet ids, so protection, filters and team formulas survive. Setup protects the `Events` tabs for both accounts and every other tool tab for the run account only; each account has its own Sheets write quota, so a run never starves the receiver. Ledger rows are found by their key columns (lead id, lane id, step), never by row position, and `doctor` flags a ledger that shrank.
- **The lease** is a file (`leadscore-lease.json`: owner and expiry) in a Cloud Storage bucket named in `leadscore.yml`. Taking it is a write conditioned on the file's generation number, which Cloud Storage refuses if another run changed it first; that compare-and-swap is what Sheets lacks. Setup creates the bucket in the team's project and gives the run account object access to it only.
- **Local commands** (`set-status`, `merge`, `mark-distinct`, `retry`) write only `Overrides`, which the run re-reads, so they need no lease; this is the same on SQLite. The row formats are contracts section 4, and what `set-status` writes is section 7.
- **Size.** Every tab is created at exactly its column count, because Sheets counts empty cells toward its 10-million-cell cap. The contracts doc's budget puts a 20,000-lead install at about 60% of the cap; `doctor` warns at 70%. Setup sets the spreadsheet to recalculate every hour, so the `Health` staleness formula updates with no edits.

**SQLite specifics.** WAL mode with a busy timeout, so `leadscore serve` can write events during a run. The lease is a row in `State`, not a file lock. Events use `INTEGER PRIMARY KEY AUTOINCREMENT`, so deleting processed rows never lets a new event reuse a number at or below a cursor. `Commit` is one transaction. Unique keys: `pushes(lead_id, lane_id, step)`, `seen_events(event_key)`, `identities(key)`, `applied_rows(source_id, row_id)`. Indexes: `outcomes(lead_id)`, `window_events(at)`. The database lives on a named Docker volume, and every CLI command runs inside the container (`docker compose exec leadscore leadscore ...`): WAL locking is not safe across the Docker Desktop VM boundary, so a host binary must never open the container's file; the `store` check fails if one does.

**Store and setup pairs.** Both stores are built in. Cloud Run keeps no files, so the hosted path uses Sheets: `run` and `doctor` refuse a SQLite store or CSV paths when Cloud Run's `CLOUD_RUN_JOB` or `K_SERVICE` variable is set. Every other difference follows the store type. The Docker paths use SQLite by default and may use Sheets. A SQLite install can also write `Ranked` and `Health` to a Google Sheet as an optional read-only view for the team.

**Plug-in stores.** PostgreSQL is not built in. A team that outgrows the built-ins implements `Backend` and `EventLog` for its database, runs the public `storetest` suite against it, and builds a custom binary that registers it; the codec, merge and engine are unchanged. `docs/postgres-store.md` is a short guide. Its one sharp edge: a Postgres serial column is assigned at insert, not commit, so a slow transaction can surface an event below a sequence a run already read; the guide serializes appends, and `storetest` has a case that interleaves a slow append with a read, and one that fails partway through `Commit` and asserts nothing was written.

**Schema version.** `State` holds a `major.minor` schema version, and `leadscore.yml` and the rubric carry `version:`. Releases change the store only by adding tables or columns (a minor bump), which `run` creates on load; an older binary runs on such a store and ignores what it does not know, so rolling back is deploying the previous tag. Only a newer major version, or a config version this binary does not know, is refused. There is no migration tool; a future breaking release brings its own.

**No data is lost by re-scoring.** Only `Ranked` is recomputed; every tier or priority change goes to `Log` with the rubric version.

### 6.7 Events and detectors

- **The receiver stores raw requests:** `(seq, received_at, kind, body)`, after the secret check. The secret is stripped from the body first, so reading the store cannot reveal it. The run parses stored requests with core's parsers, in the same binary.
- **Progress by sequence, not time:** each run reads events above its saved cursor (positions per monthly tab on Sheets, the rowid on SQLite), up to what is present at start. Reading by sequence means an event committed late with an early timestamp is never skipped. Events from other sources (CSV or Sheet event rows) use each source's own cursor.
- **De-duplication keys:** a visit is person key (core's `sourceID()`) plus event name plus the vendor's `visited_at`; with no usable `visited_at` (the visit is then timed at receipt), person key plus event name plus page plus the UTC day the request was received, so a redelivery the same day is one event and a repeat visit on another day still counts (accepted limits: no-time visits by one person to one page on one day collapse into one, and a retry across UTC midnight counts twice); a `visited_at` later than the received time is stored as the received time but keyed as sent, so a retry keys the same; a company-only visit is employer domain plus event name plus time. A reply event uses core's `eventKey()`, which identifies a stage change, so reply counts mean stage changes; a polled reply uses message id plus label (with no message id, person key, contact id first, plus label plus time, the received time when the reply has none, so two people's opt-outs never collapse). An event row from a CSV or Sheet source uses source id plus person key plus event name plus event time. Parsed keys go to `Seen events` in the same commit as the cursor.
- **Events for people not yet known.** A reply, sent or unsubscribe event, or an identified website visit (as core does), whose person matches no lead creates a lead in phase 1, under the reserved source id `receiver`, so an opt-out that arrives before that person's CSV row is never lost; the later row merges into it.
- **Apollo-held.** The first Apollo sent, reply or unsubscribe event for a lead sets `apollo_held_at` on `People` in phase 1, and nothing clears it, so the fact outlives the 90-day event window (section 6.10, rule 4).
- **Windows.** Each parsed event is also appended to `Window events`, kept for a fixed 90 days rather than the current rubric's longest window, so a rubric that later widens a window finds the history already there. Detector counts read `Window events`, so three visits spread over three runs still count three. Windows are `(now - window, now]` in UTC.
- **Detectors** compile through the same CEL path over aggregates computed from `Window events`, with three kinds: `count_in_window`, `first_seen` and `change` (contracts section 2). Attribute checks are plain conditions. `first_seen` times are kept on `People` and previous values for `change` in `Company facts`. Custom kinds implement `Detector`, which receives the lead's or the company's `Window events`.
- **Deleting processed events.** At the end of a run, the engine deletes a monthly `Events` tab once the receiver has moved on to a newer tab, an hour has passed since that month ended, every row in it is at or below its committed cursor, and its newest event is older than 90 days. SQLite deletes rows on the same rule. Every fact a later run needs is already in `Window events`, `Seen events` and `Outcomes`, so nothing reads deleted events, and raw prospect data is not kept forever.

### 6.8 Enrichment

The Apollo company lookup (copied, with Retry-After handling) fills facts for domains with none or with facts past the max age, within a per-run and a per-day lookup budget, so a large first import cannot spend a month of credits in a day. A "not found" is recorded so the domain is retried only after the max age.

### 6.9 Run order

1. **Start.** Cloud Scheduler starts the Cloud Run job (Google Cloud), or the timer in `serve` starts a run (Docker). Read `leadscore.yml` and the rubric fresh: hosted, from one Secret Manager secret that holds both, so a run never pairs a new rubric with old settings; on Docker, from disk, so an edit applies on the next run with no restart (except `schedule`, which `serve` reads at start). A file that fails to load fails that run. Take the lease (owner run id, expiry the deadline plus the save budget plus a 30-second margin); an expired lease is taken over and logged; a live one means skip, logged; the next run that holds the lease flags repeated skips in `Health`.
2. Load the tables; check the schema version. The in-run `doctor` checks (contracts section 10) run after step 5, once this run's rows are merged and folded, so the rubric check sees this run's columns.
3. Fetch sources, merge input rows, and parse events above the saved cursors, up to what is present at start. With `replies: polling`, poll Apollo here when the last poll is older than the polling interval, so polled replies go through the same parse, de-duplication and phase 1 commit as every other event.
4. Enrich within budget.
5. Fold events and overrides into statuses (section 6.12).
6. Company rollups and company-level derivations; per-lead derive and score.
7. **Phase 1 commit.**
8. **Pre-push checks**, for every lead with a step this run may call: pending retries, plus new pushes within the run's limits and a 10% margin to replace leads a check removes: the HubSpot batch lookup (contact opt-out by email, deals per company), the stage of every stored open deal (looked up each run for every company that has one, candidate or not, so a closed-lost deal releases its company), and the Apollo contact lookup by email for its opt-out flag. The last one catches a person who clicked an unsubscribe link without replying, which neither polling nor HubSpot may show. A failed lookup blocks cold and non-cold lanes for that lead. Statuses learned here are folded in, and derive and score re-run for the affected leads.
9. **Select lanes and push** in batches of 25 leads. Before each batch, check the lease is still held, and stop if not. Before each batch with a cold or non-cold lane, re-read every event above the committed cursor and the Overrides tab and re-fold statuses; then check each lead's folded status once more just before its push, with no extra read. These re-reads only decide who may be pushed: they do not move cursors or write `Seen events`. Export lanes are not pushed in batches: their tables are written in phase 2 with no ledger rows (section 6.11).
10. **Phase 2 commit**; write `Ranked`; release the lease.
11. **Report.** Hosted, the job execution's success or failure. On Docker, one summary log line per run and the result in `Health`, which `/healthz` and `leadscore status` read (section 6.13).

**Deadline.** The whole run has a deadline (default 12 minutes on every path). Past it, pushing stops, and the run saves within a fixed 90-second budget and reports unhealthy. If the deadline arrives before phase 1, phase 1 commits what was processed so far (it is chunked) and nothing is pushed. At the deadline plus the save budget, 30 seconds before the lease expires, the run's context is cancelled on every path, so no run outlives its lease; hosted, the job's task timeout is the same bound. `doctor` rejects a hosted schedule shorter than that bound. A run checks it still holds the lease before each commit and releases it only if it does, so a stalled run cannot overwrite or release its successor's work; the short gap between a check and its write is accepted.

**The Docker timer** starts a run when `serve` starts, then starts the next one a full interval after the previous run ends, so runs never overlap and a missed tick is never queued. A run that panics is recovered and recorded as failed, so the receiver keeps serving.

**Shutdown.** On SIGTERM the run stops pushing, writes the ledger first, then saves what it can and releases the lease. `compose.yaml` sets `stop_grace_period: 120s`, because Docker's default 10 seconds would cut the save budget short. Every outbound call has a 30-second timeout, as core's clients.

### 6.10 Lanes, ledger and limits

**Lane kinds:** `cold` (outreach to people who have not engaged), `non-cold` (acts on engagement), `export` (writes to a list). Every lane and every source must have a stable `id:` in the rubric or config, separate from its display name; the ledger and source ids key on it, so renaming a lane or a tab changes nothing.

1. **Built-in checks on every lane:** not `unsubscribed` or `blocked`; no unresolved duplicate; no rubric `conflicts` flag; a valid email for cold and non-cold lanes (export lanes accept LinkedIn-only leads).
2. **Cold lanes** also skip any status in section 6.3 that blocks cold lanes, and any lead at a company with an open or won deal.
3. **One cold push per lead, ever,** counted across all of a lead's identity keys, using the lane kind stored on each ledger row. A cold step holds the cold push once its call may have gone out, whatever the outcome: a timeout or a crash mid-call may still have enrolled the person. Only a step never called releases it, and while such a step is pending the lead stays in that lane. The ledger states and exactly when each holds are contracts section 8. A refusal meaning the person is already being worked (for example already active in another Apollo sequence) also counts. The lead goes to the highest-priority matching cold lane. Each non-cold lane may push a lead once; each export lane lists a lead once, and an export never counts as the cold push.
4. **A lead Apollo already holds is never enrolled in an Apollo sequence,** as core skips Apollo-origin leads. "Apollo already holds" means `apollo_held_at` is set: the lead has had an Apollo reply, sent or unsubscribe event, however long ago (section 6.7). Website visits and Apollo company enrichment do not count, so a visitor with no Apollo engagement can still be enrolled. A CSV export from Apollo is ordinary input and does not count either; teams can mark such a source `apollo_held: true`, and merge then sets `apollo_held_at` on every lead it applies a row from.
5. **Pushes start disabled:** a new install ships with `pushes_enabled: false`, which stops cold and non-cold lanes; export lanes run regardless, since they contact no one. Setup walks through `--dry-run` and `explain` before the team turns pushes on; afterwards they go straight out.

**Limits** live in the rubric: `max_pushes_per_run`, `max_pushes_per_day` and `timezone` (default UTC). A push is one (lead, lane) whose first step's `first_started_at` falls on that day in that timezone; retries of earlier pushes are free; export lanes are uncounted; non-cold lanes claim the budget first, then cold lanes by priority and score.

**The ledger** has one row per (lead id, lane id, step), with state `pending`, `done`, `failed` or `cancelled`.

- **Identity.** Identity keys are resolved through `Identities`, not copied; after a `same_as` merge, rows stay under their original lead id and a done row on either id counts as done for the survivor.
- **Before and after each batch.** The write before the calls marks every step about to be called with the run id (`intent_run`). The write after records each result, sets `called_at` on every step actually called (and never clears it), and clears `intent_run`. A crash between the two leaves `intent_run` set, which holds the cold push: the safe direction.
- **Retries.** A pending step is called again next run only if the lead still passes every check; otherwise it becomes `cancelled`. After three counted attempts a step becomes `failed` and stays failed until a `retry` row in Overrides resets it; `leadscore retry` writes one, each request applies once, and `doctor` prints the count and the command.
- **Refusals.** An `ErrRefused` step becomes `cancelled` with the reason and does not make the run unhealthy.

Non-cold and export steps never affect the cold push.

### 6.11 Sinks

Every sink is find-or-create by its step key; `sinktest` replays each step after a simulated crash and asserts one vendor-side object.

- **HubSpot contacts:** search by the `leadscore_lead_id` property first, then by email, so a crash followed by an email correction never creates a second contact; a 409 duplicate-email conflict counts as found; reuse an existing contact untouched; else create with the configured properties. `leadscore setup hubspot` creates the custom properties (new code) and resolves pipeline and stage by name (as core does).
- **HubSpot deals:** one per company. The deal carries a `leadscore_company_domain` property (not unique; the ledger and `Related` keep one open deal per company). Within a run, a company-to-deal map filled from the ledger, and updated in memory the moment each deals step succeeds, is checked first; deals steps in a batch run one at a time; the property search runs only when the ledger has no deal for that domain, because HubSpot's search can lag a fresh write. Later leads are associated with the existing deal. Only open deals are reused: the ledger map and the domain search both skip a closed-lost deal, so a later positive reply at that company opens a new one. The map covers every `hubspot:deals` lane.
- **Apollo sequences:** `contact` (create with dedupe on), then `enroll` as a separate step. The call, mailbox id, key scope and re-enroll behaviour are verified before building.
- **Export list:** not a `Sink`; the engine owns it. One export table per export lane, written through `Commit`, as a tab on a Sheets store, or one CSV per lane in `export.dir` on SQLite (a bind-mounted `./out` on Docker), or a tab in the optional Sheet view. A lead is added once, and each run refreshes every listed lead's `status` and `do_not_contact` columns, so a team sending from its own tool filters out anyone who has since opted out (columns in contracts section 4). The README tells CSV-only teams to filter on `do_not_contact` before every send.


### 6.12 Receivers and outcomes

The receiver in `leadscore serve` accepts Apollo workflow requests with the shared secret in a header or the body. We ship copy-paste templates for each workflow with exact event names and bodies.

- **Public address.** Google Cloud: Cloud Run gives `serve` its public HTTPS address. Docker on a server: the compose file's optional Caddy proxy gets a certificate for the team's domain. Docker on a laptop: there is no public address, so the README offers two choices: **poll** (replies by polling Apollo, website visits by CSV export; needs no extra account), or **a Cloudflare Tunnel** (free) that gives the laptop a public URL for live webhooks. The README says plainly that a tunnel only works while the laptop is awake: webhooks Apollo sends while it sleeps fail, and are lost unless Apollo retries them. The pre-push Apollo opt-out lookup (section 6.9) still keeps an unsubscribed person out.
- **One instance (hosted):** the service runs at most one instance, serving many requests through one write queue. Cloud Run can briefly run a second during a surge or a redeploy; per-tab positions and the append-response sequence (section 6.6) keep that safe.
- **Answering:** an event gets 2xx once the write queue has stored it. The total hold (batch window plus retries) is capped at 10 seconds, after which the request gets 5xx so Apollo can retry; Apollo's own timeout and whether it retries a timeout are checked before building. Oversized bodies are cut down to fit one Sheets cell (contracts section 5.4).
- **The receiver secret** goes in a header or the body, never the URL, because Cloud Run logs request URLs; it is stripped from the body before storing. A previous secret is accepted while it is set, so it can be rotated without dropping events. Anyone with the secret can forge events, including a positive reply a deal lane acts on, so the README warns teams to keep it private (contracts section 5.1).
- **Silence detection:** when receivers are configured, `Health` and `doctor` flag any expected event kind (contracts section 5.3) with none in the last N days.
- **Replies come from the receiver or from polling, never both,** so one reply cannot count twice under two keys. Polling runs only with `replies: polling`: every six hours, `emailer_messages/search` with the replied filter from the earlier of now minus (`polling.sequence_length` plus the margin) and the last poll minus the margin, so an outage loses nothing, dropping repeats by message id plus label.
- **Reply labels (polling)** map to statuses as in contracts section 5.5; teams can override per label.

**Status rules** are a fold recomputed each run; the precedence order, with a worked example, is contracts section 7.

**How a lead reaches `deal`.** Only when its company has an open or won deal: one the engine created (tracked in the ledger), or one found in HubSpot through the contact-to-company association, or by looking the company up by domain when the lead has no HubSpot contact (row 38). Each run reads the stage of the engine's own deals by stored deal id; closed-lost releases the company. A positive reply sets `replied_positive`, never `deal` directly; a team decides through a non-cold lane whether it becomes a deal. This differs from core on purpose (row 39).

### 6.13 CLI, configuration and run health

| Command | What it does |
|---|---|
| `leadscore run [--dry-run]` | One full run (the Cloud Run job's command; the Docker timer runs the same loop) |
| `leadscore explain <person>` | A lead's verdict and reasons |
| `leadscore doctor` | One pass/fail line per check (contracts section 10) |
| `leadscore ranked [--csv]` | Print every lead's verdict, or write it as CSV |
| `leadscore facts [--csv]` | Print every company's stored facts (`Company facts`: each fact's value and origin, the value a change replaced, the enrichment times), or write the table's own columns as CSV. Read-only, like `doctor`: no lease, no writes, SQLite opened read-only |
| `leadscore serve [--every [interval]]` | The receiver and `/healthz`; with `--every`, also runs the loop on that interval, or on `schedule` when none is given (Docker) |
| `leadscore status` | Print the `Health` table: last result, last success, open problems |
| `leadscore set-status <person> <status\|none\|resubscribe>` | Replace, remove, or undo a manual status in Overrides |
| `leadscore merge <person> <person>` / `leadscore mark-distinct <person> <person>` | Resolve namesakes in Overrides |
| `leadscore retry [--lane X] [<person>]` | Write a `retry` row in Overrides (person `*` when omitted); the next run resets those failed pushes once |
| `leadscore config push` | Hosted: upload `leadscore.yml` and the rubric together as one new Secret Manager version; the next run uses them |
| `leadscore setup hubspot` | Create custom properties, resolve pipeline and stage |
| `leadscore rules check <file>` | Compile a rubric and report errors |
| `leadscore config get <key>` / `config set-hosting k=v...` | Read a value / write the `hosting` block (used by `setup/gcp.sh`) |
| `leadscore setup sheet [--view] [--repair]` | Create the spreadsheet from the schema, the SQLite read-only view, or repair its settings |
| `leadscore healthz` | Call the local `/healthz` (the compose health check) |

Every command takes `--config` and `--rubric`.

`<person>` is an email, LinkedIn URL or lead id. The image's entrypoint is `leadscore`, so on Docker every command runs in the running container as `docker compose exec leadscore leadscore <command>`. `leadscore.yml`, abridged (every key is in contracts section 3):

```yaml
version: 1
store: { type: sheets, spreadsheet: "<id>", lease_bucket: "<bucket>" }   # or { type: sqlite, view_spreadsheet: "<id>" }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] }, { id: offline-events, type: sheetsource, tabs: [Events import], events: true } ]
enrich: { type: apollo, max_age: 30d, max_lookups_per_run: 100, max_lookups_per_day: 400 }
replies: receiver            # or: polling; each path's example file sets it explicitly
sinks: { apollo: { mailbox_id: "<id>" }, hubspot: { pipeline: "Sales", stage: "Prospect" } }
pushes_enabled: false
schedule: 15m
```

Inside a container, secrets come only from environment variables (`APOLLO_API_KEY`, `HUBSPOT_TOKEN`, `LEADSCORE_RECEIVER_SECRET`, optional `LEADSCORE_RECEIVER_SECRET_PREVIOUS`); hosted, Cloud Run fills them from Google Secret Manager, and a local command on a hosted install with an empty variable reads the key from Secret Manager as the run account (contracts section 3). **Hosted configuration:** `leadscore.yml` and the rubric are kept together as one Secret Manager secret (format in contracts section 3), mounted into the job and the service as a file; each execution reads the latest version, so a rule change or turning pushes on is `leadscore config push`, with no redeploy. Each run records the rubric version it used in `Health`, and `doctor` compares it with the local file, so the dry run a person reviewed is the rubric that runs. Google access uses standard credential loading, always requesting the Sheets scope: hosted, the job and the service run as their own service accounts the Sheet is shared with, so no key file exists; on Docker, a service-account key file or a keyless configuration, through the same code path.

**Run result.** Healthy is a run that finished, or skipped because another run held the lease. Unhealthy is a vendor unreachable or unauthorized, a step became `failed`, a receiver past its silence threshold, a commit too large, the deadline hit, or a failed in-run `doctor` check. Each run writes its result, last-success time and rubric version to `Health`, and keeps every open problem (failed or long-pending pushes, unresolved namesakes, conflicting or unknown override values, silent receivers, pushes to receiver-only leads) until it is resolved.

- **Google Cloud:** the job execution shows success or failure, and on Sheets a formula in `Health` warns when the last success is older than three intervals, so a paused or deleted scheduler is caught with no run.
- **Docker:** `/healthz` turns unhealthy when the last run failed or none has succeeded in three intervals, and the compose health check calls it, so `docker ps` shows "unhealthy". Each run logs one summary line, and `leadscore status` prints `Health`.
- **No alerting in v1, on any path.** Nobody is emailed or messaged when a run fails: a person has to look, or point an uptime monitor at `/healthz`. The README says so.

### 6.14 Doctor and setup

`doctor` prints one pass or fail line per check, with a suggested fix. The full check list is contracts section 10. The checks that need only store and vendor access also run inside every run and make it unhealthy when they fail.

The README (see section 3) marks each step as run by the agent or done by a person (pasting a key, approving billing). It says up front that Google Cloud suits non-technical teams and Docker technical users.

Both runbooks, step by step, and the upgrade and rollback steps are contracts section 9. On both paths, runs start with pushes off, so the team sees ranked results and reviews a dry run before anything is sent.

### 6.15 Error handling and logging

- A bad input row is skipped with a reason in the Log.
- A vendor outage or rate limit stops that vendor's pushes and leaves them pending; scoring completes; the run is unhealthy.
- A rubric that fails to compile, a store that cannot load, or a live lease stops the run before any push.
- Stdout and Cloud Logging carry lead ids and run ids only, redacted with `logredact`. The `Log` tab lives in the adopter's own store and carries lead id, run id and email, so a person can find who a line refers to.

## 7. Cross-Cutting Concerns

- **Data correctness.** Duplicate pushes are closed by find-or-create sinks, the step ledger, the lease, the pre-push re-check and the in-run deal map; wrong merges by following core's key-conflict rule and keeping domain + name matching off by default.
- **Observability.** The `Health` table (latest result and every open problem), `/healthz` on Docker, the Log, stdout or Cloud Logging, and `doctor`. No alerting in v1 (section 6.13).
- **Tenancy and plan scope.** Not applicable: one install serves one team.
- **Scale.** Sheets suits the 20,000-lead design target (section 6.6); SQLite suits more. Hosted cost: about 2,900 runs a month at the 15-minute default (section 4); S14b measures run time and webhook hold time, and the README states the expected monthly cost.
- **Vendor dependence.** Every Apollo feature has a fallback (CSV facts, CSV visits, polling, export list).
- **Security.** Secrets only in environment variables; the receiver compares the secret in constant time; logs are redacted; logs stay in the team's own Google Cloud project; only Cloud Scheduler's service account may start the run job. The receiver secret travels in a header or the body (never the URL), and is stripped before the body is stored, because Apollo workflows cannot sign requests, so anyone who learns it can forge events, for example a positive reply that feeds a deal lane. Mitigations: the README warns teams to keep the secret private (row 79); it can be rotated with no lost events; pushes to leads known only through the receiver are flagged in `Health`, and the example rubric's cold lanes require `receiver_only` to be false; replays change nothing because dedupe keys are kept for a year.
- **Privacy.** Erasing a person is not a v1 feature (section 3). The published repo holds no Tetriz or Epifi names (apart from the plan's `tetriz-ai` org in the module path, image name and repo links), none of our ICP, and no real prospect data.
- **Output quality.** Our ICP must reproduce core's verdicts; the example ICP must give its hand-checked results.

### Decision ownership

Every behaviour-shaping value is owned by the adopting team and set in `leadscore.yml` or the rubric, except values we fix in code, each with its reason. The full table (owner, where it is set, default, and the reason for every fixed value) is contracts section 11. Two are domain rules no setting can turn off: the status rules (an opt-out is never undone by automation, and a live deal never re-enters cold outreach) and the built-in lane checks (turning them off would allow contacting opted-out or unidentifiable people).

## 8. Testing Strategy

### 8.1 Layers

Each layer tests the behaviour stated in the section named; core's tests are the starting scenarios where listed. All of it runs in CI on every PR with no real keys.

| Layer | Section | Starts from core |
|---|---|---|
| Rubric compiler, example ICP | 6.4 | — |
| Rollups and derivation | 6.4 | `dao/account_observations_test.go` |
| Merge, identities, overrides, namesakes | 6.3, 6.5 | `dao/lead_repo_test.go`, `email_shape_test.go`, `derive_domain_test.go` |
| Events, windows, dedupe, cursors, deletion | 6.6, 6.7 | — |
| Lanes, ledger, limits, cold-push counting (a transient error, a skipped call, a crash between the ledger writes, and a crash followed by a skipped call and a cancel, each followed by the lead matching another cold lane, must never give a second cold push; two leads at one company in one batch open one deal) | 6.10 | gate cases in `relevance/engine_test.go`; push-state cases in `outreach/service_test.go` |
| Outcomes and status rules, including statuses that outlive the 90-day event window and the contracts worked example | 6.3, 6.12 | `outreach/service_test.go` |
| Sinks (`sinktest`) | 6.11 | `hubspot/client_test.go`, `properties_test.go`, `apollo/enrichment_test.go` |
| Stores (`storetest`): every case in contracts section 1 `storetest.Run` | 6.2, 6.6 | — |
| Run control: timer never overlaps, lease skip and takeover, a stalled run that lost its lease writes nothing, deadline before and after phase 1, the hard stop at the lease bound, no pushing with a backlog, SIGTERM mid-batch leaves the ledger consistent, a run panic leaves the receiver up, config re-read each run, `--dry-run` writes and spends nothing | 6.9 | — |
| Receiver: secret, size fallback, concurrent appends | 6.12 | `apollo/website_visit_test.go`, `service_test.go`, `oversize_test.go` |
| Safety: no emails in logs, constant-time secret check with rotation, timeouts, `/healthz` | 6.9, 6.15, 7 | — |
| End to end, both stores and CSV-only: expected `Ranked`, pushes and suppression; an opt-out by event, by Overrides, and before the person's row exists all keep the person out | 8 | — |

### 8.2 Service contract

One binary serves receiving and running, so the only seam is Apollo's requests: golden bodies in `testdata/events/*.json` are stored with the secret stripped and parsed identically on both stores.

### 8.3 Private parity with core

Our ICP never ships: the rubric, the exported verdicts and the ICP conformance cases live only in `LEADSCORE_PARITY_DIR`, outside the repo, and the committed test loads them from there. A one-time export test in core runs core's engine over the tracker fixture (`relevance/testdata/tracker_golden.json`, 116 anonymized rows) plus synthetic rows that hit tier 1 and P0, which the fixture never reaches because its funding and trigger-note columns are blank, and writes the verdicts to JSON. A test in the new repo, skipped unless `LEADSCORE_PARITY_DIR` is set, builds each lead's inputs directly from those rows (each fixture row is its own company with one lead, as core's `golden_test.go` runs it; only synthetic rows put several leads at one company), runs our ICP rubric over them and compares every verdict field: fit signal, tier, priority, needs review, and both score halves, with any deliberate divergence listed by name in a file in `LEADSCORE_PARITY_DIR`. Before the carve (S18), the same rubric runs on our live data once Prashasti (who owns our GTM data) connects it.

### 8.4 Live checks before release

- `LEADSCORE_LIVE_SHEETS`: creates a scratch spreadsheet, saves 20,000 leads and a year of synthetic events, and loads them within a minute; S5's measurement proof.
- `LEADSCORE_LIVE_CLOUDRUN`: deploys the pre-release image to a scratch Cloud Run service (receiver) and job (runs), posts golden Apollo requests to it including a burst above 60 a minute while a run is writing (none lost), reads and writes a shared Sheet as the runtime service account, runs a job longer than 3 minutes from Cloud Scheduler without it being cut off, and redeploys during a webhook burst with no event lost; S14b's proof.
- A Linux CI job runs `docker compose up` and a timer run end to end; S14a's proof.
- Docker on a VM with a domain: Caddy gets a certificate, a golden Apollo request posted to it is stored, and a timer run processes it; part of S16's proof.
- The check-before-building calls in section 10.
- The dogfood run on our own data.

## 9. Release & Operational Readiness

This is a public repo, not a production service, so there is no per-org rollout or feature flag.

1. Run the check-before-building calls; adjust the design if any fails.
2. Build all slices in the private working repo; the suite is green in CI. CI also builds the multi-architecture image and pushes it to a private Artifact Registry in our test project, which every pre-release deploy (S14b, S16, S18) uses; the setup script takes the image reference.
3. Dogfood with our ICP on our data, on Google Cloud (Sheets) and then Docker (SQLite); parity holds.
4. Carve through the company's open-source process: one scrubbed commit, licence, credits, reviewer panel, private staging, then publish when the plan's gates close.
5. Tag `v0.1.0`, the plan's first version; our repo's release workflow publishes binaries for macOS, Linux and Windows and a multi-architecture (amd64 and arm64) container image to `ghcr.io/tetriz-ai/leadscore`, tagged by version, as the plan's other repos ship; announce on LinkedIn. Cloud Run cannot pull from GHCR directly, so the setup script creates an Artifact Registry remote repository that proxies it (contracts section 9).

**Readiness:** `doctor`, `Health`, the run summary and the Log; the README and `SKILL.md` are the runbook. **Rollback:** deploy the previous tag; an older version runs on a store a newer minor version extended.

God-scripts do not apply: behaviour is switched per install in `leadscore.yml`, and there are no operator jobs against a shared database.

## 10. Open Questions & Risks

### Check before building (could change the design)

Deadline: before S11, S12, S14a, S14b and S15 start. Owner for vendor checks: Harshit, with Apollo and HubSpot accounts. S0 saves each Apollo and HubSpot call (endpoint, request, real responses: success, refusal, rate limit, paging) as scrubbed fixtures the fakes replay. Checks use only our own test addresses and a sequence with sending paused.

| Question | Recommended answer | If it flips | Answer | How checked |
|---|---|---|---|---|
| Apollo sequence enrollment: which call; mailbox id; master key; re-enroll a no-op; refused while in another sequence? | Works with a mailbox id; idempotent; refused while active elsewhere, which the sink reports as `ErrRefused` | The sink creates contacts only, or checks enrollment first | | |
| Apollo contacts: does a lookup by email return an opt-out flag, without using credits? does enrolling an opted-out contact get refused? does company enrichment use credits? | Yes, no credits; refused; enrichment uses credits, capped per day | Without the flag, the README warns Apollo-only teams that link unsubscribes are not seen; the daily cap's default is lowered | | |
| Apollo workflow delivery: retries a request that got a 5xx, timed out, or could not connect? its request timeout? sends custom headers? which variable carries a per-visit `visited_at`? | Retries; headers supported; `visited_at` is per visit | Without retries, silence detection is the backstop; without headers the secret goes in the body; missing `visited_at` falls back to the body hash | Custom header: yes, fixed | production (workflow runbook) |
| Apollo reply search: filter by date? returns a reply label with the eight documented values? can a label be added after the reply first appears? | Yes to all three; a later label is a new event | Wider window; without labels every polled reply is `replied_unlabelled` | Date: yes, by send date (`emailer_message_date_range`); `reply_class`: 3 values and `null` seen; no paging record, no reply time | live checks (spike; read-only, 2026-10-08) |
| HubSpot: batch read of contact status by email (does it match secondary emails?); a merged-away contact id read by id or batch read answers with the survivor; contacts linked to companies; deal search lag, and do reads by id and v4 association reads (contact to deals, deal to companies) lag too?; the v4 PUT association; opt-out field `hs_email_optout`; stage metadata (`isClosed`, `probability`); the `INVALID_EMAIL` code and the 409 "Existing ID" text; the search rate limit; token-scope info | Batch read works on the primary email only; merged ids answer with the survivor; search lags seconds, reads by id and associations do not; the field is `hs_email_optout` | Per-contact reads; the in-run map already covers the lag; if associations lag, the 15-minute window and the waiting deal step still hold the company | v4 PUT works; a repeat is a no-op (stated in a comment) | production code |
| Cloud Run: deploys a public image through an Artifact Registry remote repository proxying `ghcr.io`? the service account gets Sheets access with the scope requested? Cloud Scheduler starts the job as its own service account? skipping the sign-in check works under common organization policies? a Secret Manager file is read fresh each execution? jobs retry by default? Cloud Scheduler offered in the Cloud Run region? a disabled secret version still attached stops an instance from starting? | Yes, except job retries, which setup sets to 0 | If the proxy fails, the setup script copies the image into the team's Artifact Registry; the others change setup steps, not the design | | |
| Company sign-off, a `leadscore` card on the plan, copyright holder, copying the `backend/pkg` helpers (plan owners) | MIT, as decided for every repo; a card added before S19; helpers copied as other carves did | Only licence files change, or the helpers are rewritten | | |
| Apollo: key header, enrichment fields, create, visit body | | | Key: `X-Api-Key`. Visit: contact fields, an account block; open: account domain/website/name, contact id, `visited_at` | transport spike; production code; visitor RFC |

Evidence, citing core's records: [S0 answers](oss-outbound-engine-s0-answers.md). Apollo workflow retries are unverified, so the receiver's store-first rule is the safety net.

### Good to know

- **Does Apollo's API list website visitors?** If yes, a scheduled pull could replace receivers. Recommended: keep receivers.
- **Can HubSpot private apps subscribe to webhooks?** If yes, they could replace the pre-push lookup. Recommended: keep the lookup.

## Decisions Log

Choices that follow directly from these rows (event sequencing, atomic commits, stable ids, the event window, cold-push counting and similar) are stated in the sections they govern; numbering keeps gaps where such rows were folded in. "Decided by": the author (Harshit), the author from the plan doc, a reviewer's recommendation the author accepted, or the agent for choices that follow from decided rows.

| # | Decision | Options considered | Choice | Why (+ trade-off accepted) | Decided by | Status |
|---|---|---|---|---|---|---|
| 1 | Repo | Library in core; separate repo | Fresh standalone repo, one-time copy | Must run outside our infra; core and the repo diverge | author, from brief | decided |
| 2 | Shape | Fixed pipeline; plug-ins | Plug-ins at every edge | Other teams use other tools | author, from brief | decided |
| 3 | Rules | Go rules with editable weights; YAML to CEL | YAML compiled to CEL | A different ICP needs no code change; CEL fails typos at load and is sandboxed | author, from brief | decided |
| 4 | Delivery | Phased; single v1 | Single v1 | Nothing useful ships until everything works; accepted, with a thin end-to-end path built first | author | decided |
| 5 | Stores | Sheets only; SQLite only; both, any pairing; fixed pairings | Sheets and SQLite built in, either with either setup, as the plan doc says; `doctor` refuses SQLite where the file is not kept; SQLite can write a Sheet view; PostgreSQL only as a plug-in store | Sheets for non-technical teams, SQLite for volume; the one unsafe pairing is caught | author | decided |
| 6 | Routing | Many lanes per lead; one | One cold push per lead; one push per non-cold lane; export once per lane | Two tools never cold-contact one person; a positive reply can still become a deal | author, from brief | decided |
| 7 | Lane moves | Upgrades; push once | Never move between cold lanes | Copies prod; a lead that heats up stays in its first cold lane | author | decided |
| 8 | Push limits | Hard-coded; config | Per run and per day, in the rubric, with a timezone | Guards sequence quota | author | decided |
| 9 | Review before push | Approval column; straight | Straight, after pushes are first enabled | A bad rule can push a day's limit before anyone notices; limits bound it | author | decided |
| 10 | Apollo replies | Polling; webhooks | Receiver by default; polling per install instead (row 47) | Copies prod; positive judged from contact stage | author | decided |
| 11 | Website visits | Poll; CSV; receiver | Receiver in `leadscore serve`, CSV fallback | Apollo's API shows no visitor endpoint | author | decided |
| 12 | CRM updates on reply | Built in; out of scope | Out of scope | Native integrations do it | author | decided |
| 13 | Deal suppression | Per person; per company | Per company; open or won suppresses, closed-lost releases (stage read by stored deal id) | Core's account model; company-wide suppression is new behaviour here | author | decided |
| 14 | Manual status | God-script; column and CLI | Overrides and `set-status`, logged | No god-scripts outside core | author | decided |
| 15 | Domain + name matching | On; off | Off by default, opt-in per CSV | Can merge strangers and transfer an opt-out | author, from review | decided |
| 16 | Example ICP | Ours; made up | Made up; ours tested privately | Protects our targeting | author | decided |
| 17 | Ownership | Harshit maintains; plan maintainer | Harshit owns delivery; maintainer at publish per the plan | One maintainer model across company repos | author | decided |
| 18 | Company open-source plan | Alone; follow it | Follow it; develop privately first | Company policy; publishing waits for the plan's gates | author | decided |
| 19 | Shared core packages | Copy all; depend on all | Copy `channelauth`, `emaildomain` and the two `logredact` functions, and maintain them here | leadscore is standalone: it depends on no other company repo | author | decided |
| 20 | Rubric fields | Fixed; declared-only; auto plus optional | Every input column usable; `fields` optional | No setup work for most teams | author, from review | decided |
| 21 | Company rollups | Per-lead only; company pass | Company pass first | Parity; mirrors core's account aggregation | author, from review | decided |
| 22 | HubSpot deals | One per lead; one per company | One per company | Core moved to one per company to stop duplicates | author, from review | decided |
| 23 | Ranked vs export | Same; separate | `Ranked` is every verdict; export lanes add a lead once and are uncounted, and each run refreshes listed leads' status (row 80) | CSV-only teams would otherwise get 100 per run | author, from review | decided |
| 24 | Old events | Keep forever; copy to an archive spreadsheet; delete when processed | Delete a monthly tab (or SQLite rows) once fully processed and older than 90 days | Nothing reads them later; less setup; raw prospect data is not kept forever | author, from review | decided |
| 25 | Re-scoring | Changed only; every lead | Every lead every run, changes logged | Windows expire without input | author, from review | decided |
| 26 | Store shape | Query-level; load and save | Load, in-memory run, two-phase save ([ADR-0002][adr2]) | Removes most Sheets transaction risk | author, from review | decided |
| 27 | Google sign-in | Keyless; key; both | Hosted: the Cloud Run service identity, no key; self-hosted: key or keyless through standard credential loading | One code path; hosted needs no secret for Google at all | author | decided |
| 30 | Push idempotency | Reconcile by lookup; find-or-create | Step ledger plus find-or-create, enforced by `sinktest` ([ADR-0003](../adr/0003-a-ledger-with-intent-and-called-markers-for-at-most-once-vendor-calls.md)) | Lookup marks pre-existing or half-done steps as done | author, from review | decided |
| 32 | Lead ids | Email key; minted id with aliases | UUIDv7; identity table keeps every key; on a person's `same_as` merge the older id survives and the other becomes an alias | Leads without email exist; pushes must survive merges | author | decided |
| 33 | Reply-label map | Two labels; all eight | All eight; `out_of_office` no outcome; team override | Every label defined | author, from review | decided |
| 34 | Status rules | New; core's guards plus manual | Contracts section 7 | Core's guards plus the manual layer | author, from review | decided |
| 35 | Event dedupe | Polling only; every event | Every event, keys in section 6.7 | Retries must not inflate counts; repeat visits still count | author, from review | decided |
| 36 | Receiver silence | Reachability; expected events | Expected event kinds | Reachable is not delivering | author, from review | decided |
| 37 | Segment conflict | Built in; rubric | Rubric `conflicts` | Our buyer's concept | author, from review | decided |
| 38 | Deals created in HubSpot | Ignore; association; association plus domain lookup | Contact-to-company association, plus company lookup by domain when a lead has no HubSpot contact | Closes the new-colleague gap for one extra call | author | decided |
| 39 | How a lead reaches `deal` | Positive reply (core); existing deal | Existing deal only | Replies never update the CRM here; diverges from core | agent, from rows 12 and 22 | decided |
| 40 | When lead ids are saved | End; before pushing | Phase 1 commits lead ids and every identity key before any push; ledger rows key on lead id and resolve keys through `Identities` | A crash after pushing must not allow a second cold push | author, from review | decided |
| 42 | Key conflicts in merge | Auto-merge with alias; core's rule | Core's rule: apply the row to the email-matched lead, do not write the conflicting LinkedIn URL, log and count it | One wrong LinkedIn URL must not join two people; matches prod | author | decided |
| 43 | Community adapters | Registry plus custom build; out-of-process protocol | Registry plus custom build; release binaries hold built-ins only | Less to build and version; built-ins cover the target adopters | author | decided |
| 44 | First run | Push at once; start disabled | Start disabled until a dry run is reviewed | An unreviewed agent-written rubric could cold-email 200 people | author | decided |
| 45 | Erasing a person | `forget` command; out of scope | Out of scope | Downstream effects (lost suppression) outweigh it for v1 | author | decided |
| 47 | Replies source | Both; exclusive | Receiver or polling per install | One reply must not count twice | agent, from review | decided |
| 54 | Hosting | The options in section 4's table | Docker on your own machine; Google Cloud Run (always-on); Docker on a server or VM | One image everywhere; Cloud Run is native to the project that holds Sheets access | author, from Surya's reviews | decided |
| 55 | Scheduling | GitHub Actions cron; Firebase scheduled functions; Cloud Scheduler calling an endpoint or starting a job; host cron; a cron sidecar; a timer built into `serve` | Cloud Scheduler starting a Cloud Run job on Google Cloud; the built-in timer on Docker | Native to each platform; the timer works the same on every OS, unlike host cron | author, from Surya's reviews | decided |
| 56 | Setup instructions | `AGENTS.md`; README | The README is the ordered runbook an agent follows end to end | One entry point for people and agents | author, from Surya's review | decided |
| 57 | Plug-in store contract | Tables only; tables plus an event log | `Backend` plus an `EventLog` with commit-ordered sequence numbers, checked by public `storetest` | A team can connect PostgreSQL without forking; the event log is where a naive plug-in would skip events | author | decided |
| 58 | Hosted secrets | Plain Cloud Run settings; Secret Manager | Secret Manager | Costs nothing extra and keeps keys out of sight of project viewers | author | decided |
| 59 | Hosted failure alerts | Cloud Monitoring email; health line in the Sheet | A `Health` tab: each run's result plus a formula that warns when the last success is older than three intervals | No extra API or channel; works even when no run happens; accepted: nobody gets an email | author | decided |
| 60 | Container image | Copy into each team's registry; one public image | One public image on GHCR, deployed through an Artifact Registry proxy (row 82) | One image for everyone | author | decided |
| 61 | Hosted concurrency | Many instances; one instance with a write queue | The receiver service at most one instance with one write queue that stores each event before answering ([ADR-0001](../adr/0001-store-a-webhook-before-acknowledging-it.md)); one task per job execution, no retries; per-tab cursors keep a briefly doubled receiver safe | Removes most write races and quota contention; the target volume needs no scaling out | author | decided |
| 62 | Hosted configuration | Bake into the image; Sheet tab; Secret Manager | `leadscore.yml` and the rubric as one Secret Manager secret mounted into the job and the service, read fresh each run; `leadscore config push` uploads both together | No per-team image; a rule change needs no redeploy; the run records which rubric it used | author, from review | decided |
| 63 | Apollo as a lead source | Separate Apollo people source; events plus CSV | Apollo's workflow events, plus CSV exports for contacts who never engaged | The events carry every engaged contact; a separate source would clash with the Apollo-held rule | author | decided |
| 69 | Apollo plan | Support free plans; assume paid | Teams are assumed to be on a paid Apollo plan (workflows and API); CSV is the fallback | Workflows and the API need it; stated in the README prerequisites | author | decided |
| 70 | Store upgrades | Versioned `migrate` with a backup; additive-only changes | Additive only: `run` adds missing tables and columns; a store from a newer version is refused | v1 is final apart from minor fixes, so a migration tool would have nothing to migrate; accepted: no built-in backup, the README says to copy first | author, from review | decided |
| 71 | Store plug-in writes | Six methods; `ReadTable`, `Lease`, `Commit` | Three methods; every write goes through `Commit` as replace, append, upsert or delete | The interface is frozen at v1; one write path halves what a plug-in store builds and `storetest` checks | author, from review | decided |
| 72 | Retry | CLI writes the ledger on SQLite; Overrides row on every store | Overrides row on every store, applied by the next run | A direct ledger write races a running run's ledger save; accepted: a retry applies on the next run | author, from review | decided |
| 73 | Unsubscribe-link opt-outs | HubSpot backstop only; Apollo contact lookup before pushing | Look up each pushed lead's Apollo contact and treat its opt-out flag as `unsubscribed`; a failed lookup blocks the lead | A link click is not a reply, so polling cannot see it, and teams without HubSpot have no other backstop; accepted: one extra call per pushed lead | author, from review | decided |
| 74 | Laptop webhooks | Backstop polling; document the limit | The README says a tunnel only receives webhooks while the laptop is awake | Keeps receiver and polling exclusive (row 47); row 73 still keeps unsubscribed people out | author | decided |
| 75 | Docker run health | Sheet view by default; failure alerts; health URL | `/healthz` with a compose health check, one log line per run, `leadscore status`; no alerting in v1 on any path | `serve` has no per-run exit code; accepted: nobody is told unless they look or point a monitor at `/healthz` | author, from review | decided |
| 76 | Who the Docker paths serve | Writable Sheet view; Sheets by default; technical users | Technical users; non-technical teams use Google Cloud with Sheets; SQLite commands run through `docker compose exec` | Overrides live in one place, and the simple path needs no service account | author, from review | decided |
| 77 | Run lock on Sheets | Advisory lease in a tab, one scheduler per spreadsheet; block manual pushing runs; Cloud Storage file | A Cloud Storage file written with a generation precondition ([ADR-0002][adr2]) | Sheets has no compare-and-swap, so overlapping runs could push the same leads twice; accepted: one more bucket in setup | author | decided |
| 79 | Forged receiver events | Confirm replies with Apollo before non-cold pushes; warn only | The README warns teams to keep the receiver secret private; no extra check | Keeps setup and runs simple; accepted: a leaked secret can trigger a deal lane until it is rotated; a deliberate exception to company webhook-auth ADR-0791 (points 1 and 5) | author | decided |
| 80 | Opt-outs on export lists | Rewrite each list every run; append with a status column | Append each lead once, and refresh `status` and `do_not_contact` on every listed lead each run | Teams keep a stable list and can still filter out anyone who opted out since | author | decided |
| 81 | Dogfood pushes | Pushes on; pause core's pushes; pushes off | Pushes off on both paths; dogfood compares verdicts with core's | Core already pushes to our Apollo and HubSpot, so pushes on risks contacting people twice | author | decided |
| 82 | Release defaults | Our own; the plan's | MIT, `v0.1.0` and GHCR under `tetriz-ai`, as the plan sets and its other repos ship; no Docker Hub account exists | Licence, version and registry follow the company; Cloud Run reaches GHCR through an Artifact Registry proxy the setup script creates, which costs one step | author | decided |
| 83 | Pre-release infrastructure | Defer; fix now | GitHub Actions on the private repo, a `leadscore-dev` Google Cloud project in `asia-south1` with an Artifact Registry repo `leadscore`, keyless sign-in from CI; all CI settings, so swapping is one change | Not load-bearing; one project serves CI images, live checks and dogfood | author | decided |
| 84 | Opt-out lookups for export-only leads | Run HubSpot and Apollo lookups for export rows; document | Document: export rows reflect receiver, polling and Overrides opt-outs; lookups run only for push candidates | CSV-only teams rarely connect either tool, and per-lead Apollo lookups for every listed lead cost calls | author | decided |
| 85 | Exports and earlier contact | Keep rule 3 (an export never counts as the cold push); mark contacted leads | `do_not_contact` is `yes` for anyone already contacted by a cold push or an Apollo send | A team sending from an export list and also running cold lanes would otherwise reach one person twice | author | decided |

[adr2]: ../adr/0002-batch-job-on-a-store-without-transactions-saves-in-two-phases-under-a-cas-lease.md

## Task Breakdown

The hardened breakdown lives in [oss-outbound-engine-tasks.md](oss-outbound-engine-tasks.md), since this document is at its size limit. Each slice has a brief (scope, core code reused, proof, gate). It has 24 slices (S0 to S21, with S10 and S14 split in two), the order of work, the verification record and the gate roll-up. Ten slices are human-gated, each on a named judgment or access.
