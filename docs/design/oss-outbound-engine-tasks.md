# Task Breakdown: Open-Source Outbound Engine (`leadscore`)

- **Status:** Hardened by `/task-breakdown`, standalone mode, then reviewed slice by slice for handoff (every slice role-played by a fresh agent, plus a review of the seams between slices) and corrected. The RFC's `## Task Breakdown` points here, because the RFC is at its size limit and these briefs carry the per-slice context an agent needs.
- **Design:** [oss-outbound-engine.md](oss-outbound-engine.md) (the RFC) and [oss-outbound-engine-contracts.md](oss-outbound-engine-contracts.md) (the contracts). "RFC 6.9" is an RFC section; "C8" is a contracts section; C12 is the internal contract between slices.
- **Review state:** run `/tech-design-doc-review` on the RFC once more before the first autonomous slice; its round-4 FAIL was fixed but not re-graded.

## 1. The big picture (read this first, whatever slice you have)

**What we are building.** `leadscore` is a standalone, open-source Go binary that runs a team's outbound loop end to end: bring leads in (CSV, Google Sheet tabs, Apollo webhooks), merge them into people, enrich their companies, detect buying signals, score them with the team's own YAML rubric, route each lead to one lane, push it to Apollo sequences, HubSpot or an export list, and read outcomes back so nobody who replied or opted out gets cold outreach. It is carved out of our GTM pipeline in core (`backend/workloom/gtm/`), but it is a new codebase that reuses a few core files.

**Where the code goes.** The private repo `HarshitBadhwar8/leadscore` (it exists, empty), module path `github.com/HarshitBadhwar8/leadscore`, Go 1.25. S1 copies the three design docs into its `docs/design/`; from then on that copy is the source of truth and the core copies are frozen with a pointer to it. Nothing is built in core except S1's freeze pointer on the core design docs, S3's export test, S20's ADRs and Decisions Log links, and S21's pointer. S0 writes its answers into whichever copy is the source of truth when it finishes. The new repo never imports core packages; core files named in a brief are **copied or re-implemented**, as RFC 5 says.

**How it runs.** One binary, two commands: `leadscore serve` (the always-on receiver for Apollo webhooks, plus a run timer on Docker) and `leadscore run` (one execution of the loop). Three setups: Docker on a laptop, Docker on a server, and Google Cloud (Cloud Run service + job + Cloud Scheduler). Two built-in stores: Google Sheets and SQLite.

**Rules every slice follows.**

- **The contracts doc is law,** including C12's internal contracts. If a slice needs something the contracts do not say, stop and raise it as a design question; do not invent it. A slice that must change a C12 contract changes C12 in the same PR.
- **The safety rules are the product.** Never contact an opted-out person; never cold-contact a person twice; never acknowledge a webhook before it is stored. Any change touching these carries its tests in the same slice (RFC 8.1).
- **Tests run in CI with no real keys.** Vendors are faked (C12.1 `internal/fakes`); live checks run only behind their environment variables (RFC 8.4).
- **Our ICP never enters the repo.** Our rubric, our parity data and our conformance cases live only in `LEADSCORE_PARITY_DIR`, outside the repo; the repo becomes public later.
- **Logs carry ids, not emails**, redacted with `internal/logredact` (RFC 6.15).
- **Dependencies** are RFC 5 "Other dependencies"; add nothing else without asking.

**Pre-release infrastructure** (Decisions Log row 83; CI settings, so swapping is one change): GitHub Actions on the private repo; a Google Cloud project `leadscore-dev` in `asia-south1` with an Artifact Registry repo `leadscore`; CI signs in keylessly (Workload Identity Federation), with GitHub variables `GCP_WIF_PROVIDER` and `GCP_CI_SERVICE_ACCOUNT` on the repo. Harshit creates these before S1. The same project serves S5's live check and S18's dogfood; S14b's live check runs in a second, throwaway project.

**Done and verified, for every slice.** The slice's proof passes and its evidence is pasted in the PR; CI is green; the review panel's Critical and Important findings are fixed; the slice's own README or doc lines are updated. There are no feature flags: behaviour is switched per install in `leadscore.yml`.

## 2. Order of work

The graph has no cycles. Slices in the same wave can run **in parallel**, each in its own worktree; a slice starts only when every slice it depends on has merged. Each wave is the earliest a slice can start.

| Wave | Slices (parallel within a wave) | Why it waits |
|---|---|---|
| 0 | **S0** (vendor checks, a person), **S1** (repo, public API, check framework, config) | nothing; S0 must finish before wave 4 |
| 1 | **S2** (rubric), **S4** (SQLite store), **S7** (CSV parsing) | need S1's types |
| 2 | **S3** (parity), **S5** (Sheets store), **S6** (merge) | S3 needs the rubric; S5 needs the store; S6 needs the store and the rubric's aliases |
| 3 | **S10a** (run control) | needs rubric and merge |
| 4 | **S8** (enrichment), **S9** (events and detectors) | plug into S10a; S8 also needs S0's fixtures |
| 5 | **S10b** (lanes and ledger), **S14a** (receiver and Docker) | S10b applies lookup events through S9; S14a needs S5, S9, S10a and S0 |
| 6 | **S11** (HubSpot), **S12** (Apollo sinks), **S13** (export), **S14b** (Google Cloud) | need S10b or S14a; S11, S12, S14b need S0 |
| 7 | **S15** (outcomes) | needs both vendor slices and the receiver |
| 8 | **S16** (doctor, README, setup), **S17** (end-to-end suite) | need every feature slice |
| 9 | **S18** (dogfood) | needs setup, parity and the suite |
| 10 | **S19** (carve and release) | needs dogfood |
| 11 | **S20** (ADRs), **S21** (harness lessons) | close out after release |

Each line reads "slice ← what it waits for":

```
W0   S0 ← —            S1 ← —
W1   S2 ← S1           S4 ← S1            S7 ← S1
W2   S3 ← S2           S5 ← S4            S6 ← S2, S4
W3   S10a ← S2, S6
W4   S8 ← S10a, S0     S9 ← S2, S10a
W5   S10b ← S10a, S9   S14a ← S5, S9, S10a, S0
W6   S11 ← S10b, S0    S12 ← S10b, S0, S8, S9    S13 ← S7, S10b    S14b ← S14a, S0
W7   S15 ← S11, S12, S14a
W8   S16 ← S8, S13, S14b, S15                     S17 ← S8, S13, S15
W9   S18 ← S3, S16, S17
W10  S19 ← S18
W11  S20 ← S19         S21 ← S19
```

**The thin path** (S1, S2, S4, S6, S7, S10a, S9, S10b, S13) gives a working CSV-to-export run on SQLite with no vendor account, Sheets or hosting work. Build it first.

**The critical path** is twelve slices in sequence: S1, S4, S6, S10a, S9, S10b, S11, S15, S16, S18, S19, S20 (S12 can stand in for S11). S0's answers must arrive before wave 4.

**Sequential by necessity:** S1, S4, S6, S10a, S9, S10b. **Safe to parallelize:** S2, S4 and S7; S3, S5 and S6; S8 and S9 (S9 adds only parser files to `adapters/apollo` and leaves its registration to S8); S10b and S14a; S11, S12, S13 and S14b; S16 and S17; S20 and S21.

## 3. Slice briefs

Each brief says why the slice matters, what to build and not build, what to read, which core code to reuse, how to prove it, any human gate, and what later slices rely on.

### S0 — Check-before-building calls

- **Why:** some design choices rest on vendor behaviour we have not seen; S8, S11, S12 and S14a build fakes from what S0 records, and S14b relies on its Cloud Run answers.
- **Build:** run every vendor check in RFC 10's table on real Apollo, HubSpot and Google Cloud accounts. For every Apollo and HubSpot call the slices use (Apollo auth health, enrich, create contact, contact search with opt-out flag, sequence list, enroll, email accounts list, reply search; HubSpot contact search and create, batch read, deal search, create and read by id, associations read (contact to companies, company to deals), company search by domain, pipelines, property list, property and group create, token-scope info), record the endpoint, request, and real responses (success, refusal, rate limit, paging), saved as scrubbed fixtures: one JSON file per call and case, `testdata/vendors/<vendor>/<call>/<case>.json` = `{method, path, query, request_headers (names only), request_body, status, response_headers, response_body}`. Where a 429 cannot be triggered safely or cheaply, record the vendor's documented shape with `"documented": true` and say so. Also save each captured Apollo workflow request body, secret stripped and scrubbed, as `testdata/events/<event name>.json`, and record which Apollo workflow variables can fill the reply-template fields in C5.1. Confirm the Apollo opt-out lookup costs no credits (C6). Fixtures wait in a local folder until S1 merges, then are committed to the new repo. Add **Answer** and **How checked** columns to RFC 10's table; if the RFC goes over its size limit, put the evidence in `docs/rfc/oss-outbound-engine-s0-answers.md` and link it. Use only our own test addresses and a sequence with sending paused. For the Cloud Run checks, deploy any public `ghcr.io` image through an Artifact Registry remote repository, and use a throwaway probe (not committed).
- **Not here:** the licence and sign-off row; the plan owners answer it by S19.
- **Read:** RFC 10; C5.1, C6.
- **Proof:** the table answered, with fixtures for every call.
- **Gate:** human-gated: vendor accounts, and a design call on any failed check. Owner: Harshit.

### S1 — Repo, public API, check framework and config

- **Why:** every slice builds on these; the public API is frozen once released.
- **Build:**
  - The repo: module, folders (RFC 6.1, C12.1), the three design docs in `docs/design/`, CI on GitHub Actions (build, test, vet), and a minimal Dockerfile (entrypoint `leadscore`) that CI builds for amd64 and arm64 and pushes, tagged by commit, to `asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore` through keyless sign-in.
  - The public API exactly as C1, with types in `internal/api` re-exported from the root as aliases; the registry (a duplicate registration panics); `storetest` and `sinktest` declared exactly as C1 with skipped bodies.
  - `internal/check` (C12.4, with `Env` exactly as written), `internal/config` (C3: every key, unknown engine keys rejected, defaults, default paths, the bundle, the rubric path, `config get` and `set-hosting`), the alias table (copied from core) and squash function in `internal/api`, and an empty `internal/model` (`type Model struct{}`) that S4 fills.
  - A CLI skeleton with every command in RFC 6.13 (including `config get`, `config set-hosting`, `setup sheet` and `healthz`), each returning "not built yet" until its slice lands.
  - The `secrets` check (C10, C12.4).
  - In core, a PR adding a pointer to `docs/design/` at the top of each design doc, marking the core copies frozen.
  - `internal/logredact`: copy `Redact` and `VendorErrorDetail` and their tests; drop `RedactStruct`, its tests and helpers; re-point imports.
- **Read:** RFC 6.1, 6.2, 6.13 (command table), 9; C1, C2 (aliases and headers), C3, C10 (`secrets`), C12.1, C12.4.
- **Core code:** `pkg/logredact/redact.go`, `redact_test.go`, `vendor_error_test.go`; the alias map in `processor/gtmleadscsvimport.go` (58-129).
- **Proof:** CI green; a stub adapter of every kind compiles and registers; config loading tests for every C3 default; the image lands in the registry.
- **Gate:** human-gated: approving the public API for freezing, after seeing it compile against a stub adapter. Owner: Harshit.
- **Hands off:** types, registry, check framework and config to every slice.

### S2 — Rubric compiler and evaluator

- **Why:** the rubric is how a team describes its ideal customer with no code change; it is the second public contract.
- **Build:** everything in C2: fields and types (including the company-field and own-name rules), settings, text matching, every condition form compiled to CEL with the C2 variable table, rollups (with `first` as the first present value, oldest lead first), detector config (parsing only; evaluation is S9), derive with "no value", conflicts (block a lead when a rubric `conflicts` field is in its `LeadRef.ConflictFields`), score with band rules, limits, lanes (syntax only), the version hash, `rules check`, and the reason renderer. The evaluator and accessors per C12.3 (including `blocked` and warnings): it computes nothing merge produces. The `rubric` check's compile part (C12.4; S10a adds the field part). The made-up example ICP and the sample CSV in `examples/`, the CSV's headers covering every field the example rubric reads, whose cold lanes require `receiver_only` false (RFC 7), with hand-checked derive and score results for a few leads. Our ICP rubric, written in `LEADSCORE_PARITY_DIR` (never committed), compiles.
- **Read:** RFC 6.4, 7 (security); C2, C10 (`rubric`), C12.3, C12.4.
- **Core code:** copy the band lookup in `relevance/score.go` (`accountCorroborationPoints` 40-49, `corroborationPoints` 73-84); read `relevance/{tier,signal,priority,gate,engine,config}.go` for the rules our ICP must express; use `dao/account_observations_test.go` (core's `aggregateAccountObservations`, `dao/lead_repo.go:577`) as the rollup scenarios.
- **Proof:** compiler tests for every C2 form and load error; rollup scenarios; the example ICP's hand-checked results; our ICP compiles.
- **Gate:** human-gated: approving the rubric format as public and readable by non-technical teams. Owner: Harshit.
- **Hands off:** `rules.Compile` and `Evaluate` to S3, S9, S10a; `Aliases()` to S6; the sample CSV to S7's tests and S13's proof.

### S3 — Private parity with core

- **Why:** proves the rubric format reproduces our production scoring.
- **Build:** in core, `relevance/parity_export_test.go`, skipped unless `LEADSCORE_PARITY_OUT` is set, which runs core's engine over the tracker fixture plus synthetic rows reaching tier 1 and P0 and writes `parity.json`: per row, `{inputs, account_pass{signal, tier, priority, needs_review, account_score}, lead_pass{contact_score}}`. Each fixture row is its own company with one lead; only synthetic rows put several leads at one company. In the new repo, a generic test, skipped unless `LEADSCORE_PARITY_DIR` is set, that builds `rules.Input` from `parity.json`, runs the rubric and conformance cases from that directory, and compares every field, with deliberate divergences listed by name in the directory. Port core's tier, signal, priority, band, region and score tests as conformance cases in that directory (YAML `{input, expect}`); skip the gate, weights, config-loading and version tests. The export writes the rubric's vocabulary, not core's enums: tier 1-4 or null, priority `P0`/`P1`/`P3`/`needs_review`, `fit_signal` yes or null, `needs_review` true or false, scores as numbers; the test compares JSON-normalized values and treats null as no value. Synthetic companies list their leads in first-seen order, and the export builds core's account snapshot from them by core's coalesce rule (any tool, first segment, first trigger) with the contact count. `LEADSCORE_PARITY_OUT` is a directory; inputs use the same column names and raw country spellings as our private rubric.
- **Read:** RFC 6.4, 8.3; C2, C12.3.
- **Core code:** `relevance/testdata/tracker_golden.json` (116 rows) and its `README.md`; `relevance/golden_test.go` (`TestGoldenDiffAgainstTheTracker`, line 45) as the model; `relevance/engine_test.go`; `relevance/config.go`, `engine.go`.
- **Proof:** parity passes, divergences named.
- **Gate:** human-gated: accepting each divergence from production. Owner: Harshit.

### S4 — Model, codec and SQLite store

- **Why:** everything the engine knows lives in the store between runs.
- **Build:** the model and codec per C12.2 for every C4 table (names, column order, keyed maps and keyless slices, `Extra` for unknown columns, the C4 formats, `Encode` by table, `Committed`, `Discard`; schema version starting at `1.0`); `storetest.Schema`; the SQLite `Backend` and `EventLog` (WAL, busy timeout, `AUTOINCREMENT` events, each `Seq` a full resume cursor, the lease row with owner-checked release, `LeaseInspector`, table and column creation on first write, `OpTrim`, `ErrEventsShrank`); the schema version check (`major.minor`, unknown columns kept, never lowered); the public `storetest.Run` with every case in C1; the `store` check's SQLite cases (newer major version; disk not kept, by its mount type; opened outside a container while `opened_by` names one), and `sqlite.MarkOpenedBy`, which `serve` calls when it opens the store.
- **Read:** RFC 6.6; C1, C4, C10 (`store`), C12.2, C12.4.
- **Proof:** `storetest` green on SQLite.
- **Gate:** autonomous: `storetest` is the guardrail.
- **Hands off:** model and codec to S5, S6, S10a; `storetest` to S5.

### S5 — Sheets store and the Sheet-tab source

- **Why:** Google Sheets is the store for non-technical teams and the only store on Google Cloud.
- **Build:** the Sheets `Backend` and `EventLog`: one `batchUpdate` per commit; tabs at exact width with frozen headers, created on first write; monthly `Events` tabs created by the receiver and protected for both accounts (it copies the editors from the existing protected tabs); each `Seq` encoding every tab's position; `AppendEvents` retrying "slow down" until its context ends; `OpTrim`; the Cloud Storage lease file (C4) with `LeaseInspector`. Google sign-in with the Sheets, Drive and Cloud Storage scopes. `RAW` writes and `UNFORMATTED_VALUE` reads for tool tabs, `FORMATTED_VALUE` for people tabs (C4); `DeleteProcessed` returning the cursor without deleted tabs; `ErrTooLarge` over 9MB. `leadscore setup sheet` (and `--view`, `--repair`), run with the person's own Google login so the person owns the file: the spreadsheet created from the C4 schema with the current month's `Events` tab, shared, protected, hourly recalculation, the staleness formula in `Health!H1`, which the store rewrites on every commit that writes `Health`. The `sheetsource` source (parses tabs with `FORMATTED_VALUE`; headers in order; gets `spreadsheet` and `credentials` from the engine, C3). The `sheet-access`, `sheets` and `store` (Sheets cases) checks. `internal/fakes/sheets` and `internal/fakes/gcs` for S17. `LEADSCORE_LIVE_SHEETS`.
- **Read:** RFC 6.6 (Sheets specifics); C1, C3, C4, C9.1 step 6, C9.2, C10 (`sheet-access`, `sheets`, `store`), C12.
- **Proof:** `storetest` green against the fakes (the GCS fake enforces the generation check); `LEADSCORE_LIVE_SHEETS` saves 20,000 leads and a year of events and loads them within a minute.
- **Gate:** human-gated: Google credentials for the live check, and how the template looks to a non-technical user. Owner: Harshit.

### S6 — Merge, identities and Overrides

- **Why:** turning rows from many sources into one person per human is where wrong merges transfer an opt-out to a stranger; it must match core.
- **Build:** `internal/merge` per C12.5: `Normalize` (the C2 alias list plus rubric aliases, squashed headers, the per-row id, email shape and required-key checks), `Apply` (RFC 6.5 in full, including core's rule for an unknown email whose LinkedIn URL belongs to someone else, never-overwrite fill, `People.conflicts`, the C7 `same_as` rules, the `apollo_held` backfill, rejects recorded once in `Applied rows`), `FindPerson` and `ApplyEventPerson` (contact id first, then identities, then `merged_into`), `ParseOverrides` (normalized persons) and `Duplicates`; the primary email and the stored company domain (C4). The merge-produced fields `sources_seen` (distinct channels), `company.leads_seen` and `receiver_only`. Company facts from built-in company columns (origin `input`) and the `Companies` tab (origin `companies_tab`), never replacing a higher origin (C4). The Overrides parser and validator; the CLI writers for `set-status`, `merge`, `mark-distinct` and `retry` exactly as C7 says (request time in `note` for `retry` and `resubscribe`); the `overrides` check (raising `status_conflict:<lead>` and `override_unmatched:<row>`) and the `duplicates` check.
- **Read:** RFC 6.3 (identity), 6.4 (built-in fields), 6.5; C2 (aliases), C3 (`sources[]`), C4, C7, C10 (`overrides`, `duplicates`), C12.5.
- **Core code:** copy `outreach/email_shape.go` and its test; read S1's alias table in `internal/api`; modify `outreach/service.go` `NormalizeContact`, `deriveCompanyDomain`, `deriveRejectionReason` (560-644), `optional`, `canonicalizeLinkedin` (734-748), with `derive_domain_test.go`; copy `pkg/emaildomain/public.go`; re-implement from `dao/lead_repo.go` (`fillLeadNullsSQL` 196, `findOrCreateAccount` 605, `mergeIntoKnownSource` 715-734, `upsertByEmail` 739 and 758-760, `resolveWithoutEmail` 765 and 808-810) and `processor/gtmleadslinkaccounts.go`; scenarios from `dao/lead_repo_test.go`, `company_link_test.go`, `smoke_lifecycle_test.go`; `processor/managegtmlead.go` for the override commands.
- **Proof:** the merge scenario suite, including the unknown-email key conflict, a namesake pair resolved both ways, an opted-out lead merged by `same_as` into a clean one (the survivor reads as unsubscribed through the fold's chain rule, checked with a stub fold), a backfilled `apollo_held` source, two headers resolving to one field, a recorded conflict, an alias change re-applying rows, and the three built-in fields.
- **Gate:** autonomous: core's scenarios are the guardrail.

### S7 — CSV source

- **Why:** CSV is the one input every team has.
- **Build:** the CSV `Source` per C12.5: parse only (UTF-8, BOM stripped, commas, ragged rows, headers in order as written), event rows per C5.2 using the `internal/api` alias table (rejected rows returned with `Attrs["reject"]`, `at` formats, the Apollo visitor export rule).
- **Read:** C1 (`InputRow`, `Event`), C5.2, C12.5.
- **Core code:** read `processor/gtmleadscsvimport.go` (website rows from line 50) and `processor/gtmleadingest.go`; tests `gtmleadscsvimport_visits_test.go`, `gtmleadsimport_test.go`.
- **Proof:** parsing tests, including a BOM file and an Apollo visitor export.
- **Gate:** autonomous.

### S10a — Run control

- **Why:** the spine every piece plugs into; it keeps two runs from colliding and a crash from losing work.
- **Build:** C12.6 in full: the `Run` value (id, `SourceEvents`, `NoPush`, `Problem`) and `Hooks`; `DefaultHooks`; `RunWith` for tests (the `base_url` and `_http_client` keys); `RunOptions.Stop`; the hook error table; the backlog rule; phase contents; `ErrTooLarge` handling; skipped-run detection; config re-read every run, `config_version` from the bundle's secret version, and the Cloud Run refusal of SQLite and CSV paths; the run-level field check (`rubric.Fields()`); calling every source at step 3, saving source cursors, normalizing and merging through S6, passing source events to `Intake`; the lease (take, check before each commit, hard stop 30 seconds before expiry, owner-only release; a skipped run writes nothing); phases 1 and 2 through `codec.Encode`; chunking (`ingest_chunk_rows` across sources, events never chunked) and the no-push-while-backlog rule; `ErrTooLarge` halving; the deadline, save budget and SIGTERM (`PushCtx`); scoring through S2 with the default `Fold`; logging tier and priority changes against the loaded `Ranked` (a lead with no row is not logged); writing `Ranked`; trimming `Log`; the `Health` writer with the C4 keys; running registered checks; the field part of the `rubric` check; setting `State.first_run_at` on the first run; `run`, `--dry-run` (no lease, no writes, no credits, every row, one line per changed lead plus totals), `status`, `ranked [--csv]`, `explain`.
- **Read:** RFC 6.4 (dry-run), 6.6, 6.9, 6.13, 6.15; C3, C4, C10 (`rubric`), C11, C12.
- **Core code:** read `processor/gtmleadsrescore.go` (`scoreLeads` 218, `scoreAccounts` 310) and `outreach/enrichment_mapping.go` `SnapshotToEngine` (95).
- **Proof:** run-control cases with stub hooks: lease skip and takeover, a stalled run writes nothing, deadline before and after phase 1, the hard stop, config re-read, chunked import over several runs; dry-run output.
- **Gate:** autonomous.

### S8 — Apollo enrichment

- **Why:** most scoring rules read company facts a CSV rarely has.
- **Build:** `adapters/apollo/client.go` (owned here: key, base URL, 30-second timeout, the retrying and single-shot call modes, `_http_client`) and the package's registration; the `Enricher` per C6 (fixed funding values, raw country, `latest_funding_at`, budgets, `ErrRateLimited` with partial facts); the `Enrich` hook (skipped on dry-run) writing facts with origin `enrichment` (never over `companies_tab`), changing `previous` and `at` only when a value changes, counting calls made toward `enrich_count`; the `apollo-key` check (auth health, never an enrich call; the warning for Apollo-only teams when S0 found no opt-out flag). `internal/fakes/apollo` (enrichment part) from S0's fixtures.
- **Read:** RFC 6.8; C3, C4, C6, C10 (`apollo-key`), C12.
- **Core code:** modify `outreach/apollo/enrichment.go`, copy `enrichment_test.go`; modify `outreach/enrichment_mapping.go` `CompanyFromEnrichment`; follow `processor/gtmleadsrescore.go` `enrichCompanies` (138-217) and `dao/lead_repo.go` 1322-1345.
- **Proof:** tests against the fake: both budgets, partial facts on a rate limit, a not-found domain skipped until max age, `previous` filled on refresh.
- **Gate:** autonomous.

### S9 — Apollo events and detectors

- **Why:** webhook events are the live signal; detectors turn them into "this company is looking now".
- **Build:** `internal/events` and `internal/detect` per C12.7 (`Key`, `Apply(m, lead, e)` with every C5.3 effect including the reply-label map, the origin rule, the deal fan-out and `merged_into`, pure `Parse` returning events and receiver rows); in `adapters/apollo`, the body parsers for the C5.1 event names (yielding events and receiver input rows), `PolledReplyKey` and `RequiredPaths`; the `Intake` hook: key, person-resolve and window `Run.SourceEvents`; read the event log from `cursor:events` (returning `ErrEventsShrank` unchanged), parse, merge receiver rows, resolve persons through `merge.ApplyEventPerson`, skip events already in `Seen events`, apply effects, append `Window events`, save keys and the cursor in phase 1; polling every six hours (not on dry-run) with `since` = the earlier of now − (`sequence_length` + `window_margin`) and `last_poll_at` − `window_margin`, `last_poll_at` advanced only on success; with `replies: polling`, keying receiver `replied` and `replied_positive` in `Seen events` with no effect and no window row (C5.3); the `Detect` hook (three kinds plus registered ones, lead and company subjects, company `first_seen` in `Company facts` for every event at the domain, and `People.first_seen` for lead events); `State.last_received:<kind>`; event deletion and the `Window events` and `Seen events` trims. If S0 finds no per-visit `visited_at`, RFC 6.7's body-hash fallback covers it.
- **Read:** RFC 6.6 (deleting processed events), 6.7, 6.9 step 3; C2 (detectors), C3, C4, C5.1, C5.3, C5.4, C5.5, C12.
- **Core code:** copy `outreach/apollo/notification.go` with `notification_reserved_test.go`; modify `outreach/apollo/website_visit.go` and `outreach/apollo/service.go` (parser files only in `adapters/apollo`); tests `service_test.go`, `website_visit_test.go`.
- **Proof:** dedupe, window and first-seen tests; three visits across three runs fire a detector; golden bodies in `testdata/events/` parse to the expected events and rows.
- **Gate:** autonomous.

### S10b — Lanes, ledger and limits

- **Why:** where "never contact an opted-out person" and "never cold-contact twice" are enforced.
- **Build:** C7 and C8 in full: the `Fold` hook (status precedence, the merge-chain rule, per-company deals); built-in lane checks (RFC 6.3, 6.5, 6.10); lane selection with Apollo-held fall-through; the cancel-versus-wait rules; reselection of a never-called cancelled row; `first_started_at` and the budget order; the `PrePush` hook (provisional selection, every registered `Lookup` for candidates and pending retries, a lead with no registered `Lookup` counting as looked up, plus one live lead per company with a stored open deal or an export row; failed leads blocked; lookup events applied through `events.Apply`; affected leads re-derived; no lookups on dry-run); the `Push` hook (25-lead batches, lease check, `ReRead` hook and Overrides re-read before each pushing batch with a failed re-read stopping pushes for the run, `intent_run` and `called_at` writes, the stale-`intent_run` conversion at load, `Prior` and `Related` filled in memory, within a company non-cold before cold and one at a time, the deal rule (including deal steps called but not done) re-checked from the in-memory ledger just before each push, no calls for merged leads, `Do` on `Run.Ctx`, `pushes_enabled`, `NoPush`); the ledger-shrank block (C8); `engine.Blocked` and `engine.MatchesLane` (C12.6) for S13; `retry` and `resubscribe` application through `Applied overrides`; `ledger_rows` committed with each batch; the `pushes` check and the ledger part of the `store` check; the dry-run's planned lanes; `sinktest` (C1).
- **Read:** RFC 6.3, 6.5, 6.9 (steps 5, 8, 9), 6.10; C1 (`sinktest`), C2 (lanes, limits), C4 (`Pushes`, `Outcomes`, `Applied overrides`), C7, C8, C10 (`pushes`, `store`), C11, C12.
- **Core code:** read `relevance/gate.go`, the gate cases in `relevance/engine_test.go`, `processor/gtmleadsapollopush.go`, and `outreach/service.go` `RecordReply` (187) and `targetStatusFor` (372).
- **Proof:** lane, ledger and status suites with fake sinks and lookups, covering every C8 state and every cold-push case in RFC 8.1, deleted ledger rows blocking pushes, an opt-out on a merged-away lead blocking the survivor, an automated opt-out after a manual one surviving `resubscribe`, a capitalized Overrides email matching, and a deal opened earlier in the run blocking a colleague's cold push; the pushing run-control cases (backlog, SIGTERM mid-batch, stalled run); dry-run planned lanes.
- **Gate:** autonomous: the cases in this slice's proof are the guardrail.

### S13 — Export lists

- **Why:** completes the thin path; how a CSV-only team gets its list.
- **Build:** the `Export` hook (every run, even with pushes off or a backlog) and the CSV part of `AfterSave`, in `internal/engine/export.go`, per C4: add each newly matching lead once, then refresh `status` and `do_not_contact` (the full C4 rule, using `engine.Blocked`) on every row of every `Export *` table through `merged_into`, upserting only changed rows, with no `Pushes` rows; rewrite each CSV only after phase 2 committed (temporary file and rename).
- **Read:** RFC 6.11; C2 (lanes), C3 (`export.dir`), C4 (export), C12.6.
- **Proof:** a CSV-only run on SQLite fills `./out`; a later opt-out, an Overrides typo, an earlier cold push, and a merge of two listed people each set the right row's `do_not_contact` to `yes`.
- **Gate:** autonomous.

### S11 — HubSpot

- **Why:** contacts and deals for teams on HubSpot, plus the opt-out and deal lookups that keep people out of cold lanes.
- **Build:** the `hubspot` sink per C6: `Steps(dest)` for `contacts` and `deals`; contact search by `leadscore_lead_id` then email, 409 handling; one deal per company built from `req.Related` (open deals only), named by domain, association inside the deal step, the domain property not unique; the error mapping; `setup hubspot` (properties and group, pipeline and stage by name); the HubSpot `Lookup` (opt-out on every email; deals per company and by `CompanyDealID`, one event per company for the strongest stage, a deleted deal as `deal_lost`, stages mapped by closed and probability metadata; per-lead failures); the `hubspot` check. `internal/fakes/hubspot` from S0's fixtures.
- **Read:** RFC 6.11, 6.12; C1, C3 (`sinks.hubspot`), C5.3, C6, C10 (`hubspot`), C12; S0's answers.
- **Core code:** modify `outreach/hubspot/client.go` (`searchByLeadID` 385, `findAdoptOrCreateDeal` 221 without adoption, `PushDeal` 156, `PushAccountDeal` 186, `ResolveDealTargets` 632); copy `client_test.go`, `properties_test.go`; read `outreach/contact_push.go` and `outreach/service.go` `PushPendingDeals` (380), with `account_deal_push_test.go` and `push_resolution_test.go` as scenarios.
- **Proof:** `sinktest` green; two leads at one company in one batch open one deal; a closed-lost deal releases its company.
- **Gate:** autonomous.

### S12 — Apollo sequences, opt-out lookup and reply polling

- **Why:** the cold outreach itself, and how we learn about replies and opt-outs without webhooks.
- **Build:** on S8's client: the `apollo` sink (`Steps("sequence/<name>")` = contact, enroll; sequence names resolved to ids once per run, an unresolved name returning `ErrTransient`; refusals and 429 per C6); the Apollo contact `Lookup` (opt-out flag, per-lead failures; not registered if S0 found no flag); the reply `Poller` (uses `since` as given; emits `reply` events with `label` and `message_id` in `Attrs`, and `events.Key` derives their id, C12.7); the `apollo-sequences` check. The rest of `internal/fakes/apollo`.
- **Read:** RFC 6.9 step 8, 6.11, 6.12; C3, C5.3, C5.5, C6, C10 (`apollo-sequences`), C12; S0's answers.
- **Core code:** copy `outreach/apollo/client.go` `CreateContact` (57-139) and `splitName` (141), replacing `doWithRetry` with the client's single-shot call. Enrollment and polling are new.
- **Proof:** `sinktest` green; repeat enroll is a no-op; a refusal maps to `ErrRefused`; polling with a later label yields a new event.
- **Gate:** autonomous.

### S14a — The receiver and Docker

- **Why:** the always-on half: it must never acknowledge an event it has not stored, and on Docker it also runs the loop.
- **Build:** `serve` per C5.1: routes, secret check with rotation, the receiver-configured rule and the empty-secret behaviour, oversized bodies (C5.4), the 2-second write queue whose `AppendEvents` context ends 10 seconds after the oldest request, `/healthz` (cached; start time when no success yet), the timer (first run at start, fixed delay, interval read at start, panics recovered and written to `Health`), closing `Stop` at shutdown in the C5.1 order, `receiver.port`, calling `sqlite.MarkOpenedBy` when it opens a SQLite store, an in-process handler constructor that takes a clock (for S17). The full Dockerfile (non-root user `leadscore`, `HOME=/home/leadscore`), `compose.yaml` (config mounted at `/config`, named volume, `./out` bind mount, restart policy, a health check that runs `leadscore healthz`, `stop_grace_period: 120s`, optional Caddy). A `leadscore healthz` command that calls the local `/healthz`. The Apollo workflow templates in `setup/apollo/` matching the C5.1 event names; the laptop and server example `leadscore.yml`; the README's "keep the secret private" warning; the `receiver-secret` check. Built against S0's answers on Apollo's timeout, retries and headers.
- **Read:** RFC 4, 6.9, 6.12, 8.2; C3, C5, C10 (`receiver-secret`), C12; core's `docs/operations/gtm-apollo-workflow-runbook.md`.
- **Core code:** copy `pkg/channelauth` (`channelauth.go`, `channelauth_test.go`); re-point `outreach/apollo/oversize_test.go` at the 16KB and 50,000-character limits; read `wire/providers.go` `visitAuthFor` (42-61).
- **Proof:** receiver contract tests (golden bodies stored with the secret stripped and parsed the same on both stores; a wrong secret refused; oversized bodies cut down); the timer never overlaps, and a panicking run leaves the receiver up; SIGTERM during a run saves before exit; a Linux CI job runs `docker compose up` and a timer run end to end.
- **Gate:** autonomous.

### S14b — Google Cloud

- **Why:** the hosted path for non-technical teams.
- **Build:** `setup/gcp.sh` (C9) with `accounts` (ending with the impersonated login), `bucket`, `secrets`, `deploy` (through the `ghcr-proxy` remote repository after release; before release, the private registry image passed directly, C9), `redeploy`, `schedule`, using `leadscore config get` and `set-hosting`; the role table in C9; `config push` (the bundle); keys read from Secret Manager when `hosting.project` is set, only for adapters that need them and never inside Cloud Run; the C3 cron conversion; the `hosting` check; the Google Cloud example `leadscore.yml`; `LEADSCORE_LIVE_CLOUDRUN` (env vars for project and image; run against a project other than the dogfood one; a long run produced by a large synthetic Sheet, with no test-only flags); measuring run time and monthly cost.
- **Read:** RFC 4, 6.13, 8.4; C3, C5.1 (rotation), C9, C10; S0's Cloud Run answers.
- **Proof:** `LEADSCORE_LIVE_CLOUDRUN` passes, including a webhook burst during a run with no event lost; local `doctor` reaches the Sheet as the run account; measured cost in the README draft.
- **Gate:** human-gated: a billed Google Cloud project. Owner: Harshit.

### S15 — Outcomes, end to end

- **Why:** closes the loop: every way we learn about a reply or opt-out must reach the status fold before the next push.
- **Build:** the `ReRead` hook per C12.6 (`events.Parse`, then `merge.FindPerson` and `events.Apply` on `Run.Model`'s `Outcomes`, with no cursor, `Seen events` or `Window events` writes; the polling-mode filter; returns changed leads), registered in `DefaultHooks`; the `receiver-silence` check from `State.last_received:<kind>` and `first_run_at` (C5.3, C10); the `receiver_only_push` problem (cold and non-cold pushes to receiver-only leads this run; clears when the lead is no longer receiver-only); the outcome suite.
- **Read:** RFC 6.3, 6.12; C5.1, C5.3, C7, C10 (`receiver-silence`), C12.2, C12.5, C12.6, C12.7.
- **Core code:** `outreach/service_test.go` as the scenario list.
- **Proof:** the outcome suite: an unsubscribe by webhook, by polling, by HubSpot and by the Apollo lookup (when S0 found the flag) each block the next push; an unsubscribe arriving mid-run blocks the next batch, through `leadscore.Run` (not `RunWith`); statuses outlive the 90-day window; the C7 worked example step by step.
- **Gate:** autonomous.

### S16 — Doctor, README and setup

- **Why:** the product is only as good as a stranger's first hour with it.
- **Build:** the `doctor` command (exit codes, read-only, no lease, per C10) and the checks only it runs (`rubric-version`, `receivers`, `lease`, `pushes-enabled`; `hosting` is S14b's); the view writer in `AfterSave` (S5 owns `setup sheet --view`); the README as the ordered runbook (C9) including the CLI install step (before release, the private image with the gcloud mount), the "no alerting in v1" note, and every README duty stated elsewhere (RFC 6.11 export filtering, RFC 6.12 laptop choice and sleep warning, RFC 7 cost, C4 event-rate advice and the export-row limits, a check that S14a's C5.1 secret warning is present, C9.1 step 6's Workspace exception, any warning S0's answers require); the `enrich` and `sinks` blocks and comments in every example `leadscore.yml`; `docs/postgres-store.md` (prose with untested snippets; appends serialized with `pg_advisory_xact_lock`); `SKILL.md` (name and description frontmatter, the rule-change loop, one troubleshooting entry per C10 row).
- **Read:** RFC 6.14; C4 (`Health`, `view_write_failed`), C9, C10, C12.4, C12.6.
- **Proof:** an agent following only the README reaches a green `doctor` and a filled `Ranked` on Docker (Linux and macOS Docker Desktop), on a VM with a domain (where Caddy gets a certificate and a golden Apollo request is stored and processed, RFC 8.4), and on a fresh Google Cloud project.
- **Gate:** human-gated: billing, a domain, pasted keys, and judging the setup experience. Owner: Harshit.

### S17 — End-to-end suite

- **Why:** proves the assembled loop on both stores, in CI, with fake vendors.
- **Build:** in `internal/e2e` (default CI job), through `RunWith` with `DefaultHooks()` (fake clock, HTTP client pointed at `internal/fakes`) and S14a's in-process handler with the same clock: {SQLite, Sheets} × {Apollo + HubSpot with `replies: receiver`, CSV-only}, plus one polling case; an opt-out by Apollo `unsubscribed` webhook, by Overrides, and before the person's row exists (event in run 1, CSV row in run 2, no push in run 2); the Sheets export tab; the example ICP's full output including detectors. Expected `Ranked`, pushes and detector firings are hand-derived from the example ICP and sample data, written down before the first run, and reviewed in the PR; never regenerated from output.
- **Read:** RFC 8.1; C12.6.
- **Proof:** the suite green in CI.
- **Gate:** autonomous.

### S18 — Dogfood

- **Why:** the persona run: our team set up from the README, on our data, on both paths.
- **Build:** set up Google Cloud (in `leadscore-dev`) and Docker from the README with our ICP rubric from `LEADSCORE_PARITY_DIR`, and connect our data with Prashasti. **Pushes stay off on both paths** (Decisions Log row 81). Inputs are CSV or Sheet exports Prashasti makes from core; Apollo workflows stay on core. Before comparing, wait until every domain is enriched (raise `max_lookups_per_day` for dogfood). Export core's tier, priority, scores and company facts per email from production, join to `Ranked` by email, and require every field equal except S3's named divergences, comparing only leads whose facts match; list mismatched facts as "facts drift" and unmatched leads separately. The comparison script and outputs live in `LEADSCORE_PARITY_DIR`, never the repo.
- **Proof:** the comparison passes; findings filed and fixed.
- **Gate:** human-gated: acceptance on real data. Owners: Harshit, with Prashasti.

### S19 — Carve, scrub and release

- **Build:** before starting, a `leadscore` card on the company open-source plan (source `HarshitBadhwar8/leadscore`, shape Go CLI plus runnable service, acceptance test a CSV-only compose run that fills `./out`, version `v0.1.0`, image `ghcr.io/tetriz-ai/leadscore`). Then carve mode B from a fresh clone of the private repo, expecting the author's yes for the squash, the reflog expire and the gc (or export HEAD with `git archive` and use mode A): one scrubbed commit (no Tetriz or Epifi names except the plan's `tetriz-ai` org in the module path, image and links; no ICP, no real data, no core paths; `docs/design/` dropped, since the README and `SKILL.md` carry the public design), MIT licence, credit for the authors (per `/open-source-carve` 5.1) of every copied core file plus the private repo's authors, the plan's reviewer panel, private staging, publish when the plan's gates close; rewrite the module path to `github.com/tetriz-ai/leadscore`; switch `internal/logredact` to the published module if it is out by then (otherwise keep the copy with its notice and file a follow-up); tag `v0.1.0` at publish; release binaries for macOS, Linux and Windows and the GHCR image; Harshit announces on LinkedIn.
- **Read:** RFC 9; the open-source plan; `/open-source-carve`.
- **Proof:** the carve log; panel passes; `v0.1.0` binaries and image published.
- **Gate:** human-gated: the plan owners' sign-off (Rohan as maintainer).
- **Skill:** `/open-source-carve`.

### S20 — ADR extraction

- **Build:** ADRs in core for Decisions Log rows that generalize beyond this product, status Proposed until Harshit accepts them in review, or a note that none do (in the RFC's Task Breakdown pointer); one issue per ADR; link each ADR by replacing text in its Decisions Log row, not adding any.
- **Proof:** the ADR files, linked from the RFC.
- **Gate:** autonomous. **Skill:** `/adr`.

### S21 — Harness lessons

- **Build:** following the harness-education contract in `/tech-design-doc-writing`'s task-breakdown section, with `/harness-education`'s mining steps run against the **private working repo's** merged PRs and review threads (S1 to S19, any author) plus core's S3 PR: re-ground against the shipped code. Defect classes the reviews caught more than once become, **in the public repo** by PR reviewed by its maintainer, a Go test or golangci rule (mechanical) or a line in its `CLAUDE.md`/`AGENTS.md`; the reviewed lessons go into its `SKILL.md`, scrubbed for privacy. Core gets only a pointer in `backend/workloom/gtm/CLAUDE.md`. Predicted surfaces (to re-check, not a spec): sink idempotency, Sheets write limits, lease handling, merge edge cases.
- **Proof:** the edits, each checked against shipped code, through the review panel.
- **Gate:** human-gated: a person picks which lessons are published. Owner: Harshit. **Skill:** `/harness-education` (mining steps).

## 4. The table

| # | Slice | Layers crossed | Demoable proof | HITL/AFK | Depends-on | RFC anchor | Implementing skill |
|---|---|---|---|---|---|---|---|
| S0 | Check-before-building calls and vendor fixtures | Apollo, HubSpot, Google Cloud | RFC 10 answered; fixtures for every call | human-gated: vendor accounts | — | RFC 10; C6 | none — vendor verification |
| S1 | Repo, public API, check framework, config, CI and pre-release image | module, CI | CI green; stubs register; config defaults tested; image in the registry | human-gated: freezing the public API | — | RFC 6.1, 6.2, 9; C1, C3, C12 | none — new repo; carve later via `/open-source-carve` |
| S2 | Rubric compiler, evaluator, example ICP and sample CSV | rules | Compiler tests; rollups; example hand-checks; our ICP compiles | human-gated: the rubric format | S1 | RFC 6.4; C2, C12.3 | none — new code |
| S3 | Private parity with core | core, rules | Parity passes, divergences named | human-gated: accepting divergences | S2 | RFC 8.3; C12.3 | `/test-writing` (core side) |
| S4 | Model, codec, SQLite store, `storetest` | store | `storetest` green on SQLite | autonomous | S1 | RFC 6.6; C1, C4, C12.2 | none — new code |
| S5 | Sheets store, lease file, `setup sheet`, Sheet-tab source | store, Sheets, Cloud Storage | `storetest` on fakes; `LEADSCORE_LIVE_SHEETS` | human-gated: credentials and template look | S4 | RFC 6.6; C4, C9 | none — new code |
| S6 | Merge, aliasing, row ids, Overrides and its CLI writers | merge, CLI | Merge scenario suite | autonomous | S2, S4 | RFC 6.3, 6.5; C2, C7, C12.5 | none — new code |
| S7 | CSV parsing | adapters | Parsing tests | autonomous | S1 | C5.2, C12.5 | none — new code |
| S10a | Run control, hooks, lease, phases, Health, run commands | engine, CLI | Run-control cases with stub hooks; dry-run output | autonomous | S2, S6 | RFC 6.6, 6.9, 6.13; C3, C4, C12.6 | none — new code |
| S8 | Apollo client and enrichment | adapters | Tests against the fake; both budgets | autonomous | S10a, S0 | RFC 6.8; C6 | none — new code |
| S9 | Events, effects, intake, polling call, detectors | events, adapters | Dedupe, window, detector and golden-body tests | autonomous | S2, S10a | RFC 6.7; C5, C12.7 | none — new code |
| S10b | Status fold, lanes, ledger, limits, pre-push and push loops, `sinktest` | engine | Lane, ledger, status and pushing suites | autonomous | S10a, S9 | RFC 6.9, 6.10; C7, C8 | none — new code |
| S13 | Export lists with status refresh | engine | CSV-only run fills `./out`; rows flip correctly | autonomous | S7, S10b | RFC 6.11; C4 | none — new code |
| S11 | HubSpot sink, setup, lookup | adapters | `sinktest`; one deal per company; closed-lost releases | autonomous | S10b, S0 | RFC 6.11, 6.12; C6 | none — new code |
| S12 | Apollo sink, contact lookup, reply poller | adapters | `sinktest`; repeat enroll no-op | autonomous | S10b, S0, S8, S9 | RFC 6.11, 6.12; C6 | none — new code |
| S14a | Receiver, timer, Docker image and compose, workflow templates | receiver, Docker | Contract tests; timer and shutdown cases; Linux CI compose run | autonomous | S5, S9, S10a, S0 | RFC 4, 6.12, 8.2; C5 | none — new code |
| S14b | Google Cloud setup script, bundle, live check, cost | hosting | `LEADSCORE_LIVE_CLOUDRUN`; measured cost | human-gated: billed project | S14a, S0 | RFC 4, 8.4; C3, C9 | none — new code |
| S15 | Re-read hook, silence detection, outcome suite | engine | Outcome suite incl. the C7 worked example | autonomous | S11, S12, S14a | RFC 6.12; C5.3, C7 | none — new code |
| S16 | `doctor`, Sheet view, README, Postgres guide, `SKILL.md` | setup | Agent follows README to green `doctor` and filled `Ranked` on all three paths | human-gated: setup experience | S8, S13, S14b, S15 | RFC 6.14; C9, C10 | none — new code |
| S17 | End-to-end suite | all | E2E green in CI | autonomous | S8, S13, S15 | RFC 8.1; C12.6 | none — new code |
| S18 | Dogfood on our data, pushes off | all | Comparison with core passes | human-gated: real-data acceptance | S3, S16, S17 | RFC 8.3, 9 | none — dogfood |
| S19 | Plan card, carve, scrub, release `v0.1.0` to GHCR | repo | Carve log; release published | human-gated: plan owners' sign-off | S18 | RFC 9 | `/open-source-carve` |
| S20 | ADR extraction | docs | ADRs or a "none generalize" note | autonomous | S19 | Decisions Log | `/adr` |
| S21 | Harness lessons | public repo, core pointer | Reviewed guidance edits | human-gated: which lessons go public | S19 | RFC 9 | `/harness-education` |

## 5. Verification record

```
Ground-verified: 45 depends-on edges + 64 touch-points against 627a91c3f (core code unchanged since; the design docs re-checked at the current HEAD)
```

Edges were verified against what the RFC and contracts say each producing slice builds (the new repo has no code yet); touch-points against core's code at that commit. After the slice-by-slice review, the edges were re-derived from C12's ownership.

- S2→S1: `internal/api` types, `internal/check` (C1, C12.4)
- S3→S2: `rules.Compile`, `Evaluate`, `Input` (C12.3)
- S4→S1: `Backend`, `EventLog`, `storetest.Schema` (C1), check framework (C12.4)
- S5→S4: codec, model (C12.2), `storetest` (C1)
- S6→S4: model `Put`/`Delete` and `Commit` for Overrides writes (C12.2)
- S6→S2: the rubric's `Aliases()` for `Normalize`, which hashes them into the row hash (C12.3, C12.5)
- S7→S1: `Source`, `InputRow`, `Event` (C1)
- S10a→S2: scoring at step 6 (C12.3)
- S10a→S6: `merge.Normalize` and `Apply` at steps 3 and 4 (C12.5)
- S8→S10a: the `Enrich` hook, `Run`, check framework (C12.6)
- S8→S0: enrichment and auth-health fixtures (RFC 10)
- S9→S2: detectors compile through the rubric (C2)
- S9→S10a: the `Intake` hook and phase 1 (C12.6)
- S10b→S10a: `Fold`, `PrePush`, `Push` hooks, lease, phases (C12.6)
- S10b→S9: `events.Apply` for lookup events at step 8 (C12.7)
- S13→S7: the CSV source for the thin-path proof (RFC 6.11)
- S13→S10b: lane selection, status fold and built-in checks for `do_not_contact` (C4)
- S11→S10b: `sinktest`, `Related` and the `PrePush` lookup call (C1, C8)
- S11→S0: HubSpot calls and fixtures (RFC 10)
- S12→S10b: `sinktest`, the push loop (C1, C8)
- S12→S0: Apollo calls and fixtures (RFC 10)
- S12→S8: the shared Apollo client in `adapters/apollo` (C12.1)
- S12→S9: `apollo.PolledReplyKey` (C12.7)
- S14a→S5: both-stores contract test; `AppendEvents` with "slow down" (RFC 8.2, C1)
- S14a→S9: parser fields for the oversize fallback; golden bodies (C5.4)
- S14a→S10a: the timer calls the run; `/healthz` reads `Health` (C5.1, C12.6)
- S14a→S0: Apollo timeout, retries and headers (RFC 10)
- S14b→S14a: the image and `serve` to deploy (RFC 4)
- S14b→S0: Cloud Run checks (RFC 10)
- S15→S11: HubSpot lookup events in the outcome suite (C5.3)
- S15→S12: Apollo lookup and poller events in the outcome suite (C5.3)
- S15→S14a: receiver-fed replies; silence detection (RFC 6.12)
- S16→S8: the full run in the setup proof; `enrich` in the examples (C9)
- S16→S13: `./out` in runbook step 7; the view's export tabs (C9.2)
- S16→S14b: the `hosting` check and the Google Cloud runbook (C9.1, C10)
- S16→S15: `receiver-silence`, which the README and `doctor` output describe (C10)
- S17→S8: enrichment in the end-to-end run (RFC 8.1)
- S17→S13: export in the end-to-end run (RFC 8.1)
- S17→S15: outcomes and suppression in the end-to-end run (RFC 8.1)
- S18→S3: the parity rubric and divergences (RFC 8.3)
- S18→S16: setting up both paths from the README (RFC 9)
- S18→S17: the suite green before real data (RFC 9)
- S19→S18: dogfood before carve (RFC 9)
- S20→S19: ordering only (ADR extraction closes the RFC)
- S21→S19: ordering only (harness lessons re-ground on shipped code)

**Touch-points (64, all present at 627a91c3f):** S1 4; S2 9; S3 6; S6 11; S7 4; S8 5; S9 6; S10a 2; S10b 4; S11 7; S12 1; S14a 4; S15 1. Paths and line ranges are in each brief, relative to `backend/workloom/gtm/`, except `processor/` (= `backend/scripts/god-script/processor/`) and `pkg/` (= `backend/pkg/`). Corrections the reviews made: the parity test is `golden_test.go`; `oversize_test.go` tests trimming oversized bodies (S14a); the key-conflict rule sits at three places in `lead_repo.go`; `contact_push.go` is the HubSpot push loop (S11); the per-company deal logic is `findAdoptOrCreateDeal` (221); `labels.go` was dropped; core has no Apollo enrollment or reply polling; the alias table lives in `internal/api` (S1), and applying aliases and the email-shape check moved to merge (S6), since the engine, not the source, applies them; `logredact` is copied until carve.

## 6. Gates: the second pass

**Ten slices stay gated**, each on a judgment or access no automated check has:

| Slice | Why the gate stays |
|---|---|
| S0 | real vendor accounts, and a design call if a check fails |
| S1 | the public API is frozen once released; only a person judges whether it reads well to adopters |
| S2 | the rubric format is public; readability by non-technical teams is a judgment |
| S3 | each divergence from production needs a person to accept it |
| S5 | live Google credentials, and how the template looks to a non-technical user |
| S14b | a billed Google Cloud project |
| S16 | billing, a domain, pasted keys, and judging the setup experience |
| S18 | acceptance on our real data |
| S19 | the plan owners' sign-off |
| S21 | which lessons are published in a public repo |

**Converted to autonomous:** none.

**Checked in reverse** (autonomous slices that carry a safety risk): S6 (wrong merges), S9 (lost or doubled events), S10b (double contact, an opt-out ignored), S13 (an opted-out person on a list), S14a (acknowledging an unstored event), S15 (a lost opt-out). Each carries its guardrail inside its own proof, so none earns a new gate.

## 7. Open items

- **A `leadscore` card on the company open-source plan** (S19's first step); ask Rohan well before S19.
- **Pre-release infrastructure** (section 1): Harshit creates `leadscore-dev`, the registry, keyless CI sign-in and the two repo variables before S1. The repo itself exists.
- **Re-review the RFC** with `/tech-design-doc-review` before the first autonomous slice.
- **S0 first.** Its answers and fixtures feed S8, S11, S12, S14a and S14b.
