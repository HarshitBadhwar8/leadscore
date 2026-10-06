# Contracts: Open-Source Outbound Engine (`leadscore`)

- **Status:** Draft
- **Companion to:** [oss-outbound-engine.md](oss-outbound-engine.md). The RFC owns the design and the reasons; this file owns the exact shapes a builder codes against. Sections 1 to 11 are the product's outward contracts: the public Go API, the rubric grammar, `leadscore.yml`, the store tables, the receiver, vendor fields, status precedence, ledger states, the setup runbooks, the `doctor` checks, and every default. Section 12 is the internal contract between slices, so agents building different slices produce pieces that fit. Section 1 is frozen at `v0.1.0`; before that, a change to it needs the S1 gate owner's approval.
- **Formats used everywhere:** times are UTC in the fixed form `2006-01-02T15:04:05.000Z` (never trimmed, so text order is time order); durations are Go durations plus `d` for days (`90d`, `15m`); numbers are plain decimal; booleans in stored rows are `yes` or empty, except `do_not_contact`, which is `yes` or `no`.

## 1. Public Go API

The public surface is the root package `leadscore`. To avoid an import cycle with the engine, the types, errors and registry are defined in `internal/api` and re-exported from the root as type aliases and thin function wrappers, each with a doc comment repeating its contract. The built-in alias table and the header squash function (section 2) also live in `internal/api` but stay internal: they are not part of the public surface. Built-in adapters register in their `init` functions; a custom build imports its own adapter package and calls `leadscore.Main()` (RFC section 6.2). The module path during development is `github.com/HarshitBadhwar8/leadscore`; S19 rewrites it to `github.com/tetriz-ai/leadscore`.

```go
package leadscore

// Identifiers.
type LeadID string  // UUIDv7, minted when a person is first seen
type EventID string // an event's de-duplication key (key rules: section 12.7)
type Cursor string  // opaque progress marker; each source or store encodes its own

type StepKey struct {
    LeadID LeadID
    LaneID string // the lane's stable id: from the rubric
    Step   string // one of the sink's Steps(dest)
}

// Inputs.
type InputRow struct {
    SourceID string            // the source's stable id: from leadscore.yml
    Headers  []string          // headers in file order, as written
    Columns  map[string]string // every column as raw text, keyed by header as written
    // The engine, not the source, applies aliases and computes the per-row id (section 12.5).
}

type Event struct {
    ID          EventID           // empty from sources; the engine sets it (section 12.7)
    Kind        string            // section 5.3; empty with Attrs["reject"] set for a rejected source row
    Email       string            // person keys; both empty for a company-only event
    LinkedInURL string
    Domain      string            // the person's employer domain, or the company for a company-only event
    At          time.Time         // when it happened, UTC
    ReceivedAt  time.Time
    Origin      string            // "receiver", "polling", "hubspot", "apollo_lookup", or a source id
    Attrs       map[string]string // kind-specific: stage, label, message_id, page, deal_id, contact_id, full_name, title, company, reject
}

type RawEvent struct {
    // Seq is assigned by the store on append (empty when appending) and is a
    // complete resume cursor: reading from it returns exactly the events after
    // this one, across every partition the store keeps.
    Seq        Cursor
    Kind       string    // "apollo_visit" or "apollo_reply" (section 5.1)
    ReceivedAt time.Time
    Body       []byte    // the request body, secret already removed
}

type CompanyFacts struct {
    Domain       string
    Name         string
    Region       string            // the vendor's country, trimmed; no bucketing
    FundingStage string            // one of the values in section 6, or empty
    Employees    *int              // nil when unknown
    Extra        map[string]string // other facts; the Apollo enricher writes latest_funding_at
    FetchedAt    time.Time
    NotFound     bool              // vendor had no record; retried after max age
}

// What engines and adapters see of a lead.
type LeadRef struct {
    ID             LeadID
    Emails         []string // every email in Identities for the lead and every lead merged into it, primary first (section 4)
    LinkedInURLs   []string
    FullName       string
    Title          string
    Domain         string            // company domain; empty when the lead has none
    Status         string            // folded status (section 7)
    Fields         map[string]string // merged fields, by resolved name (section 2)
    FirstSeenAt    time.Time         // People.created_at
    SourcesSeen    int               // distinct channels (section 3, sources[].channel)
    ReceiverOnly   bool
    ConflictFields []string          // fields whose sources disagreed (People.conflicts)
    CompanyDealID  string            // the stored open or won deal at the lead's company, if any
    Verdict        *Verdict          // nil before scoring
}

type Verdict struct {
    RubricVersion string
    Values        map[string]any // every derived name (fit_signal, tier, priority, ...); numbers are float64; nil means "no value"
    AccountScore  float64        // 0 when the lead has no company; Reasons then includes "no company domain"
    ContactScore  float64
    Reasons       []string // rules fired, points added, lane checks failed, in order
}

type LedgerRef struct {
    Key      StepKey
    Dest     string
    VendorID string
    State    string // pending, done, failed, cancelled
}

// Plug-in interfaces.
type Source interface {
    ID() string
    // Fetch returns rows and events after cursor. Snapshot sources (CSV, Sheet
    // tabs) ignore the cursor and return everything every run.
    Fetch(ctx context.Context, cursor Cursor) (rows []InputRow, events []Event, next Cursor, err error)
}

type Enricher interface {
    // Enrich calls domains in the order given and stops only on a rate limit, when
    // it returns the facts so far and ErrRateLimited. A failure on one domain is
    // skipped. The caller counts calls made as the index of the last domain tried, plus one.
    Enrich(ctx context.Context, domains []string, budget int) ([]CompanyFacts, error)
}

// Poller reads outcomes on a schedule (Apollo reply polling), at run step 3.
// since is the start of the window to read; the poller uses it as given.
type Poller interface {
    Poll(ctx context.Context, since time.Time) ([]Event, error)
}

// Lookup checks leads just before pushing (HubSpot opt-out and deals, Apollo
// contact opt-out), at run step 8. It checks every email in LeadRef.Emails.
// failed names leads whose lookup failed; the engine blocks those leads. err
// means the whole lookup failed.
type Lookup interface {
    Lookup(ctx context.Context, leads []LeadRef) (events []Event, failed map[LeadID]error, err error)
}

type Sink interface {
    // Steps returns the ordered steps for one destination, for example
    // Steps("sequence/qualified") == {"contact", "enroll"}.
    Steps(dest string) []string
    // Do must be find-or-create by req.Key: calling it twice with the same key
    // leaves one vendor-side object.
    Do(ctx context.Context, req StepRequest) (vendorID string, err error)
}

type StepRequest struct {
    Key     StepKey
    Dest    string            // the part after "<sink>:" in the lane's push target
    Lead    LeadRef
    Prior   map[string]string // vendor ids from this push's earlier steps, by step name
    Related []LedgerRef       // this sink's done steps for other leads at the same company, any lane,
                              // including steps finished earlier in this batch; only deals at an
                              // open stage are included
}

// Classify with errors.Is. Any other error counts one attempt toward `failed`.
// None of these says the vendor did nothing; the ledger records whether the call
// went out (section 8).
var (
    ErrRateLimited = errors.New("rate limited") // stays pending, no attempt counted, stop this sink for the run
    ErrTransient   = errors.New("transient")    // stays pending, no attempt counted
    ErrRefused     = errors.New("refused")      // vendor said no for a reason retrying cannot change
)

// Subject is what a detector evaluates: a lead, or a company when Lead is nil.
type Subject struct {
    Lead   *LeadRef
    Domain string
}

type Detector interface {
    Name() string
    // events are the subject's Window events: a lead's own, or for a company every
    // event whose domain is the company, lead events included.
    Evaluate(s Subject, events []Event, now time.Time) (fired bool, evidence []EventID)
}

// Table-level storage. The codec maps the in-memory model to tables (section 4).
type Row = map[string]string

type WriteOp int

const (
    OpReplace WriteOp = iota // rewrite the whole table
    OpAppend                 // add rows
    OpUpsert                 // insert or update by Key columns
    OpDelete                 // delete rows matching Key columns
    OpTrim                   // delete rows whose Column is before Before
)

type TableWrite struct {
    Table  string    // the section 4 table name exactly
    Op     WriteOp
    Key    []string  // columns for OpUpsert and OpDelete; required for both
    Rows   []Row
    Column string    // OpTrim only
    Before time.Time // OpTrim only
}

var (
    ErrTooLarge     = errors.New("commit too large") // engine handling: section 12.6
    ErrLeaseHeld    = errors.New("lease held")
    ErrLeaseLost    = errors.New("lease lost")
    ErrEventsShrank = errors.New("event log shrank below a saved cursor") // engine scores but does not push
)

// RunLease is a held lease.
type RunLease interface {
    // Check returns ErrLeaseLost if the lease expired or another owner took it.
    Check(ctx context.Context) error
    // Release gives the lease up only if this owner still holds it.
    Release(ctx context.Context) error
}

type Backend interface {
    // ReadTable returns every row of a table; a missing table returns no rows and no error.
    ReadTable(ctx context.Context, name string) ([]Row, error)
    // Lease takes the run lease or returns ErrLeaseHeld. An expired lease is taken
    // over. It must be a real compare-and-swap: two callers can never both hold it.
    Lease(ctx context.Context, owner string, ttl time.Duration) (RunLease, error)
    // Commit applies every write all-or-nothing (one Sheets batchUpdate, one SQL
    // transaction). It rejects an OpUpsert or OpDelete with no Key before applying
    // anything. It creates a missing table, and appends a missing column, the first
    // time a write names it. It returns ErrTooLarge rather than splitting.
    Commit(ctx context.Context, writes []TableWrite) error
}

// LeaseInspector is optional; doctor uses it to show the lease without taking it.
type LeaseInspector interface {
    LeaseInfo(ctx context.Context) (owner string, expires time.Time, err error)
}

// The store's append-only log of raw receiver requests. storetest checks the
// ordering contract.
type EventLog interface {
    // AppendEvents stores a batch all-or-nothing, in order, and returns only once
    // it is durable. It retries the store's "slow down" answers until ctx is done.
    AppendEvents(ctx context.Context, events []RawEvent) error
    // ReadEvents returns events after cursor and the next cursor. An event not yet
    // returned is returned by a later read from the saved cursor; sequence numbers
    // are never reused, even after deletion. It returns ErrEventsShrank when a
    // partition named in the cursor is missing or holds fewer rows than the cursor
    // says were read.
    ReadEvents(ctx context.Context, cursor Cursor) ([]RawEvent, Cursor, error)
    // DeleteProcessed removes events at or below committed that are older than
    // olderThan, and returns committed with any deleted partition dropped from it;
    // the engine saves the returned cursor.
    DeleteProcessed(ctx context.Context, committed Cursor, olderThan time.Time) (Cursor, error)
}

type Config = map[string]any // an adapter's block from leadscore.yml (which block: section 3)

// Each Register* panics on an empty type, a nil factory, or a type already registered.
func RegisterSource(typ string, f func(Config) (Source, error))
func RegisterEnricher(typ string, f func(Config) (Enricher, error))
func RegisterPoller(typ string, f func(Config) (Poller, error))
func RegisterLookup(typ string, f func(Config) (Lookup, error))
func RegisterSink(typ string, f func(Config) (Sink, error))
func RegisterDetector(kind string, f func(params Config) (Detector, error))
func RegisterBackend(typ string, f func(Config) (Backend, EventLog, error))

type RunOptions struct {
    // ConfigPath is leadscore.yml, or a hosted bundle (section 3); with a bundle,
    // RubricPath is ignored. Empty means the defaults in section 3.
    ConfigPath, RubricPath string
    DryRun                 bool
    // Stop, when closed, stops new vendor calls; the run then saves within its
    // budget. Closing it is the graceful stop; cancelling ctx is the hard stop.
    Stop <-chan struct{}
}

type RunResult struct {
    Healthy  bool
    Skipped  bool     // another run held the lease
    Problems []string // the open problems written to Health
    Pushed   int
}

// Run executes one run, as `leadscore run` does, with the production hooks.
func Run(ctx context.Context, opts RunOptions) (RunResult, error)

// Main is the CLI entry point.
func Main()
```

**Conformance suites** (public packages, declared in S1 with skipped bodies and filled by S4 and S10b). They import `internal/api`, not the root, so the signatures name `api.X`; these are the same types as `leadscore.X` (the root aliases them), so a caller passes `leadscore` values unchanged:

```go
package storetest
type Table struct {
    Name         string   // section 4 name; for a pattern, the prefix ("Events ", "Export ")
    Columns      []string // fixed columns in order
    Pattern      bool     // Events YYYY-MM and Export <lane id>
    DynamicAfter string   // Ranked: derived-name columns go after this column
}
var Schema []Table // every section 4 tool table, in section 4 order
// Run checks a plug-in store: round-trip of every table, Commit all-or-nothing for
// every op (a failure injected as an OpUpsert with no Key placed last), batch
// append, ordering, a slow append interleaved with a read, crash between phases,
// added columns and unknown columns kept, many callers racing Lease with exactly
// one winner (also on an expired lease), release by a non-owner refused, OpTrim,
// DeleteProcessed dropping a partition, and ErrEventsShrank (a cursor saved from
// one store read against a fresh store).
func Run(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog))

package sinktest
type FailKind int
const (
    RateLimited FailKind = iota
    Transient
    Refused
    Other
)
type Vendor interface {
    Count(step string) int           // vendor-side objects created for a step
    Fail(step string, kind FailKind) // make the next call to that step fail this way
}
type Harness struct {
    New    func(cfg api.Config) (api.Sink, error)
    Vendor Vendor // a fake the sink is pointed at
    Dests  []string
}
// Run replays every step after a simulated crash and asserts one vendor-side
// object, calls Do twice with one key, and checks each FailKind maps to its error.
func Run(t *testing.T, h Harness)
```

**Evolution rule:** these interfaces never gain methods after `v0.1.0`. A new capability is a separate optional interface found by type assertion, so an adapter keeps compiling.

## 2. Rubric grammar

A rubric is one YAML file. Top-level keys, all optional except `version` and `lanes`: `version` (1), `fields`, `settings`, `company`, `detectors`, `derive`, `conflicts`, `score`, `limits`, `lanes`. RFC section 6.4 has an abridged example and the built-in fields.

**Aliases and headers.** The engine resolves input headers to field names before merge, once, for every source. A header is matched after squashing it to lowercase `a-z0-9` (so `Work Email` and `work_email` both become `workemail`). When two headers of one row resolve to the same field, the first in file order wins. The built-in alias table (in `internal/api`, from core plus three company facts):

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
| `at` (event rows) | visitedat, visitdate, lastvisited, lastvisitat |

Core's `data_quality_note`, `primary_ai_coding_tool` and `visited_domain` aliases do not ship. A declared field also matches its own squashed name and its `aliases`; a field's `aliases` win on a clash with the built-in table. Any other header is kept under its squashed name, and rules refer to it by that name (a header `Primary AI coding tool` is `primaryaicodingtool`).

**Fields.** `fields: { <name>: { type, level, aliases } }`. `type` is `text`, `number`, `date` (ISO 8601), `bool` (`true/false/yes/no/1/0`), or `{ ordered: <settings list> }`. `level` is `lead` (default) or `company`. A non-built-in `company` field is read from the `Companies` tab (and enrichment `Extra`); to lift a lead column to the company, use a rollup. Undeclared columns are `text` at lead level. A value that does not parse as its type is absent. A derived name shadows an input column of the same name, and the compiler warns.

**Settings.** `settings: { <name>: <list or scalar> }`. A list is an ordering for `ordered` fields; unknown values sort below all. Conditions refer to a setting as `$<name>`.

**Text matching.** Every text comparison (`eq`, `ne`, `in`, `not_in`, `contains`, and matching a value to an `ordered` list) is case-insensitive after trimming. Matching to an `ordered` list also ignores spaces, hyphens and underscores, so `Series B` matches `series_b`.

**Conditions.** A condition is one of:

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

Any comparison on an absent value is false, so only `missing` is true for it. `F` is a built-in field, a declared or input column, `company.<name>` for a company field or rollup, or a derived name.

**CEL variables** (for `expr:`):

| Variable | CEL type | Holds |
|---|---|---|
| `lead` | `map(string, dyn)` | lead fields and lead-level derived names, typed per `fields`; absent keys are missing |
| `company` | `map(string, dyn)` | company fields, rollups and company-level derived names |
| `detector` | `map(string, bool)` | every detector, true when fired for the lead or its company |
| `status` | `string` | the folded status |
| `settings` | `map(string, dyn)` | the `settings` block |

Test presence with `has(lead.x)`.

**Company rollups.** `company: { <name>: <rollup> }`, computed over a company's leads: `{ any: C }` and `{ all: C }` (bool), `{ count: C }` (number), `{ max: F }` and `{ min: F }` (number or date), `{ first: F }` (the first present value, taking leads oldest first by first seen, as core's "first non-empty wins"). Read as `company.<name>`.

**Detectors.** `detectors: { <name>: { kind, subject, ... } }`, `subject` is `lead` (default) or `company`. Every window is at most 90 days. A company-subject detector sees every event whose domain is the company, lead events included.

| Kind | Parameters | Fires when |
|---|---|---|
| `count_in_window` | `event`, `window`, `min` | at least `min` events of that kind in the window |
| `first_seen` | `event`, `within` | the first event of that kind ever was within `within` |
| `change` | `field` (a company fact), `within`, optional `from`, `to` | the fact changed within `within` (by `facts.<f>.at`), matching `from` and `to` when given |
| a registered kind | its own parameters under `params:` | as its `Detector` decides |

`event` may end in `*` to match a prefix, for example `visit_*`.

**Derive.** `derive: { <name>: <block> }`, evaluated in file order. A block is a list of rules (lead level) or `{ level, rules }`. A rule is `{ when: C, then: V }`; the last may be `{ else: V }`. The first matching rule wins. `then: null`, or no rule matching and no `else`, gives "no value": later comparisons on it are false and `missing` is true. A company block may read company fields and rollups, company-subject detectors, and earlier company blocks; a lead block may read anything above it. The name `fit_signal` is the verdict's fit signal when a rubric defines it; `tier` and `priority`, when defined, are the names whose changes are logged.

**Conflicts.** `conflicts: [ { field: F } ]`. A lead whose sources gave different non-empty values for F (recorded by merge in `People.conflicts`) is blocked on every lane, with the reason.

**Score.** `score: { account: [ <rule> ], contact: [ <rule> ] }`. A rule is `{ when: C, points: N }`, or a band rule `{ band: F, points: { <threshold>: N, ... } }`, which pays the points of the highest threshold at or below F's number (as core pays for sources or colleagues seen: a fourth adds nothing rather than falling to zero). Account rules may read only company-level values. A lead with no company gets account half 0. The score is the sum of both halves.

**Limits.** `limits: { max_pushes_per_run, max_pushes_per_day, timezone }`, defaults 100, 200, UTC.

**Lanes.** `lanes: [ { id, name, kind, priority, when, push } ]`. `id` is required and must never change; `kind` is `cold`, `non-cold` or `export`; a higher `priority` wins; `push` is `<sink>:<destination>`: `apollo:sequence/<sequence name>`, `hubspot:contacts`, `hubspot:deals`, or `export:<anything>` (export lanes only, and only export lanes use `export:`). At load the compiler checks this syntax; that the sink is registered is checked at run start.

**Version hash.** `r-` plus the first 16 hex characters of the SHA-256 of the YAML re-marshalled with sorted keys and no comments.

## 3. `leadscore.yml`

Unknown engine keys fail loading, naming the key; adapter blocks are passed through as `Config`. Engine keys (including a source's `id`, `type`, `channel`, `path` and `tabs`) are read as text exactly as written, so `spreadsheet: 0123` and `tabs: [2024]` are `"0123"` and `"2024"`. Any other key in an adapter block reaches the adapter with YAML's types (`0123` arrives as a number), so quote ids there. Relative paths are relative to the folder holding `leadscore.yml`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | number | required | `1` |
| `rubric` | path | `rubric.yml` beside `leadscore.yml` | the rubric file |
| `store.type` | `sqlite`, `sheets`, or a plug-in | required | which store |
| `store.path` | path | `/data/leadscore.db` | SQLite file (on the named volume) |
| `store.spreadsheet`, `store.lease_bucket` | text | — | Sheets: the spreadsheet id and the Cloud Storage bucket for the lease |
| `store.view_spreadsheet` | text | — | SQLite only: the optional read-only Sheet view |
| `store.credentials` | path | Google's standard loading | Docker with Sheets: the service-account key file |
| `sources[]` | list | — | each `{ id, type, channel, path or tabs, events, apollo_held, match_domain_name }`; `id` required; `channel` defaults to the id and is what `sources_seen` counts (two conference CSVs with `channel: conference` count once, as core counts channels); the flags default false. The engine copies `store.spreadsheet` (or `store.view_spreadsheet` on SQLite) and `store.credentials` into a `sheetsource` entry as `spreadsheet` and `credentials` |
| `enrich` | object | — | `{ type, max_age: 30d, max_lookups_per_run: 100, max_lookups_per_day: 400 }` |
| `replies` | `receiver` or `polling` | `receiver` | where replies come from; the laptop example sets `polling` |
| `polling.sequence_length` | duration | `30d` | the longest sequence the team runs |
| `polling.window_margin` | duration | `7d` | the poll window is `sequence_length` plus this |
| `receiver.public_url` | URL | — | where Apollo posts; `doctor` probes its `/healthz` |
| `receiver.visit_events` | list | `[]` | the `visit_<name>` kinds the workflows send, for silence detection |
| (polling window) | — | — | a poll reads from the earlier of now − (`sequence_length` + `window_margin`) and `last_poll_at` − `window_margin`, so an outage longer than the window loses nothing |
| `receiver.port` | number | `$PORT`, else `8080` | listen port |
| `sinks.apollo.mailbox_id` | text | — | sending mailbox for enrollment |
| `sinks.hubspot` | object | — | `{ pipeline, stage, property_prefix: leadscore_ }` |
| `export.dir` | path | `/out` | SQLite: where export CSVs go (a bind-mounted `./out` on Docker) |
| `reply_labels` | map | section 5.5 | per-label overrides: `{ <label>: <status or none> }`, with `_unlabelled` for no label; allowed values are the `replied_*` statuses and `none`. The `unsubscribe` label cannot be overridden |
| `pushes_enabled` | bool | `false` | cold and non-cold lanes push only when true |
| `schedule` | duration | `15m` | Docker: the `serve --every` interval (read when `serve` starts); Google Cloud: given to Cloud Scheduler, and must be a whole number of minutes dividing 60 (`*/N * * * *`) or of hours dividing 24 (`0 */N * * *`; `24h` is `0 0 * * *`) |
| `deadline` | duration | `12m` | run deadline |
| `ingest_chunk_rows` | number | `2000` | input rows processed per run, shared across sources (events are not chunked) |
| `silence_threshold` | duration | `3d` | receiver silence before `Health` flags it |
| `log_retention` | duration | `90d` | how long `Log` rows are kept |
| `hosting` | object | — | Google Cloud only, written by setup: `{ project, region, run_account, receiver_account, image }`. Resource names are fixed: service `leadscore-receiver`, job `leadscore-run`, scheduler job `leadscore-schedule`, Artifact Registry remote repository `ghcr-proxy`, secrets `leadscore-config`, `apollo-api-key`, `hubspot-token`, `receiver-secret`, `receiver-secret-previous` |

**Which block each adapter gets.** A source gets its `sources[]` entry (with `id` and `type`); the enricher gets `enrich`; the store gets `store`; a sink of type T gets `sinks.T`. A `Lookup` and a `Poller` of type T are built from `sinks.T` when that block exists (the Poller only with `replies: polling`). For tests, `RunWith` (section 12.6) adds two unexported keys to every vendor and store block: `base_url` and `_http_client`; every factory honours them, and Google clients skip authentication when `base_url` is set.

**Default paths.** Without flags, commands read `/config/leadscore.yml` if it exists, else `./leadscore.yml`; `--config` and `--rubric` override. When `/config/bundle.yaml` exists, it is used instead. In Docker, `compose.yaml` mounts the team's folder at `/config`.

**Hosted bundle.** `leadscore config push` uploads one Secret Manager secret, `leadscore-config`, whose value is a YAML document with two keys, `config` and `rubric`, each holding that file's text. It is mounted into the job and the service at `/config/bundle.yaml`. At load, a hosted run reads the number of the secret version it was given and writes it to `State.config_version`.

**Keys on Google Cloud.** A local command with `hosting.project` set and an empty key variable reads the key from Secret Manager, as the run account, only when it builds an adapter that needs the key, and never inside Cloud Run (`K_SERVICE` or `CLOUD_RUN_JOB` set).

**Config helpers.** `leadscore config get <key>` prints a value and `leadscore config set-hosting <key>=<value>...` writes the `hosting` block, keeping comments; `setup/gcp.sh` uses them instead of editing YAML.

## 4. Store tables

Every tool table is created with exactly these columns, in this order (`storetest.Schema`). Every write keeps columns the writing version does not know, and no version ever lowers `schema_version` (which starts at `1.0`). On Sheets each tab is created at that width with a frozen header row, because Sheets counts every grid cell toward its cap. JSON columns hold one JSON object as text. `TableWrite.Table` is the name below exactly; SQLite maps it to lower snake_case.

**Sheets values.** Tool tabs are written with `RAW` and read with `UNFORMATTED_VALUE`, so a value that starts with `=` stays text. People-owned tabs (`Leads`, `Companies`, `Overrides`, source tabs) are read with `FORMATTED_VALUE`. Only `Health!H1` is written as a formula (`USER_ENTERED`).

| Table | Key | Columns |
|---|---|---|
| `Leads` (or one tab per source), `Companies` | — | People-owned. Any columns. `Companies` needs `domain`; other columns are facts |
| `Overrides` | — | People-owned: `person` (email, LinkedIn URL or lead id; `*` only on a `retry` row, meaning every lead), `action` (`status`, `same_as`, `distinct`, `retry`), `value` (a status or `resubscribe`; the other person; the lane id for `retry`, or empty for every lane), `note`. The model holds it as an ordered list and removes a row by `OpDelete` keyed on all four columns |
| `Applied overrides` | `row_hash` | `row_hash`, `applied_at`, `run_id`. `retry` and `resubscribe` rows apply once; the CLI writes the request time in `note`, so each request is a new row |
| `People` | `lead_id` | `lead_id`, `created_at`, `apollo_held_at` (never cleared), `merged_into` (the surviving lead id after a `same_as` merge; merges are permanent), `first_seen` (JSON: event kind to time), `fields` (JSON: resolved field name to {value, source_id, at}, plus `derived: true` on a company domain derived from a work email), `conflicts` (JSON: field to the list of {value, source_id} that disagreed with the kept value) |
| `Identities` | `key` | `key` (lowercased email or canonical LinkedIn URL), `kind` (`email` or `linkedin`), `lead_id`, `source_id`, `first_seen_at` |
| `Company facts` | `domain` | `domain`, `facts` (JSON: fact to {value, origin, at}, the resolved winner), `previous` (JSON, same shape: the value a change replaced), `rollups` (JSON), `first_seen` (JSON: event kind to time, for every event whose domain is the company), `enriched_at`, `not_found_at` |
| `Events YYYY-MM` (Sheets) / `Events` (SQLite) | `seq` | `seq`, `received_at`, `kind`, `body` |
| `Window events` | `event_key` | `event_key`, `subject` (`lead` or `company`), `lead_id`, `domain`, `kind`, `at`, `attrs` (JSON) |
| `Applied rows` | `source_id`, `row_id` | `source_id`, `row_id`, `row_hash`, `lead_id` (empty for a rejected row), `first_applied_at` |
| `Seen events` | `event_key` | `event_key`, `first_received_at`, `run_id` |
| `Outcomes` | `lead_id` | `lead_id`, `status`, `status_at`, `unsubscribed_at`, `unsubscribed_origin` (`event`, `lookup`, `manual`), `reply_status`, `reply_at`, `contacted_at`, `deal_id`, `deal_stage`, `deal_checked_at` |
| `Ranked` | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, then one column per derived name, then `account_score`, `contact_score`, `score`, `status`, `lane`, `reasons`, `rubric_version` |
| `Pushes` | `lead_id`, `lane_id`, `step` | `lead_id`, `lane_id`, `step`, `lane_kind`, `dest`, `state`, `vendor_id`, `attempts`, `called_at`, `intent_run`, `last_error`, `first_started_at`, `updated_at`. Cold and non-cold lanes only |
| `Log` | — | `at`, `run_id`, `level`, `lead_id`, `email`, `kind`, `message`, `rubric_version`. Only appended and trimmed; runs do not load it. Kinds include `tier_change`, `priority_change`, `row_rejected`, `key_conflict`, `push_failed` |
| `Health` | `kind`, `key` | `kind` (`result` or `problem`), `key`, `value`, `first_seen_at`, `updated_at` |
| `State` | `key` | `key`, `value` (keys below) |
| `Export <lane id>`, one per export lane (CSV: `<export.dir>/<lane id>.csv`) | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, `score`, `reasons`, `first_listed_at`, `status`, `do_not_contact`, `updated_at` |

**Primary email.** The lead's newest email identity from a same-source correction, else its email identity with the earliest `first_seen_at`. **Company domain.** Stored in `People.fields["company.domain"]`; the people-by-domain index reads it.

**Company fact origins**, highest first: `companies_tab`, `enrichment`, `input`. `facts` holds the winner. A source never replaces a fact from a higher origin. A refresh with an unchanged value leaves `facts.<f>` and `previous.<f>` alone (only `enriched_at` moves); a changed value moves the old entry to `previous` and sets `at` to the fetch time.

**`Health` rows.** Results: `last_result` (`healthy` or `unhealthy`), `last_run_at`, `last_success_at`, `run_id`, `rubric_version`, `schedule`. Problems: key `<kind>:<id>`, exactly as the check or hook gives it, for example `push_failed:<lead>:<lane>:<step>`, `namesake:<lead>`, `secret_missing:<variable>`, `status_conflict:<lead>`, `override_unmatched:<row>`, `receiver_only_push:<lead>`, `silent:<kind>`, `skipped_runs`, `ledger_shrank`, `view_write_failed`. Each run rewrites the problem rows: it keeps `first_seen_at` for a problem still open and deletes resolved ones. On Sheets, cell `H1` of the `Health` tab (outside the table, the one exception to exact width) holds the staleness formula, rewritten each run: `=IF(NOW()-DATEVALUE(LEFT(<last_success_at cell>,10))-TIMEVALUE(MID(<last_success_at cell>,12,8))>3*<schedule in days>,"STALE: no successful run in 3 intervals","ok")`.

**`State` keys.** `schema_version` (`major.minor`), `config_version`, `cursor:<source id>`, `cursor:events`, `last_poll_at`, `first_run_at`, `last_received:<kind>` (received time of the newest event of each kind), `enrich_count:<YYYY-MM-DD>` (UTC), `ledger_rows` (the highest committed ledger row count; never lowered by a run), `key_conflicts` (running count), `opened_by` (the hostname of the `serve` process that last opened a SQLite file), and on SQLite `lease_owner` and `lease_expires_at`. The `store` check (SQLite) fails when the current process is not in a container (no `/.dockerenv`) while `opened_by` names one.

**Sheets lease file.** `gs://<lease_bucket>/leadscore-lease.json`: `{"owner": "<run id>", "expires_at": "<time>"}`. Taking it writes with `ifGenerationMatch` (0 when absent); release deletes it with `ifGenerationMatch`. A Sheets commit whose encoded body is over 9MB, or that the API rejects as too large, is `ErrTooLarge`.

**Export rows.**

- On Sheets, every commit that writes `Health` also rewrites the `Health!H1` formula.
- A lead is added once, the first run it matches the lane. Every run then recomputes `status` and `do_not_contact` for every row in every `Export *` table in the store (including tables of lanes since removed), following `merged_into` to the live lead, and upserts only rows whose values changed (`updated_at` changes only then).
- `do_not_contact` is `yes` when the live lead is blocked on every lane for any reason (unsubscribed, unresolved duplicate, rubric conflict, Overrides status conflict, unknown Overrides value); has any status that blocks cold lanes; is at a company with an open or won deal; has already been contacted (any cold row that holds the cold push under section 8, across the `merged_into` chain, or `contacted_at` set); matches a cold lane or has an open cold row this run; or when its row's lead was merged into a lead that also has a row in the same table. Otherwise `no`.
- Export lanes write no `Pushes` rows; the table is their once-only record. HubSpot and Apollo opt-out lookups run only for push candidates, so an export row reflects opt-outs from the receiver, polling and Overrides (the README says so).
- A CSV is rewritten only after phase 2 committed, only from the committed table, and never on dry-run: written to a temporary file in `export.dir` and renamed, UTF-8, header row in column order.

**Retention.** `Window events` keeps 90 days, `Seen events` one year, `Log` `log_retention`. Each run trims them with `OpTrim` in its phase 2 commit. Statuses never depend on retained events: `Outcomes` keeps the unsubscribe, reply, contact and deal facts for good.

**Sheets cell budget** at the 20,000-lead target, assuming 1,000 receiver events a day:

| Tab | Rows | Width | Cells |
|---|---|---|---|
| `People`, `Outcomes` | 20,000 each | 7, 11 | 360,000 |
| `Identities`, `Applied rows` | about 40,000 each | 5, 5 | 400,000 |
| `Ranked` | 20,000 | about 20 | 400,000 |
| `Pushes` | about 40,000 | 13 | 520,000 |
| `Seen events` | 365,000 | 3 | 1,095,000 |
| `Window events` | 90,000 | 7 | 630,000 |
| `Log` | about 180,000 | 8 | 1,440,000 |
| `Events` monthly tabs | about 120,000 live (four months) | 4 | 480,000 |
| `Leads` and other input tabs | about 20,000 | about 20 | 400,000 |
| `Companies`, `Company facts` | about 5,000 each | about 10, 7 | 85,000 |
| Export tab | up to 20,000 | 11 | 220,000 |
| **Total** | | | **about 6.0 million of 10 million** |

`doctor` warns at 70% of the cap and names the largest tabs. Above about 1,500 events a day, the README points teams to SQLite or a shorter `log_retention`.

## 5. Receiver interface

### 5.1 HTTP

| Route | Purpose | Answers |
|---|---|---|
| `POST /apollo/visit` | Apollo website-visit workflow | 2xx once stored; 401 wrong or unset secret; 5xx past the 10-second hold cap |
| `POST /apollo/reply` | Apollo reply, sent and unsubscribe workflow | same |
| `GET /healthz` | Health check | With a timer (`--every`): 200 when the last run succeeded or none is due yet, 503 when the last run failed or none succeeded in three intervals (with no `last_success_at`, measured from `serve`'s start); read from the store and cached for 60 seconds. Without a timer (Cloud Run service): 200 unless the last `AppendEvents` failed |

- **Secret.** It travels in the `X-Leadscore-Secret` header, or in a top-level `leadscore_secret` body field when Apollo cannot send headers. It is compared in constant time against `LEADSCORE_RECEIVER_SECRET` and, when set, `LEADSCORE_RECEIVER_SECRET_PREVIOUS`, and removed from the body before storing. The receiver counts as configured when `replies: receiver` or `receiver.visit_events` is non-empty. If its secret variable is empty, every POST gets 401 and `serve` logs a warning but still starts, so `/healthz` and the timer work. The `receiver-secret` check runs at `serve` start and in `doctor`, not inside runs.
- **Write queue.** Requests are gathered for 2 seconds and appended in one `AppendEvents` call whose context ends 10 seconds after the oldest request in the batch arrived; when it ends, every request in the batch gets 5xx.
- **Timer.** The first run starts when `serve` starts; the next starts `schedule` after the previous one ends. A run that panics is recovered; `serve` then writes `last_result=unhealthy` and `last_run_at` to `Health` itself.
- **Shutdown.** On SIGTERM `serve` stops its timer, closes the running run's `Stop` channel (the run then saves within its budget), keeps the receiver storing events until the run returns, drains the write queue, and exits.
- **Image.** The container runs as the non-root user `leadscore` with `HOME=/home/leadscore`. `leadscore healthz` calls the local `/healthz` and exits 0 on 200 (the compose health check).

**Body formats**, set by the workflow templates (`setup/apollo/`). Event names use core's names, so the copied parser reads them: `email_sent` is `sent`, `email_replied` is `replied`, `email_replied_positive` is `replied_positive`, `email_unsubscribed` is `unsubscribed`, and `website_visited_<name>` is `visit_<name>`. A visit body uses core's nested `contact.*` and `account.*` fields. A reply-workflow body carries core's flat fields (`contact_email`, `contact_stage`, `last_conversation_link`) plus `contact_id`, `contact_name`, `contact_title`, `contact_linkedin_url` and `account_domain`; S0 confirms which of these Apollo's workflow variables can fill, and any it cannot are left out of the template. S0 saves a real body of each kind in `testdata/events/`.

**Receiver rows.** Each identified request also yields an input row under source id and channel `receiver`, with columns `contact_id`, `email`, `linkedin_url`, `full_name`, `title`, `company.name`, `company.domain` (whatever the body carries); its row id is the contact id, else the email, else the LinkedIn URL. So `receiver` counts in `sources_seen` for every lead it touches, as core upserts the contact.

**Keep the secret private.** The README warns, in the setup steps and next to the Apollo workflow templates, that the receiver secret works like a password: anyone who has it can send fake events, including a fake positive reply that a deal lane would act on. It must never be pasted into chat, tickets or shared docs, and should be rotated if it might have leaked.

**Rotating the secret.** Move the current secret to `LEADSCORE_RECEIVER_SECRET_PREVIOUS`, set a new `LEADSCORE_RECEIVER_SECRET`, update each Apollo workflow, then remove the previous one. On Docker, edit `.env` and `docker compose up -d`. On Google Cloud, add versions to `receiver-secret` and `receiver-secret-previous` and run `setup/gcp.sh redeploy`, which attaches the previous secret only while it has an enabled version. `doctor` warns while a previous secret is still set. Rotating the Apollo or HubSpot keys works the same way.

### 5.2 Event rows from a CSV or Sheet source

A source marked `events: true` carries one event per row: `event` (the kind), `at` (RFC 3339, or `YYYY-MM-DD` read as UTC midnight; the `at` aliases apply), one of `email`, `linkedin_url` or `domain`, and any extra columns as `Attrs`. A row may carry only `visit_*` or a custom kind. A row with a forbidden kind (`sent`, `replied*`, `unsubscribed`, `reply`, `optout`, `deal_*`), no parseable `at`, or no key is returned with `Kind` empty and `Attrs["reject"]` set to the reason; the engine logs it as `row_rejected`. `Domain` is filled only from a `domain` column. An Apollo visitor export with no `event` column is read as `visit_<source id>`.

### 5.3 Event kinds

| Kind | From | Effect (applied by `events.Apply`, section 12.7) |
|---|---|---|
| `visit_<name>` | receiver, CSV visits | detector input; an identified visit creates the lead if unknown, as core does |
| `sent` | receiver | sets `contacted_at`; sets `apollo_held_at` |
| `replied` | receiver | `reply_status` `replied_neutral`; sets `apollo_held_at` |
| `replied_positive` | receiver | `reply_status` `replied_positive`; sets `apollo_held_at` |
| `unsubscribed` | receiver | sets `unsubscribed_at` (kept at its earliest) and `unsubscribed_origin` to `event`; sets `apollo_held_at` |
| `reply` | polling, with `label` and `message_id` in `Attrs` | per the reply-label map (section 5.5); sets `apollo_held_at` |
| `optout` | HubSpot or Apollo lookup | sets `unsubscribed_at` (earliest) and `unsubscribed_origin` to `lookup` |
| `deal_open`, `deal_won`, `deal_lost` | HubSpot lookup, with `deal_id` and `stage` in `Attrs` | sets `deal_id`, `deal_stage`, `deal_checked_at` on every live lead at the company and every lead whose `deal_id` equals it |

- **Origins.** An automated opt-out always sets `unsubscribed_origin` to `event` or `lookup`, even over `manual`.
- **Several deals.** When a company has several deals, the lookup emits one event for the strongest stage (won, then open, then lost), naming that deal. A deleted stored deal (404) is `deal_lost`.
- **Polling mode.** With `replies: polling`, receiver `replied` and `replied_positive` events are keyed in `Seen events` but have no effect and no window row (`sent` and `unsubscribed` still apply), so one reply never counts twice.
- **Lookup events** are not de-duplicated through `Seen events`; applying them is idempotent.
- **Silence detection** expects the `receiver.visit_events` kinds, and `sent` when `replies: receiver`; silence is measured from the later of `State.last_received:<kind>` and `State.first_run_at`, against `silence_threshold`.

### 5.4 Oversized bodies

The receiver decodes the JSON and shortens every string over 16KB with a `__truncated_fields` marker (core's caps). If the body is still over the 50,000-character Sheets cell limit, it keeps only the fields the parser needs, at their original paths (event, email, LinkedIn URL, stage, conversation link, `visited_at`, contact id, `account.domain`, `account.website_url`; `apollo.RequiredPaths` lists them), and adds `"__dropped_reason"`. A body that is not JSON is stored as `{"__not_json": true, "raw": "<first 16KB>"}`.

### 5.5 Reply-label map (polling)

| Apollo label | Status |
|---|---|
| `willing_to_meet` | `replied_positive` |
| `unsubscribe` | `unsubscribed` (fixed) |
| `not_interested` | `replied_negative` |
| `follow_up_question`, `person_referral`, `already_left_company_or_not_right_person`, `none_of_the_above` | `replied_neutral` |
| `out_of_office` | no outcome |
| no label | `replied_unlabelled` |

Teams override every row except `unsubscribe` with `reply_labels`. Two polled replies at the same received time are ordered by message id, then label.

## 6. Vendor fields

**Apollo.**

- **Client.** `adapters/apollo/client.go` has two call modes: a retrying call (Retry-After, used by enrichment) and a single-shot call that maps 429 to `ErrRateLimited` with no sleep (used by sinks, lookups and the poller).
- **Funding stage.** The enricher writes one of `pre_seed`, `seed`, `series_a`, `series_b`, `series_c`, `series_d_plus`, or leaves it empty for other labels. Region is the vendor's country, trimmed.
- **Checks.** The `apollo-key` check uses Apollo's free auth-health call (S0 confirms it), never an enrichment call. The `apollo-sequences` check confirms `mailbox_id` is in the email-accounts list and every lane's sequence name resolves.
- **Sequences.** `apollo:sequence/<name>` names a sequence; the sink resolves names to ids once per run. An unresolved name makes `Do` return `ErrTransient`, so the step waits until the name is fixed.
- **Refusals.** `ErrRefused`: contact active in another sequence, opted out, or invalid email.
- **Budgets.** Every enrichment call made counts toward both budgets. A domain is looked up when `enriched_at` and `not_found_at` are both empty or older than `max_age`.

**HubSpot properties** created by `leadscore setup hubspot`. The `leadscore_` prefix is configurable; properties go in a group named `leadscore`.

| Object | Property | Type | Use |
|---|---|---|---|
| Contact | `leadscore_lead_id` | text, unique | the contact sink searches by this first, then by email, so a crash followed by an email correction never creates a second contact |
| Contact | `leadscore_lane`, `leadscore_tier`, `leadscore_priority`, `leadscore_reasons` | text | context for salespeople; set on create only |
| Contact | `leadscore_score` | number | context for salespeople; set on create only |
| Deal | `leadscore_company_domain` | text, not unique (one open deal per company is enforced by the ledger and `Related`) | finding the company's deal |
| Deal | `leadscore_lane` | text | which lane opened it |

- **Destinations.** `hubspot:contacts` has steps `{contact}`; `hubspot:deals` has `{contact, deal}`, with the association made inside the deal step. Deals are named `<company domain>`. Only deals at an open stage are reused; a won or lost deal is never reused.
- **Errors.** 429 is `ErrRateLimited`; 5xx and timeouts are `ErrTransient`; 400 invalid email is `ErrRefused`; others count an attempt. On 409, the existing contact id is parsed from the message, else found by email search.
- **Deal stages.** Open, won or lost comes from the stage's closed and probability metadata in the pipeline.
- **Opt-out reads.** HubSpot: the contact's `hs_email_optout`. Apollo: the opt-out flag on the contact record found by email. S0 confirms both names, and that the Apollo lookup costs no credits; if Apollo has no such flag, the Apollo `Lookup` is not registered and the `apollo-key` check warns Apollo-only teams.

## 7. Status precedence

Statuses are recomputed each run from `Outcomes` and `Overrides` by taking the first rule that applies. Every input is saved in `Outcomes` when first learned, so no status depends on events that are later trimmed. The fold reads `Outcomes` across the lead and every lead merged into it: the earliest `unsubscribed_at`, the latest reply, any `contacted_at`, and any deal.

1. **Unsubscribed.** `unsubscribed_at` is set: by any `unsubscribed` or `optout` event, a polled `unsubscribe` label, or a manual `unsubscribed` row. Automation never clears it. The fold sets origin `manual` only when `unsubscribed_at` is empty. A `resubscribe` row clears it only while the origin is `manual`, applies once, and is logged.
2. **Blocked.** A lead with two or more different status rows in Overrides, under any of its identity keys, or with an unknown Overrides value, has status `blocked`: blocked on every lane and shown in `Health` until fixed.
3. **Manual.** The lead's one status row in Overrides wins. Removing the row releases it. An override for a person not yet known waits until that person appears; one still unmatched after a run shows as `override_unmatched` in `Health`.
4. **Deal.** Any lead at the company has a deal at an open or won stage, or a `hubspot:deals` step at the company has `called_at` or `intent_run` set (the deal may exist even if the call timed out), until a lookup shows the company has no open or won deal. A closed-lost deal releases it.
5. **Reply.** `reply_status`: the latest automated reply by received time.
6. **Contacted.** `contacted_at` is set (a `sent` event or a completed cold push).
7. Otherwise `new`.

**Matching Overrides.** `person` is normalized like Identities (emails lowercased and trimmed; LinkedIn URLs canonicalized) before matching.

**The CLI writes Overrides the same way on both stores.** The row's `person` is the lead's primary email, else LinkedIn URL, else lead id. `set-status <person> <status>` replaces the lead's status rows under every one of its identity keys with one row; `set-status <person> none` deletes them; `set-status <person> resubscribe` deletes them, including a manual `unsubscribed`, and writes a `resubscribe` row with the request time in `note`. A `resubscribe` row is not a status row for rules 2 and 3. `merge` and `mark-distinct` append a `same_as` or `distinct` row, and `retry` appends a `retry` row.

**A `same_as` merge.** The survivor is the lead with the older `created_at` (ties: lower lead id); the other gets `merged_into`. The survivor's `fields` are filled from the absorbed lead by fill-if-empty, with disagreements added to `conflicts`; `apollo_held_at` and `first_seen` take the earliest values; the absorbed lead's identities, `Outcomes` and ledger rows stay under its id and count for the survivor (the fold above, and section 8).

**Worked example** (an install with `replies: receiver`, a HubSpot deals lane, and the Apollo lookup). Priya gets an Apollo `sent` event from the receiver and becomes `contacted` (rule 6). Her `replied_positive` event arrives and she becomes `replied_positive` (rule 5). The warm-handoff lane opens a deal at her company, so she becomes `deal` (rule 4). A later `replied` event updates `reply_status` but leaves her at `deal`. She then clicks an unsubscribe link without replying; the pre-push Apollo lookup returns `optout`, so she becomes `unsubscribed` (rule 1) for good.

## 8. Ledger states

Each `Pushes` row is one step for one lead in one cold or non-cold lane. Two columns record whether the vendor may have acted: `called_at` is set by the post-batch write for every step actually called and is never cleared; `intent_run` is set by the pre-batch write for every step about to be called and cleared by the post-batch write. When a run loads the ledger, it first sets `called_at` to the load time and clears `intent_run` on any row whose `intent_run` was left by another run, saved in phase 1, so the proof of a possible call can never be lost. `first_started_at` is set in the pre-batch write the first time any step of the push gets an `intent_run`.

| State | Reached when | Next | Holds the cold push? |
|---|---|---|---|
| `pending`, not called | the lead is first selected for the lane, or a call was skipped | called next run if not cancelled | only while `intent_run` is set |
| `pending`, called | the call returned `ErrRateLimited` or `ErrTransient`, or another error below 3 attempts | called again next run if not cancelled | yes |
| `done` | the vendor returned an id | final | yes |
| `failed` | three counted attempts failed | a `retry` row resets it to `pending` with `attempts` 0 | yes |
| `cancelled` | a cancelling check failed before its next call, the vendor refused (`ErrRefused`), or the lane was removed from the rubric | a row with neither `called_at` nor `intent_run` returns to `pending` if the lead is selected for that lane again; otherwise final | yes if `called_at` or `intent_run` is set |

**What cancels and what only waits.** A pending step is **cancelled** when the lead fails a built-in lane check, a cold-lane status or deal check, the Apollo-held rule on an Apollo sequence lane, or the lane's `when` no longer matches. It **stays pending and waits** when a lookup failed, the re-read before its batch failed, pushes are disabled, pushing is blocked for the run (`NoPush`, section 12.6), the run or day limit is reached, the deadline passes, or its sink stopped on a rate limit.

**Merged leads are never called.** No step is called for a lead with `merged_into` set; at load, its `pending` rows with neither `called_at` nor `intent_run` are cancelled. The open-row rule and the check just before each push count every cold row across the `merged_into` chain.

**The ledger is never trusted when it shrank.** If the loaded ledger has fewer rows than `State.ledger_rows`, the run blocks all cold and non-cold pushing (`NoPush`), raises `ledger_shrank`, and keeps `ledger_rows` until the rows are restored.

**Selection and budget.**

- Step 8 makes a provisional selection: every pending step that is not cancelled, plus new pushes within the run's limits and a 10% margin (rounded up). Only leads looked up this run may push to cold or non-cold lanes; a lead whose sinks have no registered `Lookup` counts as looked up. A lead removed by a lookup past the margin is not replaced. Dry-run calls no `Lookup`; its planned lanes assume lookups pass.
- For every company with a stored open deal, and, when a HubSpot `Lookup` is registered, every company with a row in any `Export *` table, step 8 also sends one live lead at the company (with `CompanyDealID` set) to the HubSpot lookup, candidate or not, with pushes on or off.
- A lead goes to its highest-priority matching cold lane. An Apollo-held lead skips Apollo sequence lanes and falls through to its next matching cold lane. A lead may be reselected into the lane whose rows hold its cold push.
- The day's budget goes to non-cold pushes first, then cold, each ordered by lane `priority` (highest first), then score (highest first), then lead id. A push counts against the day of its `first_started_at`; a push with `first_started_at` set is a free retry.
- Within one company, non-cold steps run before cold steps, one at a time. The check just before each push re-evaluates the deal rule (section 7, rule 4) from the in-memory ledger, so a deal step called earlier in the run, even one that timed out, blocks a colleague's cold push.
- A cold lead with a `pending` row stays in that lane: it never moves to another cold lane while the row is open.
- Holding rows count for the lead itself and every lead whose `merged_into` chain ends at it. Non-cold rows never affect the cold push.

## 9. Setup runbooks

The README follows these steps in order (RFC section 6.14). It says up front that Google Cloud is the path for non-technical teams and the Docker paths are for technical users. Each step is marked as something the agent runs or something a person must do.

**The setup script** is `setup/gcp.sh` (bash with `gcloud`, run from macOS, Linux or Google Cloud Shell), one subcommand per step: `accounts`, `bucket`, `secrets`, `deploy`, `redeploy`, `schedule`. It reads and writes `leadscore.yml` only through `leadscore config get` and `config set-hosting`. The Sheet step is the Go command `leadscore setup sheet`, which the script calls.

**Credentials.** `leadscore setup sheet` runs with the person's own Google login, so the person owns the spreadsheet. Every other local command acts as the run account through Application Default Credentials made with `gcloud auth application-default login --impersonate-service-account=<run account>`, which `setup/gcp.sh accounts` runs at its end.

**Roles.**

| Account | Roles, on which resource |
|---|---|
| Run account | Secret Manager Secret Accessor on the key secrets and `leadscore-config`; Secret Version Adder on `leadscore-config` (for `config push`); Storage Object Admin on the lease bucket; Cloud Run Viewer and Cloud Scheduler Viewer on the project (for `doctor`); editor on the spreadsheet |
| Receiver account | Secret Manager Secret Accessor on `receiver-secret`, `receiver-secret-previous` and `leadscore-config`; editor on the spreadsheet (`Events` tabs protected for it) |
| Scheduler account | Cloud Run Invoker on the job `leadscore-run` |
| The person | the roles to create the above, plus Service Account Token Creator on the run account |

**The image.** Releases publish `ghcr.io/tetriz-ai/leadscore`. Cloud Run cannot pull from GHCR directly, so `setup/gcp.sh deploy` creates an Artifact Registry remote repository `ghcr-proxy` in the team's project pointing at `ghcr.io` and deploys through it. Before release, the image is the private registry's (`asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore`), passed to `deploy` directly.

### 9.1 Google Cloud

1. Prerequisites: a paid Apollo plan (for workflows and the API), `gcloud` installed and logged in, and the person holding the roles to create projects, service accounts, Cloud Run services and jobs, Cloud Scheduler jobs and Artifact Registry repositories, and to grant roles on them.
2. Install the `leadscore` CLI: the release binary for the person's OS, or `docker run --rm -v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro -v "$PWD":/config <image>`.
3. Create or pick a Google Cloud project with billing (a person approves billing).
4. Enable the Cloud Run, Cloud Scheduler, Secret Manager, Cloud Storage, Sheets, Drive, Artifact Registry and IAM Service Account Credentials APIs.
5. `setup/gcp.sh accounts` and `bucket`: create the run and receiver accounts with the roles above, create the lease bucket, write the `hosting` block, and finish with the impersonated login.
6. `leadscore setup sheet`, with the person's own login: create the spreadsheet from the section 4 schema (the template is code), including the current month's `Events` tab; share it with both accounts; protect the `Events` tabs for both accounts and every other tool tab for the run account only; set hourly recalculation; write the staleness formula; write `store.spreadsheet`. If a Workspace sharing policy blocks the share, it reports Drive's error and the README names the exception to ask the admin for.
7. `setup/gcp.sh secrets`: add the API keys and the receiver secret to Secret Manager (a person pastes each key).
8. Write the rubric, run `setup hubspot` if HubSpot is used, and upload the bundle with `leadscore config push`.
9. `setup/gcp.sh deploy <image>`: the receiver service (receiver account, minimum 0 and maximum 1 instance, sign-in check skipped, receiver secrets and bundle attached) and the run job (run account, `leadscore run`, task timeout the deadline plus the save budget, maximum retries 0, key secrets and bundle attached).
10. Create the Apollo workflows pointing at the service, from the templates in `setup/apollo/`.
11. `setup/gcp.sh schedule`: create a scheduler account, grant it Cloud Run Invoker on the job, and create the scheduler job from `schedule`. Pushes are still off, so these runs only score.
12. Run `doctor` until green, then open the `Ranked` tab to see the first results.
13. Review a dry run (a person approves).
14. Set `pushes_enabled: true` and `leadscore config push`.

### 9.2 Docker (your machine, or a server or VM)

1. Install Docker or podman; copy `compose.yaml`, the example `leadscore.yml` for the path (laptop or server) and the example rubric into one folder (mounted at `/config`).
2. Put the API keys and the receiver secret in `.env` (a person pastes each key).
3. Write the rubric from the team's CSV headers and the example.
4. On a server, set the team's domain in the compose file's Caddy section and enable it. On a laptop with a tunnel, start it and set `replies: receiver`. Then point the Apollo workflows at that address.
5. `docker compose up -d`. Pushes are off, so the first runs only score.
6. If HubSpot is used, `docker compose exec leadscore leadscore setup hubspot`; then `docker compose exec leadscore leadscore doctor` until green.
7. See the results: `docker compose exec leadscore leadscore ranked --csv > ranked.csv`, and the export lists in `./out`.
8. Review a dry run (a person approves).
9. Set `pushes_enabled: true` in `leadscore.yml`; the next run picks it up with no restart.

**Sheets on Docker.** As the store: Google Cloud steps 3, 4 and 6, one service account with a key file mounted into the container, and a lease bucket. As the optional view on SQLite: steps 3 and 4, and `leadscore setup sheet --view`, which creates a spreadsheet with only the `Ranked`, `Health` and export tabs and shares it with the one account; no bucket. The view is written by the `AfterSave` hook (section 12.6) through the Sheets API directly, with no lease and no schema version; a failed view write is a `Health` problem (`view_write_failed`) and the run stays healthy.

### 9.3 Upgrading and rolling back

A new version changes the store only by adding tables or columns (a minor schema version), which the next run creates. An older version runs on a store a newer minor version extended, ignoring and preserving what it does not know; only a newer major version is refused. Google Cloud: `setup/gcp.sh deploy <new image>`, then `doctor`. Docker: change the pinned image tag in `compose.yaml`, `docker compose up -d`, then `doctor`; the receiver is down for those few seconds, so webhooks sent then rely on Apollo retrying. Rolling back is deploying the previous tag. Copying the spreadsheet or the SQLite volume first is still recommended, for a store damaged some other way.

## 10. Doctor checks

`doctor` exits 0 when no check fails (warnings allowed) and 1 otherwise. It never writes the store and never takes the lease: it loads the model read-only when the store opens (otherwise model-based checks report "skipped") and only prints. Checks marked "in run" also run inside every run and make it unhealthy when they fail; the rest run only from `doctor`. Check names are unique.

| Check | Fails when | Suggested fix |
|---|---|---|
| `secrets` (in run) | a configured adapter's key variable is missing | add it to Secret Manager or `.env` |
| `receiver-secret` (`serve` start and `doctor`) | the receiver is configured and its secret is missing; a previous secret is still set (warning) | set it; finish the rotation |
| `hosting` (only when `hosting` is set) | service, job, scheduler job or `ghcr-proxy` missing; service not at most 1 instance; job task timeout not equal to the deadline plus the save budget, or retries not 0; deadline plus save budget not below `schedule`; scheduler account unable to run the job; the latest `leadscore-config` version differs from `State.config_version` | `setup/gcp.sh` the missing step, or `leadscore config push` |
| `rubric-version` (doctor only, warning) | `Health.rubric_version` from the last run differs from the local rubric's version | `leadscore config push`, or redeploy the matching file |
| `sheet-access` (in run) | an account cannot open the Sheet (not shared, or blocked by a Workspace sharing policy) | share it, or ask the Workspace admin for an exception |
| `sheets` (in run) | spreadsheet not set to recalculate hourly; cell use past 70% | `leadscore setup sheet --repair`; shorten `log_retention` or move to SQLite |
| `hubspot` (in run) | token missing the scopes; custom properties or pipeline and stage missing | re-create the private app; `leadscore setup hubspot` |
| `apollo-key` (in run) | the key fails the auth-health call; Apollo has no opt-out flag (warning for Apollo-only teams) | check the key |
| `apollo-sequences` (in run) | mailbox id not found; a lane's sequence name missing or ambiguous | check `sinks.apollo.mailbox_id` and the sequence names |
| `receivers` | with `replies: receiver`, `receiver.public_url`'s `/healthz` unreachable | re-deploy; on a laptop, `replies: polling` or a Cloudflare Tunnel |
| `receiver-silence` (in run) | any expected event kind silent past the threshold (raises `silent:<kind>`) | check the Apollo workflow |
| `lease` | on Sheets, the lease bucket missing or not writable. A held lease is shown with owner and expiry as a warning; it clears itself at expiry | `setup/gcp.sh bucket` |
| `pushes` (in run) | failed steps, or steps pending more than 24 hours | `leadscore retry`, or a `retry` row in Overrides |
| `store` (in run) | store from a newer major version; a SQLite store or CSV path while `CLOUD_RUN_JOB` or `K_SERVICE` is set; ledger below `ledger_rows` (pushing is blocked); SQLite on a disk that is not kept (its mount is overlay or tmpfs), or opened outside a container while `opened_by` names one | install a matching version; restore the ledger rows; use a named volume and `docker compose exec` |
| `rubric` (in run) | fails to compile; a rule reads a field that is not built in, declared, or a loaded column | fix the file |
| `overrides` (in run) | unknown value in Overrides; conflicting status rows | fix the cell |
| `pushes-enabled` | still off (a warning) | review `--dry-run`, then set `pushes_enabled: true` |
| `duplicates` (in run) | unresolved namesakes, and `key_conflicts`, with counts | resolve in Overrides (`distinct` or `same_as`) |

## 11. Defaults and owners

RFC section 7 explains the ownership rule; this is every value.

| Value | Owner | Where | Default |
|---|---|---|---|
| Run schedule | Team | `schedule` in `leadscore.yml` | every 15 minutes |
| Push limits and their timezone | Team | rubric `limits` | 100 per run, 200 per day, UTC |
| Pushes enabled | Team | `leadscore.yml` | off until reviewed |
| Enrichment max age, lookups per run and per day | Team | `leadscore.yml` | 30 days (core's value), 100, 400 |
| Replies via receiver or polling | Team | `leadscore.yml` | receiver; the laptop example file sets polling |
| Polling sequence length and window margin | Team | `leadscore.yml` | 30 days, plus 7 days |
| Ingest chunk size | Team | `leadscore.yml` | 2,000 rows per run |
| Receiver silence threshold | Team | `leadscore.yml` | 3 days |
| Log retention | Team | `leadscore.yml` | 90 days |
| Source channels | Team | `sources[].channel` | each source its own channel |
| Polling interval, push batch size, attempts before `failed`, call timeout | Us, fixed | code | 6 hours, 25 leads, 3, 30 seconds: no adopter need to tune them, and each knob adds docs and tests |
| Pending-push warning, pre-push lookup margin, cell-use warning | Us, fixed | code | 24 hours, 10%, 70%: thresholds for warnings and a safety margin, not policy |
| Receiver batch window, hold cap, `/healthz` cache | Us, fixed | code | 2 seconds, 10 seconds, 60 seconds: inside Apollo's request timeout and the Sheets limits |
| Save budget after the deadline | Us, fixed | code | 90 seconds: bounds the job's task timeout |
| Lease length | Us, fixed | code | the deadline plus the save budget plus 30 seconds: every run is stopped 30 seconds before its lease expires, and a killed run's lease expires before the next tick |
| Event window retention | Us, fixed | code | 90 days: the longest detector window a rubric may declare |
| `Seen events` retention | Us, fixed | code | one year: longer than any plausible vendor re-delivery |
| Receiver truncation | Us, fixed | code | 16KB per string (core's cap), then the 50,000-character cell limit |
| Staleness warning, skipped-run warning | Us, fixed | code | three schedule intervals; a gap of more than two intervals since the last run |
| Domain + name matching | Team | per CSV source | off |
| Reply-label map | Team | `reply_labels` | section 5.5; `unsubscribe` fixed |
| Run deadline | Team | `leadscore.yml` | 12 minutes |
| Status rules | Domain, fixed | code | an opt-out must never be undone by automation, and a live deal must never re-enter cold outreach |
| Built-in lane checks | Domain, fixed | code | turning them off would allow contacting opted-out or unidentifiable people |

## 12. Internal contracts between slices

Not public API: these live under `internal/` and may change between releases. They are fixed here so slices built in parallel fit together. A slice that needs to change one changes this section in the same PR.

### 12.1 Package layout

| Package | Owner slice | Holds |
|---|---|---|
| `internal/api` (re-exported by the root) | S1 | section 1 types, errors, registry; the built-in alias table and the header squash function |
| `internal/check` | S1 | the check framework (12.4) |
| `internal/config` | S1 | loading `leadscore.yml`, the rubric path, the bundle, `config get` and `set-hosting` (section 3) |
| `internal/logredact` | S1 | `Redact`, `VendorErrorDetail` (copied) |
| `internal/model` | S1 creates it empty; S4 fills it | the in-memory model (12.2) |
| `internal/store/codec`, `internal/store/sqlite` | S4 | codec and the SQLite store |
| `internal/store/sheets` | S5 | the Sheets store and lease |
| `internal/rules` | S2 | rubric compiler and evaluator (12.3) |
| `internal/merge` | S6 | normalize, merge, event persons, Overrides parsing (12.5) |
| `internal/engine` | S10a (loop, `DefaultHooks`), S10b (fold, lanes, ledger), S13 (`export.go`) | the run (12.6) |
| `internal/events`, `internal/detect` | S9 | event keys, effects and parsing (12.7); built-in detector kinds and window aggregates |
| `internal/receiver`, `internal/receiver/auth` | S14a | `serve`, with a handler constructor that takes a clock; the copied `channelauth` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | S5 | importable fake servers |
| `internal/fakes/apollo` | S8 (enrichment), S12 (the rest) | built from S0's fixtures |
| `internal/fakes/hubspot` | S11 | built from S0's fixtures |
| `internal/e2e` | S17 | the end-to-end suite |
| `adapters/apollo` | S8 owns `client.go` (key, base URL, 30-second timeout, the two call modes) and registration; S9 adds the body parsers, `PolledReplyKey` and `RequiredPaths`; S12 adds sinks, the `Lookup` and the `Poller` | |
| `adapters/hubspot`, `adapters/csv`, `adapters/sheetsource` | S11, S7, S5 | |

**Import rule.** Inside the module, only `cmd/` and `_test.go` files import the root package; `internal/*`, `adapters/*`, `storetest` and `sinktest` use `internal/api`, whose names the root aliases (so `api.Backend` is `leadscore.Backend`). A root import from anywhere else becomes an import cycle once the root reaches that package; a test in the root package enforces the rule. Code blocks in section 12 therefore write `api.X`.

**Conformance tests** call `storetest.Run` and `sinktest.Run` from an external test package (`package sqlite_test`, `package apollo_test`), never from inside the package under test.

### 12.2 The in-memory model (S4)

One Go struct per section 4 table, with typed fields for known columns and an `Extra map[string]string` for unknown ones. Keyed tables are held in maps by primary key (key type `[]string` in section 4 key order); keyless tables (`Overrides`, `Log`) are ordered slices. Two indexes: identities by key, and people by company domain.

- Every change goes through `Model.Put(table, row)` and `Model.Delete(table, key)`, which record what changed.
- `codec.Encode(model, tables ...string) []TableWrite` turns the recorded changes for the named tables (all when none named) into writes: `OpAppend` for new rows of `Seen events`, `Window events`, `Log` and `Identities`; `OpUpsert` for new or changed keyed rows (including `Applied rows`); `OpDelete` for deleted rows (by key, or all columns for `Overrides`); `OpReplace` for `Ranked` (first chunk; later chunks append); `OpTrim` for retention. For `State`, a table name may carry a key prefix (`State:cursor:`).
- `Model.Committed(writes)` clears only the committed changes after `Commit` succeeds. `Model.Discard()` drops all uncommitted changes (dry-run, and `ErrTooLarge` reloads).
- No slice writes a table any other way.

### 12.3 The evaluator (S2)

- `rules.Compile(yaml) (*Rubric, error)`.
- `rubric.Evaluate(in Input) (verdicts map[LeadID]Verdict, blocked map[LeadID]string, warnings []string)`. `Input` carries leads (as `LeadRef`), company facts and `leads_seen` per domain, `rules.DetectorResults` (per subject, per detector), and each lead's folded status. `blocked` names leads stopped by `conflicts`, with the reason. Warnings (for example an unparseable number) are written to `Log` by the engine. The evaluator computes nothing merge produces; S3 builds `Input` directly from fixture rows.
- Accessors: `Fields() []string` (every field the rubric reads), `Aliases() map[string]string`, `ConflictFields() []string`, `Lanes() []Lane`, `Limits() Limits`, `Detectors() []DetectorSpec`, `Version() string`.

### 12.4 The check framework (S1)

```go
type Check interface {
    Name() string                        // unique; the section 10 names
    InRun() bool                         // also runs inside every run
    Run(ctx context.Context, env Env) []Problem
}
type Env struct {
    Config *config.Config
    Model  *model.Model // nil when doctor could not load the store
    Store  api.Backend
    Events api.EventLog
}
type Problem struct{ Key, Message, Fix string; Warning bool } // Key in the section 4 form <kind>:<id>
func Register(c Check)
```

A problem is written to `Health` under `Problem.Key` as given, and cleared when a later run of the same check stops returning it. Owners: S1 `secrets`; S2 registers `rubric` (compile), and S10a adds the field part to that same check (`Fields()` against the loaded columns in `Env.Model`); S4 `store` (SQLite cases) and S5 its Sheets cases, `sheets`, `sheet-access`; S6 `overrides` (raising `status_conflict:<lead>` and `override_unmatched:<row>`) and `duplicates`; S8 `apollo-key`; S10b `pushes` and the ledger part of `store`; S11 `hubspot`; S12 `apollo-sequences`; S14a `receiver-secret`; S14b `hosting`; S15 `receiver-silence`; S16 `rubric-version`, `receivers`, `lease`, `pushes-enabled`. S14b extends `secrets` for Secret Manager keys (a local command on a hosted install, where S1's check skips the environment variables).

### 12.5 Merge owns persons, aliases and row ids (S6)

- `merge.Normalize(row InputRow, aliases map[string]string) (Normalized, error)` resolves headers with the section 2 table plus rubric aliases (first header in `row.Headers` order wins), computes the per-row id (RFC 6.5; for source `receiver`, the `contact_id` column, else email, else LinkedIn URL), the row hash (SHA-256 over the sorted raw `header=value` pairs plus a hash of the `aliases` map, so an alias change re-applies rows), and does the email-shape and required-key checks. A reject is recorded in `Applied rows` with an empty `lead_id` and logged once.
- `merge.Apply(m, rows []Normalized, c ApplyCtx{Now, RunID, Sources})` applies rows incrementally (RFC 6.5, section 7's `same_as` rules), writes company facts from built-in company columns with origin `input`, and reads the `Companies` tab with origin `companies_tab`.
- `merge.FindPerson(m, Event) (LeadID, bool)` matches an event's person, with no writes: first by `Attrs["contact_id"]` through `Applied rows` (source `receiver`, row id), then through `Identities`, then follows `merged_into` to the live lead. `merge.ApplyEventPerson(m, Event) LeadID` does the same and, when no lead matches, creates one under source id `receiver`. `Intake` merges receiver rows before resolving persons.
- `merge.ParseOverrides(m) Overrides` (normalized persons) and `merge.Duplicates(m) map[LeadID]bool`.
- S7's CSV source only parses files: UTF-8 with an optional BOM stripped, comma-delimited, ragged rows allowed, headers returned in `Headers` as written. For event rows it uses the `internal/api` alias table to find `event`, `at` and the person columns.

### 12.6 The run and its hooks (S10a)

```go
type Run struct {
    ID           string          // run id (UUIDv7), also the lease owner
    Ctx          context.Context // cancelled at the hard stop; vendor calls and the post-batch write use it
    PushCtx      context.Context // cancelled on Stop or at the deadline; stops new vendor calls only
    Config       *config.Config
    Rubric       *rules.Rubric
    Model        *model.Model
    Store        api.Backend
    Events       api.EventLog
    Lease        api.RunLease
    DryRun       bool
    Now          func() time.Time
    HTTPClient   *http.Client
    SourceEvents []api.Event // step 3 source events, set by S10a before Intake
    NoPush       string            // non-empty: score and save, but push nothing (the reason)
    Problem      func(key, message, fix string, warning bool) // raise an open problem this run
}
type Hooks struct {
    Intake    func(*Run) error                       // step 3 after sources (S9)
    Enrich    func(*Run) error                       // step 4 (S8); skipped on dry-run
    Fold      func(*Run) error                       // step 5; default sets every lead to new (S10b)
    Detect    func(*Run) (rules.DetectorResults, error) // step 6, before Evaluate (S9)
    PrePush   func(*Run, []api.LeadID) error   // step 8 (S10b)
    Push      func(*Run) error                       // step 9 (S10b)
    ReRead    func(*Run) (changed []api.LeadID, err error) // before each pushing batch (S15)
    Export    func(*Run) error                       // after Push, before phase 2, every run (S13)
    AfterSave func(*Run) error                       // after phase 2 committed: CSV rewrite (S13), view (S16)
}
func DefaultHooks() Hooks // the production set; each hook slice sets its field here in its own PR

// Owned by S10b, used by S13:
func Blocked(r *Run, id api.LeadID) (blocked bool, reason string) // blocked on every lane
func MatchesLane(r *Run, id api.LeadID, laneID string) bool
// RunWith is the test entry point: it adds base_url and _http_client to every
// vendor and store block (section 3) and uses the given clock.
func RunWith(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client) (api.RunResult, error)
```

- **Wiring.** `leadscore.Run`, `leadscore run`, the `serve` timer and the e2e suite all use `DefaultHooks()`. `leadscore run` closes `Stop` from its own SIGTERM handler; `serve` closes it at shutdown.
- **Sources and chunks.** S10a calls every `Source.Fetch` at step 3, puts source events in `SourceEvents`, normalizes and merges rows, and saves `cursor:<source id>` in phase 1 only when all of that source's rows were taken this run. The backlog is accepted rows whose (source, row id, row hash) is not yet in `Applied rows` after this chunk; while it is non-empty, `NoPush` is set.
- **Phases.** Phase 1 commits `People`, `Identities`, `Applied rows`, `Seen events`, `Window events`, `Outcomes`, `Pushes` (load-time fixes, cancels, retry resets), `Applied overrides`, and `State` cursors, `last_poll_at` and `first_run_at` (set by the first run). Each pushing batch commits `Pushes` and `State.ledger_rows`. Phase 2 commits everything else (including the `Export` tables) except `Ranked`, which is written after phase 2 in chunks (12.2).
- **`ErrTooLarge`.** On phase 1 only, the run discards the model, reloads, and redoes steps 3 to 6 with half the rows; a second `ErrTooLarge` sets `NoPush`. Other commits retry once at the same size, then fail the run.
- **Hook errors.** `Intake` returning `ErrEventsShrank` sets `NoPush` and continues; any other `Intake` error fails the run before phase 1. `Enrich`, `Detect` and `Export` errors make the run unhealthy and it continues. A `PrePush` error skips `Push`. `Push` and `ReRead` errors make the run unhealthy.
- **Skipped runs.** A run that finds the lease held writes nothing. The next run that holds the lease raises `skipped_runs` when more than two `schedule` intervals passed since `last_run_at`.
- **Re-reads.** `ReRead` resolves each new event's person with `merge.FindPerson` (skipping unknown persons, which the next run creates) and applies its effects to `Run.Model`'s `Outcomes` through `events.Apply` (idempotent), without moving cursors, writing `Seen events` or appending `Window events`, and returns the changed leads; S10b re-folds them before the batch. The next run re-applies those events normally.
- **Run-level checks.** Before scoring, S10a fails the run if `rubric.Fields()` names a field that is not built in, declared, or a loaded column. Hooks raise problems through `Run.Problem`; unreported problem keys are cleared at save.

### 12.7 Events (S9)

- `events.Key(Event) EventID` holds every de-duplication key rule (RFC 6.7). `internal/events` imports `adapters/apollo`, never the reverse: `adapters/apollo` defines `PolledReplyKey(messageID, label string) EventID` and the body parsers, and `events.Key` calls `PolledReplyKey` for `reply` events.
- `events.Apply(m, lead LeadID, e Event)` applies a section 5.3 effect to the live lead (following `merged_into`), including the label map and `reply_labels`, the origin rule and the deal fan-out. It is the only code that writes event effects into `Outcomes` and `People`. The caller resolves the person: `Intake` through `merge.ApplyEventPerson`; S10b and S15 with the lead they looked up or re-read.
- `events.Parse(raw []RawEvent) ([]Event, []InputRow)` is pure: it returns events and the receiver input rows (source id `receiver`), with no writes.
