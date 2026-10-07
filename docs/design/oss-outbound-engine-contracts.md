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
    Extra        map[string]string // other facts (the Apollo enricher writes latest_funding_at); an empty value clears that fact (section 4)
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
    Done           []LedgerRef       // Lookup calls only: the lookup's sink's done steps for the lead and every lead merged into it
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
    // Enrich calls domains in the order given and stops on a rate limit, when
    // it returns the facts so far and ErrRateLimited. A failure on one domain is
    // skipped; a failure every later domain would likely share (the key
    // refused, ctx done, three failures in a row) also stops it, returning the
    // facts so far and that error. The caller counts calls made as the index of
    // the last domain tried, plus one. The engine calls it one domain at a time.
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
    OpAppend                 // add rows; on a keyed section 4 table, a key the table already holds fails the commit
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
    // Every write was saved, but a people-owned table needs a person's look (Sheets: section 4, "Sheets people tabs").
    // The engine treats the commit as done and raises a Health problem; it never resends the writes.
    ErrCommittedWithProblems = errors.New("committed, but a people-owned table needs attention")
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
    // anything. An OpAppend to a keyed section 4 table of a key the table already
    // holds (or that the same commit already wrote) fails the whole commit. It creates a missing table, and appends a missing column, the first
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
func RegisterDetector(kind string, f func(params Config) (Detector, error)) // kind stored lowercased; kinds differing only in case collide
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
// DeleteProcessed dropping a partition, ErrEventsShrank (a cursor saved from
// one store read against a fresh store), and a commit that fails while applying
// (its last write an OpAppend of a key a keyed table already holds) leaving
// nothing applied.
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

A rubric is one YAML file. Top-level keys, all optional except `version` and `lanes`: `version` (1), `fields`, `settings`, `company`, `detectors`, `derive`, `conflicts`, `score`, `limits`, `lanes`. RFC section 6.4 has an abridged example and the built-in fields. YAML aliases (`*name`) and a key repeated in one mapping fail at load, naming the line: an alias's expansion can grow exponentially, and a repeated key would silently keep only the last value.

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

Core's `data_quality_note`, `primary_ai_coding_tool` and `visited_domain` aliases do not ship. A declared field also matches its own squashed name and its `aliases`; a field's `aliases` win on a clash with the built-in table. Any other header is kept under its squashed name, and rules refer to it by that name (a header `Primary AI coding tool` is `primaryaicodingtool`). A rule naming an undeclared field that is not a squashed name (lowercase letters and digits only) fails at load, since no column can carry it; so does a rule naming a header spelling that resolves to another field (`jobtitle` is `title`; write `title`).

**Fields.** `fields: { <name>: { type, level, aliases } }`. `type` is `text`, `number`, `date` (ISO 8601), `bool` (`true/false/yes/no/1/0`), or `{ ordered: <settings list> }`. `level` is `lead` (default) or `company`. A non-built-in `company` field is read from the `Companies` tab (and enrichment `Extra`, keyed by the field name with or without `company.`; an exact `company.<name>` key wins over `<name>`); an undeclared `company.<name>` is such a fact, as text. To lift a lead column to the company, use a rollup. Declaring a built-in field may add `aliases` but not change its type or level; `status` cannot be declared. Declaring a field whose own name or alias is a built-in header spelling of another field (for example `stage`) is allowed, with a warning, and the rubric's field wins. Undeclared columns are `text` at lead level. A value that does not parse as its type is absent. A date with no time is midnight UTC, and dates compare as instants. A derived name shadows an input column (or, for a company block, a company field) of the same name from that block on, and the compiler warns; `status`, `sources_seen` and `receiver_only` cannot be derived, and neither can a fixed `Ranked` column name (section 4: `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, `account_score`, `contact_score`, `score`, `status`, `lane`, `reasons`, `rubric_version`). Every derived name has one type: all its `then`/`else` values are numbers, or all text, or all booleans (or null). Names in `fields`, `settings`, `company`, `detectors` and `derive` use lowercase letters, digits and underscores, starting with a letter.

**Settings.** `settings: { <name>: <list or scalar> }`. A list is an ordering for `ordered` fields; unknown values sort below all. `company.funding_stage` ranks by `settings.funding_order`, which a rubric must declare only to compare it with `lt`, `lte`, `gt` or `gte`; the value compared against must be in the list. Conditions refer to a setting as `$<name>`.

**Text matching.** Every text comparison (`eq`, `ne`, `in`, `not_in`, `contains`, and matching a value to an `ordered` list) is case-insensitive after Unicode NFC normalization and trimming. Matching to an `ordered` list also ignores every Unicode space, hyphen and underscore, so `Series B` matches `series_b`.

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

Any comparison on an absent value is false, so only `missing` is true for it (and `not` of a comparison). A condition on `status` with `eq`, `ne`, `in` or `not_in` may name only the statuses in RFC 6.3, so a typo fails at load (`contains` and raw `expr:` are not checked). `in: []` is never true and `not_in: []` is true for any present value. A raw `expr:` that fails at run time (for example `lead.x` on a lead without `x`) counts as false, with a warning that names the rule but never a lead's values. Detectors are read with `{ detector: D }` (or `detector.D` in `expr:`), not `field:`.

**Cost limit.** Each condition may cost at most 1,000,000 CEL cost units (about one per operation) for one lead. A condition whose estimated worst case is over that fails at load (the estimate takes `lead`, `company` and `detector` at up to 1,000 entries with values of up to 50,000 characters, settings at their sizes in the rubric, and a list written in the expression at its own size); evaluation stops one that reaches it, and it counts as false with a warning. `F` is a built-in field, a declared or input column, `company.<name>` for a company field or rollup, or a derived name.

**CEL variables** (for `expr:`):

| Variable | CEL type | Holds |
|---|---|---|
| `lead` | `map(string, dyn)` | lead fields and lead-level derived names, typed per `fields`; absent keys are missing |
| `company` | `map(string, dyn)` | company fields, rollups and company-level derived names |
| `detector` | `map(string, bool)` | every detector, true when fired for the lead or its company |
| `status` | `string` | the folded status |
| `settings` | `map(string, dyn)` | the `settings` block |

Test presence with `has(lead.x)`. A company-level condition (a company derive block, an account score rule) has no `lead` and no `status` variable.

**Company rollups.** `company: { <name>: <rollup> }`, computed over a company's leads: `{ any: C }` and `{ all: C }` (bool), `{ count: C }` (number), `{ max: F }` and `{ min: F }` (number or date), `{ first: F }` (the first present value, taking leads oldest first by first seen). Read as `company.<name>`.

**Detectors.** `detectors: { <name>: { kind, subject, ... } }`, `subject` is `lead` (default) or `company`. Every `window` and `within` is longer than zero and at most 90 days. A company-subject detector sees every event whose domain is the company, lead events included.

| Kind | Parameters | Fires when |
|---|---|---|
| `count_in_window` | `event`, `window`, `min` | at least `min` events of that kind in the window |
| `first_seen` | `event`, `within` | the first event of that kind ever was within `within` |
| `change` | `field` (a stored company fact: not a rollup, `leads_seen` or `domain`), `within`, optional `from`, `to` | the fact changed within `within` (by `facts.<f>.at`), matching `from` and `to` when given |
| a registered kind | its own parameters under `params:` | as its `Detector` decides |

`event` may end in `*` (and contain no other `*`) to match a prefix, for example `visit_*`. Event kinds are compared lowercased, and so are detector `kind` names: `Count_In_Window` is `count_in_window`, with its checks; `RegisterDetector` stores a kind lowercased (so `Mixed_Kind` is found as `mixed_kind`), and two kinds that differ only in case panic as a duplicate.

**Derive.** `derive: { <name>: <block> }`, evaluated in file order. A block is a list of rules (lead level) or `{ level, rules }`. A rule is `{ when: C, then: V }`; the last may be `{ else: V }`. The first matching rule wins. `then: null`, or no rule matching and no `else`, gives "no value": later comparisons on it are false and `missing` is true. A company block may read company fields and rollups, company-subject detectors, and earlier company blocks; a lead block may read anything above it. The name `fit_signal` is the verdict's fit signal when a rubric defines it; `tier` and `priority`, when defined, are the names whose changes are logged.

**Conflicts.** `conflicts: [ { field: F } ]`. A lead whose sources gave different non-empty values for F (recorded by merge in `People.conflicts`) is blocked on every lane, with the reason.

**Score.** `score: { account: [ <rule> ], contact: [ <rule> ] }`. A rule is `{ when: C, points: N }`, or a band rule `{ band: F, points: { <threshold>: N, ... } }`, which pays the points of the highest threshold at or below F's number (a fourth source adds nothing rather than falling to zero); a threshold may appear once (`2` and `2.0` are the same). Account rules may read only company-level values. A lead with no company gets account half 0. The score is the sum of both halves.

**Limits.** `limits: { max_pushes_per_run, max_pushes_per_day, timezone }`, defaults 100, 200, UTC; `timezone` is an IANA name, and `Local` is refused because it differs between machines.

**Lanes.** `lanes: [ { id, name, kind, priority, when, push } ]`. `id` is required, unique ignoring case, and must never change; it starts with a letter or digit and uses letters, digits, `-` and `_`, since it names a table and a file; `name` defaults to the id and `priority` to 0; `when` is required. A cold lane whose `when` is not, and is not an `all` that includes, `{ field: receiver_only, eq: false }` gets a load warning (RFC 7); `kind` is `cold`, `non-cold` or `export`; a higher `priority` wins; `push` is `<sink>:<destination>`: `apollo:sequence/<sequence name>`, `hubspot:contacts`, `hubspot:deals`, or `export:<anything>` (export lanes only, and only export lanes use `export:`). At load the compiler checks this syntax; that the sink is registered is checked at run start (an `export` lane needs no sink): a lane whose sink is not registered raises `lane_sink_unregistered:<lane id>` and makes the run unhealthy, and the run goes on.

**Version hash.** `r-` plus the first 16 hex characters of the SHA-256 of the YAML re-marshalled with sorted keys and no comments.

## 3. `leadscore.yml`

Unknown engine keys fail loading, naming the key; adapter blocks are passed through as `Config`. Engine keys (including a source's `id`, `type`, `channel`, `path` and `tabs`) are read as text exactly as written, so `spreadsheet: 0123` and `tabs: [2024]` are `"0123"` and `"2024"`. Any other key in an adapter block reaches the adapter with YAML's types (`0123` arrives as a number), so quote ids there. Relative paths are relative to the folder holding `leadscore.yml`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | number | required | `1` |
| `rubric` | path | `rubric.yml` beside `leadscore.yml` | the rubric file |
| `store.type` | `sqlite`, `sheets`, or a plug-in | required | which store |
| `store.path` | path | `/data/leadscore.db` | SQLite file (on the named volume) |
| `store.spreadsheet`, `store.lease_bucket` | text | —; `lease_bucket` is `<hosting.project>-leadscore-lease` when `hosting.project` is set | Sheets: the spreadsheet id and the Cloud Storage bucket for the lease (bucket names are global: set it only when the default is taken) |
| `store.view_spreadsheet` | text | — | SQLite only: the optional read-only Sheet view |
| `store.credentials` | path | Google's standard loading | Docker with Sheets: the service-account key file |
| `sources[]` | list | — | each `{ id, type, channel, path or tabs, events, apollo_held, match_domain_name }`; `id` required, and not one of the reserved origins `receiver`, `polling`, `hubspot`, `apollo_lookup` (a source under one could pose as a vendor); every event a source returns gets `Origin` = its id, whatever the source set; `channel` defaults to the id and is what `sources_seen` counts (two conference CSVs with `channel: conference` count once, as core counts channels); only input rows (and identities) count, so an `events: true` source adds nothing to `sources_seen`, while a receiver request's input row (section 5.1) adds `receiver`; the flags default false. The engine copies `store.spreadsheet` (or `store.view_spreadsheet` on SQLite) and `store.credentials` into a `sheetsource` entry as `spreadsheet` and `credentials` |
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
| `reply_labels` | map | section 5.5 | per-label overrides: `{ <label>: <status or none> }`, with `_unlabelled` for no label; allowed values are the `replied_*` statuses and `none`. The `unsubscribe` label cannot be overridden. Labels compare ignoring case, so two keys differing only in case are refused |
| `pushes_enabled` | bool | `false` | cold and non-cold lanes push only when true |
| `schedule` | duration | `15m` | Docker: the `serve --every` interval (read when `serve` starts); Google Cloud: given to Cloud Scheduler, and must be a whole number of minutes dividing 60 (`*/N * * * *`) or of hours dividing 24 (`0 */N * * *`; `24h` is `0 0 * * *`) |
| `deadline` | duration | `12m` | run deadline |
| `ingest_chunk_rows` | number | `2000` | input rows processed per run, shared across sources (events are not chunked) |
| `silence_threshold` | duration | `3d` | receiver silence before `Health` flags it |
| `log_retention` | duration | `90d` | how long `Log` rows are kept |
| `hosting` | object | — | Google Cloud only, written by setup: `{ project, region, run_account, receiver_account, image }`. Resource names are fixed: service `leadscore-receiver`, job `leadscore-run`, scheduler job `leadscore-schedule` and its account `leadscore-scheduler`, Artifact Registry remote repository `ghcr-proxy`, secrets `leadscore-config`, `leadscore-config-version`, `apollo-api-key`, `hubspot-token`, `receiver-secret`, `receiver-secret-previous` |

**Which block each adapter gets.** A source gets its `sources[]` entry (with `id` and `type`); the enricher gets `enrich`; the store gets `store`; a sink of type T gets `sinks.T`. A `Lookup` and a `Poller` of type T are built from `sinks.T` when that block exists (the Poller only with `replies: polling`). Two unexported keys serve tests in every vendor and store block: `base_url` and `_http_client`; every factory honours them, and Google clients skip authentication when `base_url` is set. `RunWith` (section 12.6) adds `_http_client` to every block; a test sets each block's `base_url` (its fake's URL) in its YAML.

**Default paths.** Without flags, commands read `/config/leadscore.yml` if it exists, else `./leadscore.yml`; `--config` and `--rubric` override. When `/config/bundle.yaml` exists, it is used instead. In Docker, `compose.yaml` mounts the team's folder at `/config`.

**Hosted bundle.** `leadscore config push` uploads one Secret Manager secret, `leadscore-config`, whose value is a YAML document with two keys, `config` and `rubric`, each holding that file's text. It is mounted into the job and the service at `/config/bundle.yaml`. `config push` adds the bundle's version first, then adds that version's number as a new version of the secret `leadscore-config-version`; if the second write fails, the push fails and says to run it again. The run job gets the environment variable `LEADSCORE_CONFIG_VERSION` from `leadscore-config-version:latest` (S14b's deploy attaches it), so both are read when an execution starts and a push needs no redeploy. At step 1, a run reads `LEADSCORE_CONFIG_VERSION` and writes it to `State.config_version` in phase 1; when the variable is unset, the stored value is left as it is.

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
| `Company facts` | `domain` | `domain`, `facts` (JSON: fact to {value, origin, at}, the resolved winner), `previous` (JSON, same shape: the value a change replaced), `rollups` (JSON), `first_seen` (JSON: event kind to time, for every event whose domain is the company), `enriched_at`, `not_found_at`, `enrich_failed_at` (the last lookup that got no answer; the domain waits a day) |
| `Events YYYY-MM` (Sheets) / `Events` (SQLite) | `seq` | `seq`, `received_at`, `kind`, `body` |
| `Window events` | `event_key` | `event_key`, `subject` (`lead` or `company`), `lead_id`, `domain`, `kind`, `at`, `attrs` (JSON) |
| `Applied rows` | `source_id`, `row_id` | `source_id`, `row_id`, `row_hash` (one row group's hash, section 12.5), `lead_id` (empty for a rejected row that never applied; a row that applied before keeps its lead), `first_applied_at`, `key_conflict_at` (when the row first carried a key another lead holds; it is counted once) |
| `Seen events` | `event_key` | `event_key`, `first_received_at`, `run_id` |
| `Outcomes` | `lead_id` | `lead_id`, `status`, `status_at`, `unsubscribed_at`, `unsubscribed_origin` (`event`, `lookup`, `manual`), `reply_status`, `reply_at`, `contacted_at`, `deal_id`, `deal_stage`, `deal_checked_at` (`deal_stage` holds only the class, `open`, `won` or `lost`; the vendor's own stage name is stored nowhere, and the deal rule reads only the class) |
| `Ranked` | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, then one column per derived name, then `account_score`, `contact_score`, `score`, `status`, `lane`, `reasons`, `rubric_version`. `email` is the primary email and `linkedin_url` the lead's LinkedIn identity in canonical form (`linkedin.com/in/<slug>`, section 12.5). `lane` before PrePush is the highest-priority lane whose `when` holds; after PrePush, the highest-priority lane the lead is pushed to this run, else the export lane it is listed on (passes the built-in lane checks); empty when none, or when a rubric conflict blocks the lead. `reasons` ends with why each matching lane was skipped, export lanes included |
| `Pushes` | `lead_id`, `lane_id`, `step` | `lead_id`, `lane_id`, `step`, `lane_kind`, `dest`, `state`, `vendor_id`, `attempts`, `called_at`, `intent_run`, `last_error`, `first_started_at`, `updated_at`. Cold and non-cold lanes only |
| `Log` | — | `at`, `run_id`, `level`, `lead_id`, `email`, `kind`, `message`, `rubric_version`. Only appended and trimmed; runs do not load it. Kinds include `tier_change`, `priority_change`, `row_rejected`, `event_rejected`, `event_ignored` (a stored request of a kind we do not act on), `event_unmatched`, `key_conflict`, `merged`, `push_failed` |
| `Health` | `kind`, `key` | `kind` (`result` or `problem`), `key`, `value`, `first_seen_at`, `updated_at` |
| `State` | `key` | `key`, `value` (keys below) |
| `Export <lane id>`, one per export lane (CSV: `<export.dir>/<lane id>.csv`) | `lead_id` | `lead_id`, `email`, `linkedin_url`, `full_name`, `company_domain`, `score`, `reasons`, `first_listed_at`, `status`, `do_not_contact`, `updated_at`. `linkedin_url` is in canonical form, as in `Ranked`; `score` and `reasons` are as when listed (only `status` and `do_not_contact` are refreshed) |

**Primary email.** The lead's newest email identity from a same-source correction, else its email identity with the earliest `first_seen_at`. Merge keeps it in `People.fields["email"]`, which moves only on such a correction. **Company domain.** Stored in `People.fields["company.domain"]`; the people-by-domain index reads it.

**Company fact origins**, highest first: `companies_tab`, `enrichment`, `input`. `facts` holds the winner. A source never replaces a fact from a higher origin. A refresh with an unchanged value leaves `facts.<f>` and `previous.<f>` alone (only `enriched_at` moves); a changed value moves the old entry to `previous` and sets `at` to the fetch time. A `companies_tab` fact whose cell is emptied, or whose row is removed, moves to `previous` and leaves `facts`, so a lower origin can fill it. An enrichment answer that leaves a field out keeps the stored fact; a value the vendor sent that cannot be mapped (a funding label or date the enricher does not know, given as an empty `Extra` value) moves the stored fact below `companies_tab` to `previous` and out of `facts`, so a stale stage does not outlive the vendor's change. An exact `company.<name>` key in `Extra` wins over `<name>`.

**`Health` rows.** Results: `last_result` (`healthy` or `unhealthy`), `last_run_at`, `last_success_at` (moved only by a healthy run that scored; a run stopped before scoring is not a success), `run_id`, `rubric_version`, `schedule`. Problems: key `<kind>:<id>`, exactly as the check or hook gives it, for example `push_failed:<lead>:<lane>:<step>`, `namesake:<lead>`, `secret_missing:<variable>`, `status_conflict:<lead>`, `override_unmatched:<row>`, `override_unknown_lane:<row>`, `merge_cycle:<lead>`, `receiver_only_push:<lead>` (a warning: a step was called for a lead known only from the receiver; kept while the lead stays receiver-only), `silent:<kind>`, `skipped_runs`, `key_conflicts`, `ledger_shrank`, `push_pending` (a warning: the count of steps pending over 24 hours), `apollo-key:auth`, `apollo-key:config`, `apollo-key:unreachable` and `apollo-key:no_optout_flag` (warnings), `enrich_failed` (a warning: lookups that got no answer), `lookup_failed:<lookup type>` (a whole lookup failed; a warning when only some leads failed), `sink_failed:<sink type>` (a sink could not be built, or has no steps for a lane), `view_write_failed`, `export_dir_readable` (a warning), `export_lane_invalid:<lane id>`, `poll_failed:<sink type>`, and the run's own: `source_failed:<source id>`, `lane_sink_unregistered:<lane id>`, `rubric_unknown_field:<field>`, `step_failed:<hook>`, `events_shrank`, `ingest_backlog` (a warning), `deadline_passed`, `run_stopped` (a warning), `commit_too_large`, `run_failed` (also for a `Ranked` write that fails). A problem's `value` is its message and fix (`<message>. Fix: <fix>.`, prefixed `warning: ` for a warning). Each run rewrites the problem rows: it keeps `first_seen_at` for a problem still open and deletes resolved ones; a run that failed or was cut short (deadline, `Stop`) deletes none, since it did not re-check them. On Sheets, cell `H1` of the `Health` tab (outside the table, the one exception to exact width) holds the staleness formula, rewritten each run: `=IF(NOW()-DATEVALUE(LEFT(<last_success_at cell>,10))-TIMEVALUE(MID(<last_success_at cell>,12,8))>3*<schedule in days>,"STALE: no successful run in 3 intervals","ok")`. `<schedule in days>` comes from the `schedule` result row (the 15-minute default when it is missing); with no `last_success_at` row yet the cell holds `="STALE: no successful run yet"`. The stored times are UTC, so setup sets the spreadsheet's time zone to UTC (`Etc/GMT`) and `NOW()` matches them. `Health` has room for seven columns (A to G) before `H1`; its tab is eight columns wide.

**`State` keys.** `schema_version` (`major.minor`), `config_version`, `cursor:<source id>`, `cursor:events`, `last_poll_at`, `first_run_at`, `last_received:<kind>` (received time of the newest event of each kind), `enrich_count:<YYYY-MM-DD>` (UTC; only today's key is kept, a run deletes the others), `ledger_rows` (the highest committed ledger row count; never lowered by a run), `key_conflicts` (running count), `opened_by` (the hostname of the `serve` process that last opened a SQLite file, written only when that `serve` runs in a container and cleared otherwise), `export_lane:<lane id>` (`yes`: the lane once had an `Export` table, so the table is still loaded and kept current after the lane leaves the rubric), and on SQLite `lease_owner` and `lease_expires_at`. The `store` check (SQLite) fails when the current process is not in a container (no `/.dockerenv`) while `opened_by` names one.

**Sheets events.** On Sheets an event's sequence is its position: its monthly tab (by the UTC month of `received_at`) and its row there. Each appended row's `seq` cell holds a random id. A cursor holds, for every tab read, the number of data rows read from it and the id of the last one; a read whose tab is shorter than that (checked against the tab's row count before reading, since Sheets refuses a range below the grid) or whose row at that place has another id (rows sorted, inserted or deleted) is `ErrEventsShrank`. A batch is appended in one `batchUpdate` (`appendCells`, so all-or-nothing even when it spans two months), creating a missing month's tab (hidden) in the same request. `DeleteProcessed` deletes whole tabs only (deleting rows would move the positions cursors hold): a tab whose rows are all at or below the committed cursor (its last row being the one the cursor read), whose month ended before `olderThan`, whose month plus the one-hour grace has passed, and that is not the newest `Events` tab. The same `batchUpdate` records each deleted month in the spreadsheet's developer metadata (`leadscore.events_deleted`), and `ReadEvents` skips a cursor month recorded there instead of failing, so a run that crashed after the delete and before saving the shorter cursor reads on.

**Sheets tabs the store creates** (an export lane's, a new month's `Events` tab) copy the protection of their siblings: an `Events` tab the editors of an existing protected `Events` tab, any other tool tab those of an existing protected tool tab. When no protected `Events` tab is left, a new one gets the tool tabs' editors plus the appending account (as Drive reports it). People-owned tabs are never protected. The machine-data tabs (`People`, `Identities`, `Company facts`, `Window events`, `Applied rows`, `Seen events`, `Applied overrides`, `State`, `Events YYYY-MM`) are hidden when created.

**Sheets people tabs.** A commit that deletes or changes rows of a people-owned tab (`Overrides`) re-reads it just before sending and refuses if it changed, and re-reads it after: a row deleted by mistake (rows sorted at that instant) is put back, and a row meant to go that is still there is reported. The write itself landed, so `Commit` returns `ErrCommittedWithProblems` (never a plain error, which would make the caller resend writes already saved); the engine marks the commit done and raises the `people_tab_check` problem (a warning) with the store's message, and the next run deletes the intended row from a fresh read. An upsert writes only the cells it changes, so a team's formula in an extra column survives. A new column goes after the last used column.

**Sheets lease file.** `gs://<lease_bucket>/leadscore-lease.json`: `{"owner": "<run id>", "expires_at": "<time>"}`. Taking it writes with `ifGenerationMatch` (0 when absent); release deletes it with `ifGenerationMatch`. A Sheets commit whose encoded body is over 9MB, or that the API rejects as too large, is `ErrTooLarge`.

**Export rows.**

- On Sheets, every commit that writes `Health` also rewrites the `Health!H1` formula.
- A lead is added once, the first run it matches the lane (its `when` held and it is not blocked on every lane); a lead counts as listed when it, or any lead in its `merged_into` family, already has a row in that table, so a merge never lists one person twice. A run adds at most `ingest_chunk_rows` new rows in total across all export lanes (rubric order, then lowest lead id first; the rest are listed in later runs); refreshes of listed rows are never capped. Every run then recomputes `status` and `do_not_contact` for every row in every `Export *` table in the store (including tables of lanes since removed), following `merged_into` to the live lead, and upserts only rows whose values changed (`updated_at` changes only then).
- `do_not_contact` is `yes` when the live lead is blocked on every lane for any reason (unsubscribed, unresolved duplicate, rubric conflict, Overrides status conflict, unknown Overrides value); has any status that blocks cold lanes; is at a company with an open or won deal; has a manual status in Overrides that blocks cold lanes (read straight from Overrides, so it holds even in a run that did not fold); has already been contacted (any cold row that holds the cold push under section 8, across the `merged_into` chain, or `contacted_at` set); matches a cold lane (the lane's `when` held, whatever its other checks say and even before its sink is set up: a cold lane claims its leads, since loosening this would bring back a double contact once the sink pushes) or has an open cold row this run; or when its row's lead was merged into a lead that also has a row in the same table (more generally, when several rows of one table lead to one live lead, only one stays `no`: the live lead's own row, else the earliest listed, then the lowest lead id). Otherwise `no`. A run that did not judge the lanes on full inputs (it did not score: cut short, or phase 1 too large; or `Enrich` or `Detect` failed) adds no row and only ever turns `do_not_contact` to `yes`, never back to `no`.
- The export tables are not part of the phase 2 commit: right after phase 2, their changed rows are written in their own commits of at most 5,000 rows, rows turning `do_not_contact` to `yes` first (across all lanes), then every other change; each commit checks the lease, `ErrTooLarge` halves the chunk, and a lane's `export_lane:` record travels with its table's first chunk. So a mass switch to `yes` (a new cold lane claiming most of a list) can never make phase 2 too large and block every later opt-out. When one of these commits fails the run fails, keeping what was committed.
- Export lanes write no `Pushes` rows; the table is their once-only record. HubSpot and Apollo opt-out lookups run only for push candidates, so an export row reflects opt-outs from the receiver, polling and Overrides (the README says so).
- A CSV is rewritten only after phase 2 committed, only from the committed table, and never on dry-run: written to a temporary file in `export.dir` and renamed, UTF-8, header row in column order, every cell through `internal/csvsafe`, file mode 0600 (the lists hold personal data). It runs even when the `Ranked` write after phase 2 fails (only the CSV rewrite, not the rest of `AfterSave`), reading the tables under its own short timeout rather than the run's context, so a list never lags an opt-out phase 2 saved. Every table recorded in `State` gets its file (a recorded id that breaks the section 2 lane id rule is skipped and raises `export_lane_invalid:<lane id>`), and so does each export lane of the rubric (header only while nobody is listed) unless it matches a recorded lane only ignoring case. Leftover temporary files are removed first (only the names the writer makes, `.<lane id>.csv.<digits>.tmp`, for the lanes it writes; nothing else in the folder is touched); the rewrite stops 5 seconds before the lease runs out and checks the lease before each rename; and the folder is synced after each rename. An existing `export.dir` that other users can read raises the warning `export_dir_readable`.

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
| `Companies`, `Company facts` | about 5,000 each | about 10, 8 | 90,000 |
| Export tab | up to 20,000 | 11 | 220,000 |
| **Total** | | | **about 6.0 million of 10 million** |

`doctor` warns at 70% of the cap and names the largest tabs. Above about 1,500 events a day, the README points teams to SQLite or a shorter `log_retention`.

## 5. Receiver interface

### 5.1 HTTP

| Route | Purpose | Answers |
|---|---|---|
| `POST /apollo/visit` | Apollo website-visit workflow | 2xx once stored; 401 wrong or unset secret; 5xx past the 10-second hold cap |
| `POST /apollo/reply` | Apollo reply, sent and unsubscribe workflow | same |
| `GET /healthz` | Health check | With a timer (`--every`): 200 when the last run succeeded or none is due yet, 503 when the last run failed or none succeeded in three intervals (with no `last_success_at`, measured from `serve`'s start); read from the store and cached for 60 seconds (a finished run drops the cache, and a run that returned an error or panicked is 503 whatever the store holds). Without a timer (Cloud Run service): 200 unless the last `AppendEvents` failed |

- **Secret.** It travels in the `X-Leadscore-Secret` header, or in a top-level `leadscore_secret` body field when Apollo cannot send headers. The header is preferred: a body secret is only known once the body is read, so unauthenticated bodies are read at most 16 at a time, each within 3 seconds; a flood of slow senders can still delay body-secret requests (never header ones) into 503s that Apollo must retry. Every top-level copy of the field (and, in a body that is not valid JSON, every copy found in its text) is a candidate, and the request is accepted when any one matches. It is compared in constant time against `LEADSCORE_RECEIVER_SECRET` and, when set, `LEADSCORE_RECEIVER_SECRET_PREVIOUS`, and removed from the body before storing. The receiver counts as configured when `replies: receiver` or `receiver.visit_events` is non-empty. If its secret variable is empty, every POST gets 401 and `serve` logs a warning but still starts, so `/healthz` and the timer work. The `receiver-secret` check runs at `serve` start and in `doctor`, not inside runs.
- **Write queue.** Requests are gathered for 2 seconds and appended in one `AppendEvents` call whose context ends 10 seconds after the oldest request in the batch arrived; when it ends, every request in the batch gets 5xx.
- **Timer.** The first run starts when `serve` starts; the next starts `schedule` after the previous one ends. A run that panics is recovered; `serve` then writes `last_result=unhealthy`, `last_run_at` and `run_failed` to `Health` itself through `engine.RecordCrash` (section 12.6), under the run lease, reading only `State.schema_version` and `Health` (a newer-major store gets no write); when another run holds the lease it writes nothing, since that run writes `Health`. A panic while writing is recovered and logged.
- **Shutdown.** On SIGTERM `serve` stops its timer, closes the running run's `Stop` channel (the run then saves within its budget), keeps the receiver storing events until the run returns, drains the write queue, and exits.
- **Image.** The container runs as the non-root user `leadscore` with `HOME=/home/leadscore`. `leadscore healthz` calls the local `/healthz` and exits 0 on 200 (the compose health check).

**Body formats**, set by the workflow templates (`setup/apollo/`). Event names use core's names, so the copied parser reads them: `email_sent` is `sent`, `email_replied` is `replied`, `email_replied_positive` is `replied_positive`, `email_unsubscribed` is `unsubscribed`, and `website_visited_<name>` is `visit_<name>`. A visit body uses core's nested `contact.*` and `account.*` fields. A reply-workflow body carries core's flat fields (`contact_email`, `contact_stage`, `last_conversation_link`) plus `contact_id`, `contact_name`, `contact_title`, `contact_linkedin_url` and `account_domain`; S0 confirms which of these Apollo's workflow variables can fill, and any it cannot are left out of the template. S0 saves a real body of each kind in `testdata/events/`.

**Receiver rows.** Each identified request also yields an input row under source id and channel `receiver`, with columns `contact_id`, `email`, `linkedin_url`, `full_name`, `title`, `company.name`, `company.domain` (whatever the body carries); its row id is the contact id, else the email, else the LinkedIn URL. So `receiver` counts in `sources_seen` for every lead it touches, as core upserts the contact.

**Keep the secret private.** The README warns, in the setup steps and next to the Apollo workflow templates, that the receiver secret works like a password: anyone who has it can send fake events, including a fake positive reply that a deal lane would act on. It must never be pasted into chat, tickets or shared docs, and should be rotated if it might have leaked.

**Rotating the secret.** Move the current secret to `LEADSCORE_RECEIVER_SECRET_PREVIOUS`, set a new `LEADSCORE_RECEIVER_SECRET`, update each Apollo workflow, then remove the previous one. On Docker, edit `.env` and `docker compose up -d`. On Google Cloud, add the current value as a version of `receiver-secret-previous` and the new one to `receiver-secret`, and run `setup/gcp.sh redeploy`, which attaches the previous secret while it has an enabled version; after updating each Apollo workflow, `setup/gcp.sh redeploy --finish-rotation` detaches it, and only then are its versions disabled (S0 confirms that a disabled version still attached stops a new instance from starting, so the order matters). `doctor` warns while a previous secret is still attached. An Apollo or HubSpot key is rotated by adding a version to its secret; `setup/gcp.sh redeploy` attaches a key added after the first deploy.

### 5.2 Event rows from a CSV or Sheet source

A source marked `events: true` carries one event per row: `event` (the kind), `at` (RFC 3339, or `YYYY-MM-DD` read as UTC midnight; the `at` aliases apply), one of `email`, `linkedin_url` or `domain`, and any extra columns as `Attrs`. A row may carry only `visit_*` or a custom kind. Event rows are events, not input rows: they do not add the source to a lead's `sources_seen` (receiver requests do, through their receiver row). The kind is lowercased; a kind with any character other than `a-z`, `0-9` and `_` is rejected, so look-alike or invisible characters cannot pass the forbidden check. A row with an empty or such a kind, a forbidden kind (`sent`, `replied*`, `unsubscribed`, `reply`, `optout`, `deal_*`), no parseable `at`, or no key is returned with `Kind` empty and `Attrs["reject"]` set to the reason; the engine logs it as `row_rejected`. A reject reason names the line, never a cell value, so the log carries no email. A file with no `at` column, or with none of the `email`, `linkedin_url` and `domain` columns, fails the source's `Fetch` instead of rejecting every row. Columns are found by their section 2 names (the first header in file order wins), so `Domain` comes from any `company.domain` spelling and is never derived from an email. An Apollo visitor export with no `event` column is read as `visit_<source id>`, lowercased with any character outside `a-z`, `0-9` and `_` made `_` (source `My-Site` gives `visit_my_site`). The extra columns go into `Attrs` under the same names (an unaliased header under its squashed name, or its trimmed text when it squashes to nothing; `company.name` as `company`), empty cells left out; a column named `reject` is ignored so a file cannot mark its own rows.

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
- **No deal.** A deals-by-company lookup that finds no open or won deal for a company it was asked about emits `deal_lost` with the company's domain, no `deal_id`, and `At` the lookup time. That releases a company the ledger holds through a `deal` step that timed out (it has no deal id to look up). The engine stamps every lookup deal event with the run's clock, so `deal_checked_at` is the lookup time. HubSpot's search can lag a fresh create, so the engine does not trust a `deal_lost` for a company with a `deal` step last called less than 15 minutes ago that has no deal id back: it drops the event and the company's live leads wait this run, as if their lookup failed (section 8; S0 confirms the lag).
- **Polling mode.** With `replies: polling`, receiver `replied` and `replied_positive` events are keyed in `Seen events` but have no effect: no outcome, no window row, no lead created and no receiver row merged (`sent` and `unsubscribed` still apply), so one reply never counts twice. Polled events always carry `Origin` `polling`, whatever the poller set.
- **Opt-outs reach every holder.** An opt-out (`unsubscribed`, `optout`, a polled `unsubscribe` label) is written on the raw owners of the event's identity keys: the lead each key names as stored (the receiver contact's `Applied rows` lead, the lead of its well-formed email and of its LinkedIn URL in `Identities`), before following `merged_into`; on the resolved live lead only when no key has an owner; and, for a lead in a hand-edited `merged_into` cycle, on every member of the cycle (and every lead merged into one), so fixing a cycle by hand never drops it. The fold reads opt-outs across the merge family, so while merged the whole family is blocked; undoing a wrong merge leaves the opt-out with the person who opted out and the other person contactable. An owner outside the resolved lead's family is logged as `key_conflict` with lead ids only, and only when it changed that lead's outcome. Wrongly not contacting someone is acceptable; contacting an opted-out person is not. Replies and visits stay on the resolved lead.
- **Sources never report vendor kinds.** `Intake` refuses `sent`, `replied*`, `unsubscribed`, `reply`, `optout` and `deal_*` from any source (`row_rejected`), whatever its `Origin`.
- **Lookup events** are not de-duplicated through `Seen events`; applying them is idempotent.
- **Silence detection** runs only when the receiver is set up (`receiver.public_url` is set). It expects the `receiver.visit_events` kinds, and `sent` when `replies: receiver`; silence is measured from the later of `State.last_received:<kind>` and `State.first_run_at`, against `silence_threshold`. `Intake` never stores a `last_received` later than the run's clock (and replaces a stored one that is), and the check reads one later than now as never heard (silence is then measured from `first_run_at`).

### 5.4 Oversized bodies

The receiver decodes the JSON and shortens every string over 16KB with a `__truncated_fields` marker (core's caps). If the body is still over the 50,000-character Sheets cell limit, it keeps only the fields the parsers read, at their original paths: `apollo.RequiredPaths` lists all 19 (event; reply workflow: `contact_email`, `contact_stage`, `last_conversation_link`, `contact_id`, `contact_name`, `contact_title`, `contact_linkedin_url`, `account_domain`; visit workflow: `visited_at`, `contact.id`, `contact.email`, `contact.linkedin_url`, `contact.first_name`, `contact.last_name`, `contact.title`, `contact.company`, `account.domain`, `account.website_url`, `account.name`). The name, title and company fields are kept too because they fill the receiver row, so a cut-down body still updates the lead, and adds `"__dropped_reason"`. A body that is not JSON (or not a JSON object) is stored as `{"__not_json": true, "raw": "<first 16KB>"}`. The receiver reads at most 1 MB of a request; a larger one is answered 413 and not stored. Nothing is decoded before the secret is checked: a header secret is checked before the body is read, and a body secret is found by scanning only the object's first level (or, when the body is not valid JSON, by matching the `"leadscore_secret":"..."` text). Every `leadscore_secret` key (any case, any depth) is removed before storing, a configured secret inside any key or value is masked, and in a body stored as text every configured secret (as written or in its JSON-escaped forms) and any such field's value is masked. A body with anything after its JSON object is not JSON. The list of cut paths caps each path at 128 characters and is dropped before any parser field.

### 5.5 Reply-label map (polling)

| Apollo label | Status |
|---|---|
| `willing_to_meet` | `replied_positive` |
| `unsubscribe` | `unsubscribed` (fixed) |
| `not_interested` | `replied_negative` |
| `follow_up_question`, `person_referral`, `already_left_company_or_not_right_person`, `none_of_the_above` | `replied_neutral` |
| `out_of_office` | no outcome |
| no label | `replied_unlabelled` |

Teams override every row except `unsubscribe` with `reply_labels`. Two polled replies at the same received time are ordered by message id, then label. A polled reply's `At` and received time are Apollo's reply time (else its send time; S0 confirms the fields), so a relabelled reply keeps its place in time.

## 6. Vendor fields

**Apollo.**

- **Client.** `adapters/apollo/client.go` has two call modes: a retrying call (Retry-After, used by enrichment) and a single-shot call that maps 429 to `ErrRateLimited` with no sleep (used by sinks, lookups and the poller).
- **Funding stage.** The enricher writes one of `pre_seed`, `seed`, `series_a`, `series_b`, `series_c`, `series_d_plus`, or leaves it empty for other labels. Region is the vendor's country, trimmed.
- **Checks.** The `apollo-key` check uses Apollo's free auth-health call (S0 confirms it), never an enrichment call. The `apollo-sequences` check confirms `mailbox_id` is in the email-accounts list and every lane's sequence name resolves.
- **Sequences.** `apollo:sequence/<name>` names a sequence; the sink resolves names to ids once per run. An unresolved name makes `Do` return `ErrTransient`, so the step waits until the name is fixed.
- **Refusals.** `ErrRefused`: contact active in another sequence, opted out, or invalid email. Until S0 confirms Apollo's answers, the sink reads the contact and searches the team's contacts by the lead's emails before enrolling: already in this sequence is a no-op; the contact opted out (`email_unsubscribed`, whatever `ContactOptOutFlag` says) or in any other sequence (paused or finished too), or any duplicate contact opted out or in any sequence, is refused. Refusals are read only from a skip reason or an error reply's `error` and `error_code` fields, never the rest of a body (which may echo the contact). A skip whose reason the sink does not recognise, or an add reply that neither lists nor skips the contact, counts one attempt (a skip enrolled nobody, so a retry cannot contact twice).
- **Errors.** 429 is `ErrRateLimited`; so are 401 and 403 (a refused key, or one that is not a master key: no lead is at fault, so the sink stops for the run with no attempt counted, and the `apollo-sequences` check raises `apollo-sequences:key`); 5xx, timeouts and network failures are `ErrTransient`; others count an attempt. A search page that comes back full with no `total_pages`, or a search over its page limit, is an error, never read as complete.
- **Budgets.** Every enrichment call made counts toward both budgets. A domain is looked up when `enriched_at` and `not_found_at` are both empty or older than `max_age`, and no lookup of it failed in the last 24 hours (`enrich_failed_at`). Never-tried domains go first, then the least recently tried. A personal mail provider's domain, or a name with no dot, is never looked up. Three failed lookups in a row stop the run's enrichment and raise the warning `enrich_failed`; so does any failed lookup. A rate limit stops it too, and is not a failure.

**HubSpot properties** created by `leadscore setup hubspot`. The `leadscore_` prefix is configurable; properties go in a group named `leadscore`.

| Object | Property | Type | Use |
|---|---|---|---|
| Contact | `leadscore_lead_id` | text, unique | the contact sink searches by this first, then by email, so a crash followed by an email correction never creates a second contact |
| Contact | `leadscore_lane`, `leadscore_tier`, `leadscore_priority`, `leadscore_reasons` | text | context for salespeople; set on create only |
| Contact | `leadscore_score` | number | context for salespeople; set on create only |
| Deal | `leadscore_company_domain` | text, not unique (one open deal per company is enforced by the ledger and `Related`) | finding the company's deal |
| Deal | `leadscore_lane` | text | which lane opened it |

- **Destinations.** `hubspot:contacts` has steps `{contact}`; `hubspot:deals` has `{contact, deal}`, with the association made inside the deal step. Deals are named `<company domain>`. Only deals at an open stage are reused; a won or lost deal is never reused.
- **The deal step** looks in three tiers, in order, reusing the first deal it reads as open: (1) the deals the engine knows: another lead's done deal step in `Related`, the deal a deal step settled earlier in the run (this covers a lead of the same family, which `Related` leaves out), and `CompanyDealID`; (2) deals associated with the lead's contact, unless there is evidence one is another company's: a non-empty domain property naming another domain, or links only to company records of other domains (an empty domain counts: a salesperson's deal on the person; this tier also catches a retry of a call that timed out after HubSpot created the deal, since `Related` is empty then and the association read does not lag, S0 confirms); (3) deals found by the domain property. Domains compare as merge normalizes them (lowercase, trimmed, no scheme, `www.`, port or path), on both sides. Only then does it create one, with the name, pipeline, stage, domain property and the contact association in the same call. A candidate at a stage no pipeline lists makes the step wait (`ErrTransient`) instead of creating, and the `hubspot` check raises `hubspot:unknown_stage` while any deal step waits so. Any read that fails stops the step before a create.
- **The lookup** reads, for its leads: each email of the lead's family by batch read (`hs_email_optout`; S0 confirms whether a read by email also matches secondary addresses: an answer under an address not sent makes the batch's unmatched emails be read one by one), and the lead's own contacts: by `leadscore_lead_id`, and by the contact ids in `LeadRef.Done` (a contact merged in HubSpot answers with the survivor, S0 confirms); an opt-out on the lead's own contact is reported under the lead's primary email. For each company (lead domain): the deals of the companies its contacts belong to (or, when none, the company found by `domain`), the deals carrying the domain property, the deals linked to its contacts unless shown to be another company's (by the same evidence rule as the deal step's second tier: an empty domain counts), and `CompanyDealID` read by id. A read that fails, or a search with more pages than it follows, fails every lead it was for; a company any such read touched gets no deal event. When every lead failed, the whole lookup fails, unless it learned opt-outs, which it returns (with every lead failed). A stage no pipeline lists counts as open.
- **Test keys.** `base_url` is refused without `_http_client`, so a file never sends the token to another host. Search calls are spaced 250 ms apart against the real API (S0 confirms HubSpot's search rate limit).
- **Errors.** 429 is `ErrRateLimited`; so are 401 and 403 (a revoked token or a missing scope: no lead is at fault, so the sink stops for the run with no attempt counted, and the `hubspot` check raises the problem); 5xx and timeouts are `ErrTransient`; 400 invalid email (`INVALID_EMAIL`, S0 confirms the code) is `ErrRefused`; others count an attempt. Redirects are not followed, and an answer over 16 MB is `ErrTransient`. On 409, the existing contact id is parsed from the message, else found by email search.
- **Deal stages.** Open, won or lost comes from the stage's closed and probability metadata in the pipeline.
- **Opt-out reads.** HubSpot: the contact's `hs_email_optout`. Apollo: the opt-out flag on the contact record found by email. S0 confirms both names, and that the Apollo lookup costs no credits; if Apollo has no such flag, the Apollo `Lookup` is not registered and the `apollo-key` check warns Apollo-only teams.

## 7. Status precedence

Statuses are recomputed each run from `Outcomes` and `Overrides` by taking the first rule that applies. Every input is saved in `Outcomes` when first learned, so no status depends on events that are later trimmed. The fold reads `Outcomes` across the lead and every lead merged into it: the earliest `unsubscribed_at`, the latest reply, any `contacted_at`, and any deal.

1. **Unsubscribed.** `unsubscribed_at` is set: by any `unsubscribed` or `optout` event, a polled `unsubscribe` label, or a manual `unsubscribed` row. Automation never clears it. The fold sets origin `manual` only when `unsubscribed_at` is empty. A `resubscribe` row clears it only while the origin is `manual`, applies once, and is logged.
2. **Blocked.** A lead with two or more different status rows in Overrides, under any of its identity keys, or with an unknown Overrides value, has status `blocked`: blocked on every lane and shown in `Health` until fixed.
3. **Manual.** The lead's one status row in Overrides wins. Removing the row releases it. An override for a person not yet known waits until that person appears; one still unmatched after a run shows as `override_unmatched` in `Health`.
4. **Deal.** Any lead at the company has a deal at an open or won stage, or the `deal` step of a `hubspot:deals` lane at the company has `called_at` set, or an `intent_run` left by another run (the deal may exist even if the call timed out; the `contact` step cannot create a deal and never counts, and a step this run marked but has not called is no deal yet), until a lookup shows the company has no open or won deal. A closed-lost deal releases it. A ledger step is released by a `deal_lost` whose `deal_checked_at` is later than the step's `called_at`; a lookup event with no time takes the run's clock, so `deal_checked_at` is the lookup time.
5. **Reply.** `reply_status`: the latest automated reply by received time.
6. **Contacted.** `contacted_at` is set (a `sent` event or a completed cold push).
7. Otherwise `new`.

**Matching Overrides.** `person` is normalized like Identities (emails lowercased and trimmed; LinkedIn URLs canonicalized) before matching.

**The CLI writes Overrides the same way on both stores.** The row's `person` is the lead's primary email, else LinkedIn URL, else lead id. `set-status <person> <status>` replaces the lead's status rows under every one of its identity keys with one row; `set-status <person> none` deletes them; `set-status <person> resubscribe` deletes them, including a manual `unsubscribed`, and writes a `resubscribe` row with the request time in `note`. A `resubscribe` row is not a status row for rules 2 and 3. An explicit status also deletes a `resubscribe` row still waiting for the lead. If a hand edit leaves both a manual `unsubscribed` row and an unapplied `resubscribe` row for one lead, the fold applies the `resubscribe` first and then reads the status rows, so the lead stays unsubscribed and the `resubscribe` is used up (logged as such): an opt-out is never reopened by a stale instruction. `merge` appends a `same_as` row and refuses unless both persons are known leads (a merge is permanent); `mark-distinct` appends a `distinct` row, and `set-status` and `mark-distinct` may name a person not yet known (the row waits). `retry` appends a `retry` row.

**A `same_as` merge.** The survivor is the lead with the older `created_at` (ties: lower lead id); the other gets `merged_into`. The survivor's `fields` are filled from the absorbed lead by fill-if-empty, with disagreements added to `conflicts`; `apollo_held_at` and `first_seen` take the earliest values; the absorbed lead's identities, `Outcomes` and ledger rows stay under its id and count for the survivor (the fold above, and section 8).

**Worked example** (an install with `replies: receiver`, a HubSpot deals lane, and the Apollo lookup). Priya gets an Apollo `sent` event from the receiver and becomes `contacted` (rule 6). Her `replied_positive` event arrives and she becomes `replied_positive` (rule 5). The warm-handoff lane opens a deal at her company, so she becomes `deal` (rule 4). A later `replied` event updates `reply_status` but leaves her at `deal`. She then clicks an unsubscribe link without replying; the pre-push Apollo lookup (which reads the leads about to be pushed, here by a non-cold lane for `deal` leads) returns `optout`, so she becomes `unsubscribed` (rule 1) for good.

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
- **One deal per company.** A row is a deal row when its step is `deal` and its destination is `deals`, whatever its lane says now, so editing or removing a deals lane never drops a company's hold. Within a company, a `deal` step waits (stays pending, not cancelled, not called) while another lead's `deal` step there was called and has no result yet (no deal id; pending, or cancelled after its call) and no lookup has read the company since that call. It is retried in a later run, once the first has its deal id (passed in `Related`, so the sink reuses that deal) or a lookup has the company's answer.
- A lookup that failed for some leads releases none of their companies: its `deal_lost` for a domain with a failed lead is dropped.
- Nor does a `deal_lost` release a company with a `deal` step whose latest call was less than 15 minutes ago (`called_at` keeps the first call, so the latest is read from `updated_at`) and no deal id back (HubSpot's search may not show that deal yet): the event is dropped, and the company's live leads wait this run as if their lookup failed, logged as `lookup_failed`. A later run's answer, past the window, releases it.
- Reselecting a never-called cancelled row moves it to the lane's current destination.
- A cold lead with a `pending` row stays in that lane: it never moves to another cold lane while the row is open.
- Holding rows count for the lead itself and every lead whose `merged_into` chain ends at it. Non-cold rows never affect the cold push.
- **A lane that changes under its rows.** A row counts as cold when its stored `lane_kind` or its lane's current kind is cold; a row used for, or loaded under, a cold lane is stored as cold and never downgraded, so changing a lane's kind never releases a cold push. A pending row whose lane's destination changed moves to the new destination when never called (no `called_at`, no `intent_run`); a called one is cancelled at load (it holds the cold push if cold) and the lane does not push that lead again, since the vendor may already hold the person at the old destination.
- A cold lane whose open rows were never called (no `called_at`, no `intent_run`) and that now fails a cancelling check does not hold the lead: its rows are cancelled and the lead falls through to its next matching cold lane in the same run.
- The deal lookup's one-lead-per-company set also covers companies held only by a `hubspot:deals` step in the ledger (one that timed out has no deal id), so a lookup can release them.

## 9. Setup runbooks

The README follows these steps in order (RFC section 6.14). It says up front that Google Cloud is the path for non-technical teams and the Docker paths are for technical users. Each step is marked as something the agent runs or something a person must do.

**The setup script** is `setup/gcp.sh` (bash with `gcloud`, run from macOS, Linux or Google Cloud Shell), one subcommand per step: `accounts`, `bucket`, `secrets`, `deploy`, `redeploy`, `schedule`. It reads and writes `leadscore.yml` only through `leadscore config get` and `config set-hosting`. The Sheet step is the Go command `leadscore setup sheet`, which the script calls.

**Credentials.** `leadscore setup sheet` runs with the person's own Google login, so the person owns the spreadsheet: it takes their token from `gcloud auth print-access-token`, so the person first runs `gcloud auth login --enable-gdrive-access` (Sheets accepts the Drive scope). It shares the spreadsheet with `hosting.run_account` and `hosting.receiver_account` (a bare name gets `@<project>.iam.gserviceaccount.com`), or on Docker with the one account in `store.credentials`. Every other local command acts as the run account through Application Default Credentials made with `gcloud auth application-default login --impersonate-service-account=<run account>`, which `setup/gcp.sh accounts` runs at its end.

**Roles.**

| Account | Roles, on which resource |
|---|---|
| Run account | Secret Manager Secret Accessor on the key secrets, `leadscore-config` and `leadscore-config-version`; Secret Version Adder on `leadscore-config` and `leadscore-config-version`, and Secret Manager Viewer on `leadscore-config-version` (for `config push`, which checks it exists before uploading and refuses a bundle holding a stored API key; the run account cannot read `receiver-secret`, so a receiver secret pasted into the bundle is not detected); Storage Object Admin on the lease bucket; Cloud Run Viewer and Cloud Scheduler Viewer on the project, and Artifact Registry Reader on `ghcr-proxy` when it exists (for `doctor`); editor on the spreadsheet |
| Receiver account | Secret Manager Secret Accessor on `receiver-secret`, `receiver-secret-previous` and `leadscore-config`; editor on the spreadsheet (`Events` tabs protected for it) |
| Scheduler account | Cloud Run Invoker on the job `leadscore-run` |
| The person | the roles to create the above, plus Service Account Token Creator on the run account |
| Before release only: the team project's Cloud Run service agent | Artifact Registry Reader on `leadscore-dev`'s repository `leadscore`, to pull the private image (granted by a `leadscore-dev` owner) |

**The image.** Releases publish `ghcr.io/tetriz-ai/leadscore`. Cloud Run cannot pull from GHCR directly, so `setup/gcp.sh deploy` creates an Artifact Registry remote repository `ghcr-proxy` in the team's project pointing at `ghcr.io` and deploys through it. Before release, the image is the private registry's (`asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore`), passed to `deploy` directly.

### 9.1 Google Cloud

1. Prerequisites: a paid Apollo plan (for workflows and the API), `gcloud` installed and logged in, and the person holding the roles to create projects, service accounts, Cloud Run services and jobs, Cloud Scheduler jobs and Artifact Registry repositories, and to grant roles on them.
2. Install the `leadscore` CLI: the release binary for the person's OS, or `docker run --rm -v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro -v "$PWD":/config <image>`.
3. Create or pick a Google Cloud project with billing (a person approves billing).
4. Enable the Cloud Run, Cloud Scheduler, Secret Manager, Cloud Storage, Sheets, Drive, Artifact Registry and IAM Service Account Credentials APIs.
5. `setup/gcp.sh accounts` and `bucket`: create the run and receiver accounts, write the `hosting` block, create the lease bucket (`store.lease_bucket`, by default `<project>-leadscore-lease`), and finish with the impersonated login. Each role in the table above is granted in the step that creates its resource: project roles in `accounts`, the bucket's in `bucket`, the secrets' in `secrets`, `ghcr-proxy`'s in `deploy`, the job's in `schedule`, the spreadsheet's in `setup sheet`.
6. `leadscore setup sheet`, with the person's own login: create the spreadsheet from the section 4 schema (the template is code), including the current month's `Events` tab; share it with both accounts; protect the `Events` tabs for both accounts and every other tool tab for the run account only; set hourly recalculation; write the staleness formula; write `store.spreadsheet`. The template hides the machine-data tabs, colors `Ranked` and `Health`, gives the people tabs (`Leads`, `Companies`, `Overrides`) 1,000 rows, notes on their headers and a green color, puts a note and a red highlight (while it reads `STALE`) on `Health!H1`, and turns off re-sharing by editors (`writersCanShare`). `--view` makes `Ranked`, `Health` and one `Export <lane id>` tab per export lane in the rubric. If a Workspace sharing policy blocks the share, it reports Drive's error and the README names the exception to ask the admin for.
7. `setup/gcp.sh secrets`: create the secrets (including `leadscore-config` and `leadscore-config-version`) and add the API keys and the receiver secret (a person pastes each key).
8. Write the rubric, run `setup hubspot` if HubSpot is used, and upload the bundle with `leadscore config push`.
9. `setup/gcp.sh deploy <image>`: the receiver service (receiver account, minimum 0 and maximum 1 instance, sign-in check skipped, receiver secrets and bundle attached) and the run job (run account, `leadscore run`, task timeout the deadline plus the save budget, maximum retries 0, key secrets, bundle and `LEADSCORE_CONFIG_VERSION` from `leadscore-config-version:latest` attached).
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
| `hosting` (only when `hosting` is set) | service, job, scheduler job or `ghcr-proxy` missing; service not at most 1 instance; job task timeout not equal to the deadline plus the save budget, or retries not 0; deadline plus save budget not below `schedule`; scheduler account unable to run the job; the version secret's latest value differs from the bundle's latest version (an interrupted push); the job not reading `LEADSCORE_CONFIG_VERSION` from it; the service or job running as another account; the receiver service without `LEADSCORE_RECEIVER_SECRET`; anyone (`allUsers`, `allAuthenticatedUsers`) allowed to run the job. Warnings: the latest `leadscore-config` version differs from `State.config_version` (no run has started since the push); the previous receiver secret still attached; the scheduler job paused | `setup/gcp.sh` the missing step, or `leadscore config push` |
| `rubric-version` (doctor only, warning) | `Health.rubric_version` from the last run differs from the local rubric's version | `leadscore config push`, or redeploy the matching file |
| `sheet-access` (in run) | an account cannot open the Sheet (not shared, or blocked by a Workspace sharing policy): this account cannot open it (`sheet-access:<spreadsheet id>`), or on Google Cloud the run or receiver account is not among its editors (`sheet-access:<account>`) | share it, or ask the Workspace admin for an exception |
| `sheets` (in run) | spreadsheet not set to recalculate hourly (`sheets:recalc`) or not on UTC (`sheets:timezone`), either of which leaves the `Health!H1` formula wrong; cell use past 70% (`sheets:cells`, a warning naming the largest tabs) | `leadscore setup sheet --repair`; shorten `log_retention` or move to SQLite |
| `hubspot` (in run; only with `sinks.hubspot` and the token set) | token missing the scopes (`hubspot:scopes`); custom properties missing or of the wrong type (`hubspot:properties`); pipeline or stage not found, not set while a lane pushes to `hubspot:deals`, or a closed stage (`hubspot:pipeline`); a deal step waiting on a deal at a stage no pipeline lists (`hubspot:unknown_stage`, read from the ledger, also with no token); the block unreadable (`hubspot:config`) or HubSpot unreachable (`hubspot:api`) | re-create the private app; `leadscore setup hubspot` |
| `apollo-key` (in run) | Apollo refuses the key on the auth-health call (401, 403, or `is_logged_in` false: `apollo-key:auth`); the call gets no answer (429, 5xx, network: the warning `apollo-key:unreachable`); Apollo has no opt-out flag (the warning `apollo-key:no_optout_flag`, for teams with `sinks.apollo` and no `sinks.hubspot`, until an unsubscribe webhook has been received (`State.last_received:unsubscribed` set: real evidence Apollo's unsubscribe workflow reaches the receiver); the enrich or `sinks.apollo` block cannot build a client (`apollo-key:config`, for example `base_url` without a test client) | check the key |
| `apollo-sequences` (in run) | mailbox id not found; a lane's sequence name missing or ambiguous; the key refused for sequences (401 or 403: `apollo-sequences:key`, not a master key; S0 confirms); a call with no answer (429, 5xx, network) is the warning `apollo-sequences:unreachable` | check `sinks.apollo.mailbox_id` and the sequence names; use a master key |
| `receivers` | with `replies: receiver`, `receiver.public_url`'s `/healthz` unreachable | re-deploy; on a laptop, `replies: polling` or a Cloudflare Tunnel |
| `receiver-silence` (in run; only when `receiver.public_url` is set, so an install that never set up a receiver is not flagged) | any expected event kind silent past the threshold (raises `silent:<kind>`; section 5.3) | check the Apollo workflow |
| `lease` | on Sheets, the lease bucket missing or not writable. A held lease is shown with owner and expiry as a warning; it clears itself at expiry | `setup/gcp.sh bucket` |
| `pushes` (in run) | failed steps (`push_failed:<lead>:<lane>:<step>`), or steps pending more than 24 hours (`push_pending`, with the count, a warning per section 11); rows of merged leads are left out | `leadscore retry`, or a `retry` row in Overrides |
| `store` (in run) | store from a newer major version; a SQLite store or CSV path while `CLOUD_RUN_JOB` or `K_SERVICE` is set; ledger below `ledger_rows` (`ledger_shrank`; pushing is blocked); SQLite on a disk that is not kept (its mount is overlay or tmpfs), or opened outside a container while `opened_by` names one | install a matching version; restore the ledger rows; use a named volume and `docker compose exec` |
| `rubric` (in run) | fails to compile; a rule reads a field that is not built in, declared, or a loaded column | fix the file |
| `overrides` (in run) | unknown value in Overrides; conflicting status rows (`status_conflict:<lead>`); a row naming no known person is a warning (`override_unmatched:<row>`, the row's position); a `retry` row naming a lane the rubric does not have is a warning (`override_unknown_lane:<row>`) | fix the cell |
| `pushes-enabled` | still off (a warning) | review `--dry-run`, then set `pushes_enabled: true` |
| `duplicates` (in run) | unresolved namesakes (`namesake:<lead>`); leads in a hand-edited `merged_into` cycle (`merge_cycle:<lead>`, blocked on every lane); and `key_conflicts` (a warning), with counts | resolve in Overrides (`distinct` or `same_as`) |

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
| Wait after a failed enrichment lookup, failures in a row that stop enrichment | Us, fixed | code | 24 hours, 3: a domain that always fails cannot take the budget from the rest |
| Deal search-lag window | Us, fixed | code | 15 minutes: a no-deal answer this soon after a deal call is not trusted (S0 confirms HubSpot's lag) |
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
| `internal/csvsafe` | S10a | `Cell` and `Row`: quote a CSV cell a spreadsheet would read as a formula (starting with a tab or a carriage return, or whose first non-space character is `=`, `+`, `-` or `@`, unless it parses as a number); `ranked --csv` and the export CSVs (S13) use it |
| `internal/duration` | S1 (moved out of `internal/config` by S2) | `Parse`: Go durations plus `d` for days, shared by config and the rubric |
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
| `internal/fakes/sink` | S10b | an in-memory find-or-create sink and lookup for engine and `sinktest` tests |
| `internal/hosting` | S14b | Google Cloud: the section 3 resource names, the cron conversion, Secret Manager reads and writes (`config push`, keys on a hosted install), the reads the `hosting` check makes; its client honours `base_url` and `_http_client` like a vendor block |
| `internal/fakes/gcp` | S14b | importable fake of Secret Manager, Cloud Run, Cloud Scheduler and Artifact Registry |
| `internal/e2e` | S17 | the end-to-end suite |
| `adapters/apollo` | S8 owns `client.go` (key, base URL, 30-second timeout, the two call modes; 12.8) and registration; S9 adds the body parsers, `PolledReplyKey` and `RequiredPaths`; S12 adds sinks, the `Lookup` and the `Poller` | |
| `adapters/hubspot`, `adapters/csv`, `adapters/sheetsource` | S11, S7, S5 | `adapters/hubspot` also registers the `hubspot` check (it imports `internal/check`), and `internal/cli` imports it for `setup hubspot`, so every build registers the HubSpot sink, lookup and check: a custom adapter registered as `hubspot` panics as a duplicate |

**Built-in stores** register through a blank import in the root package (`leadscore.go`): their packages are internal, so a custom build could not import them, and this way every build has them.

**Import rule.** Inside the module, only `cmd/` and `_test.go` files import the root package; `internal/*`, `adapters/*`, `storetest` and `sinktest` use `internal/api`, whose names the root aliases (so `api.Backend` is `leadscore.Backend`). A root import from anywhere else becomes an import cycle once the root reaches that package; a test in the root package enforces the rule. Code blocks in section 12 therefore write `api.X`.

**Conformance tests** call `storetest.Run` and `sinktest.Run` from an external test package (`package sqlite_test`, `package apollo_test`), never from inside the package under test.

### 12.2 The in-memory model (S4)

One Go struct per section 4 table, with typed fields for known columns and an `Extra map[string]string` for unknown ones (`Ranked`'s non-fixed columns are its derived names, held in `RankedRow.Derived`). Keyed tables are held in maps by primary key; keyless tables (`Overrides`, `Log`) are ordered slices. Go map keys cannot be slices, so the key type is `model.Key`: the key column values in section 4 key order, joined (`model.K(parts...)`, `Key.Parts()`); a one-column key is the value itself (`m.People[model.Key(id)]`). Export tables are `Model.Exports[<lane id>]`; the people-owned `Companies` tab is loaded read-only as raw rows (`Model.Companies`). Two indexes: identities by key (the `Identities` map) plus `Model.IdentitiesOf(lead)`, and people by company domain (`Model.PeopleAt(domain)`, reading `People.fields["company.domain"]`).

- Every change goes through `Model.Put(table, row)`, `Model.Delete(table, key []string)` and `Model.Trim(table, column, before)` (retention: removes the rows now and records an `OpTrim`), which record what changed. A row put back unchanged records nothing. `Put` returns an error and records nothing for an `Export <lane id>` table whose lane id breaks section 2's rule (letters, digits, `-`, `_`, starting with a letter or digit) or matches an already recorded `export_lane:` id only ignoring case (two such lanes would share one SQLite table); `Delete` and `Trim` on such a table do nothing. A row of the wrong type for its table panics. `Put` keeps the row as the store will return it (times in UTC to the millisecond, JSON numbers as float64, exact up to 2^53), and the first row put in an `Export <lane id>` table also sets `State` `export_lane:<lane id>` to `yes`. That record must be committed in the same commit as the table's first write; `Encode` guarantees it: encoding an export table without `State` adds that one `State` row. `Model.SetState(key, value)` and `Model.StateValue(key)` are shorthands for `State`.
- `codec.Encode(model, tables ...string) []TableWrite` turns the recorded changes for the named tables (all when none named) into writes: `OpAppend` for new rows of `Seen events`, `Window events`, `Log` and `Identities`, and new `Overrides` rows (an append of a key a keyed table already holds fails the commit, C1, so `Encode` appends only keys not in the store); `OpUpsert` for new or changed keyed rows (including `Applied rows`); `OpDelete` for deleted rows (by key, or all columns for `Overrides`); `OpReplace` for `Ranked` (the whole table; `codec.Chunk(write, size)` splits it into a first chunk and later appends); `OpTrim` for retention, first among a table's writes. For `State`, a table name may carry a key prefix (`State:cursor:`).
- `Model.Committed(writes)` runs after `Commit` succeeds: the model takes what the writes stored as committed and compares every row they touched with its current value again, so a change made between `Encode` and `Committed` (even back to the old value, or a delete of a row the write added) stays recorded. `Model.Discard()` drops all uncommitted changes (dry-run, and `ErrTooLarge` reloads).
- `codec.Load(ctx, backend)` reads `State` first and refuses a newer major `schema_version` with `codec.ErrNewerSchema` (a value that is not digits `.` digits is `codec.ErrBadVersion`) before decoding anything else. It then reads every table except `Log` and `Events`, plus `Companies` and the export table of every lane recorded as `export_lane:<lane id>` (the `Backend` cannot list tables). It records this binary's version when the stored one is missing or older; a newer minor is kept. Load errors name the table, row and column, never the cell value. Times are `model.TimeFormat`; `model.FormatTime` and `model.ParseTime` read and write it.
- No slice writes a table any other way.
- `storetest` partitions: the suite appends old events with an old `RawEvent.ReceivedAt`, so a store that keeps one partition per month picks the month from `ReceivedAt` (the receiver's clock).
- A store that deletes partitions must survive a crash between `DeleteProcessed` and the engine saving the cursor it returned: the next `ReadEvents` from the older cursor must not fail on the deleted partition. The Sheets store records each deleted month in the same request as the delete (section 4, "Sheets events").

### 12.3 The evaluator (S2)

- `rules.Compile(yaml) (*Rubric, error)`. On failure the error is `rules.LoadErrors`, a list of `LoadError{Line, Field, Msg}` in line order (the caller adds the file name); compiling continues past an error where it can, so `rules check` reports them all.
- `rubric.Evaluate(in Input) Result` and `rubric.EvaluateContext(ctx, in) (Result, error)` (a cancelled `ctx` stops it before the next company or lead, interrupts a running condition, and returns ctx's error). `Input` carries leads (as `LeadRef`, whose `Status` is the folded status), company facts and `leads_seen` per domain, and `rules.DetectorResults` (per subject, per detector). `Result.Blocked` names leads stopped by `conflicts`, with the reason. `Result.Lanes` holds, per lead, the lanes whose `when` holds, highest priority first (file order on a tie), after derive and score; the built-in lane checks, the ledger and limits are S10b's. `Result.Warnings` (for example an unparseable number, or a lane expression that failed) are written to `Log` by the engine; they never carry a lead's values. The evaluator computes nothing merge produces; S3 builds `Input` directly from fixture rows.

```go
type Input struct {
    Leads     []api.LeadRef
    Companies map[string]api.CompanyFacts // by domain; a lead's domain with no entry is a company with only its domain
    LeadsSeen map[string]int              // company.leads_seen by domain; missing means no value
    Detectors DetectorResults
}
type DetectorResults struct {
    Leads     map[api.LeadID]map[string]bool // lead-subject detectors that fired
    Companies map[string]map[string]bool     // company-subject detectors that fired, by domain
}
type Result struct {
    Verdicts map[api.LeadID]api.Verdict
    Blocked  map[api.LeadID]string
    Lanes    map[api.LeadID][]string
    Warnings []string
}
```

- Accessors: `Fields() []string` (every input field the rubric reads: lead fields by name, company facts as `company.<name>`; derived names, rollups, `status` and detectors are not listed), `Aliases() map[string]string` (squashed header to field name, `company.<name>` for a company field), `ConflictFields() []string`, `Lanes() []Lane`, `Limits() Limits`, `Detectors() []DetectorSpec`, `Version() string`, `DerivedNames() []string` (file order: the `Ranked` columns), `Warnings() []LoadError` (compile warnings with line and field, which `rules check` prints).
- The reason renderer: `rubric.Explain(Verdict) string` renders a verdict (derived values in rubric order, the score and its halves, then the reasons). `leadscore explain` prints the lead's stored `Ranked` row in that same layout (it does not re-score), with a note when the row's `rubric_version` differs from the local rubric's; and `rules.ReasonsText(Verdict) string` (reasons joined with `; `) for a table cell.
- The `rubric` check raises `rubric_invalid:compile` when the rubric does not compile.

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
    Columns []string // the raw input headers the run fetched this time; nil in doctor
    Rubric  *rules.Rubric // the rubric the run is scoring with; nil in doctor, which compiles the file
    Now     func() time.Time // the run's clock; nil in doctor (the wall clock); Env.Clock() reads it
}
type Problem struct{ Key, Message, Fix string; Warning bool } // Key in the section 4 form <kind>:<id>
func Register(c Check)
```

A problem is written to `Health` under `Problem.Key` as given, and cleared when a later run of the same check stops returning it. Owners: S1 `secrets`; S2 registers `rubric` (compile), and S10a adds the field part to that same check (`Fields()` against the loaded columns: `Env.Model`'s People fields, Company facts and `Companies` headers, plus `Env.Columns`, so a column whose every cell is empty still counts; raising `rubric_unknown_field:<field>`); S4 `store` (the schema-version and SQLite cases, in `internal/check/store.go`, raising `store:newer_schema`, `store:bad_schema_version`, `store:disk_not_kept` and `store:opened_outside_container`; S5, S10a and S10b add their cases to that file) and S5 its Sheets cases, `sheets`, `sheet-access`; S6 `overrides` (raising `status_conflict:<lead>` and `override_unmatched:<row>`) and `duplicates`; S8 `apollo-key`; S10a the Cloud Run part of `store` (a SQLite store or CSV path while `CLOUD_RUN_JOB` or `K_SERVICE` is set, raising `store:cloud_run_files`; `run` refuses to start on it); S10b `pushes` and the ledger part of `store`; S11 `hubspot`; S12 `apollo-sequences`; S14a `receiver-secret`; S14b `hosting`; S15 `receiver-silence`; S16 `rubric-version`, `receivers`, `lease`, `pushes-enabled`. S14b extends `secrets` for Secret Manager keys (a local command on a hosted install, where S1's check skips the environment variables).

### 12.5 Merge owns persons, aliases and row ids (S6)

- `merge.Normalize(row InputRow, aliases map[string]string) Normalized` resolves headers with the section 2 table plus rubric aliases (first header in `row.Headers` order wins), computes the per-row id (RFC 6.5; for source `receiver`, the `contact_id` column, else email, else LinkedIn URL), the row hash (SHA-256 over the sorted raw `header=value` pairs plus a hash of the alias table, so an alias change re-applies rows), and does the email-shape and required-key checks. It never fails: a rejected row comes back with `Reject` set (a row with no key at all gets row id `row:` plus 16 hex characters of its hash) and goes to `Apply` like any other row, so no caller can drop a reject before it is recorded. A LinkedIn value is a key only when it is a profile URL: any `*.linkedin.com` host with a non-empty `/in/<slug>` (or `/pub/...`) path, canonicalized to `linkedin.com/in/<slug>` (each path segment percent-decoded on its own, then lowercased in Unicode NFC; scheme, subdomain, query, fragment, empty segments and anything after the slug such as `/en` dropped; a `.` or `..` segment, or a segment that decodes to contain `/`, makes it no key); anything else (`N/A`, `-`) is dropped as a key. The domain + name key is accepted here; `Apply` rejects it for a source whose `match_domain_name` is off.
- **Row groups.** Rows of one source that share a row id (the same email on two lines) form one group (`merge.Group`): applied as one unit, rows in order, under one hash (the row hash for one row, else the SHA-256 of the sorted row hashes), stored in `Applied rows.row_hash`, so a repeated row id settles. `merge.Pending(m, rows)` returns the groups not applied at their current hash: the run's backlog. The run's chunking never splits a group.
- `merge.Apply(m, rows []Normalized, c ApplyCtx{Now, RunID, Sources, Aliases})` applies rows incrementally (RFC 6.5, section 7's `same_as` rules), writes company facts from built-in company columns with origin `input`, and reads the `Companies` tab with origin `companies_tab` (`Aliases` are the rubric's, to resolve that tab's headers). Fact names in `Company facts` drop the `company.` prefix (`name`, `employees`, `funding_stage`, `region`); a `Companies` header that resolves to `company.<f>` is fact `<f>`, any other header the fact named by its squashed form, and only the first row for a domain counts. A row's company columns are credited to the lead's stored domain, and only when the row's own domain (or the one derived from its email) is that domain or absent. A key conflict adds one to `State.key_conflicts` and logs `key_conflict` only on the first conflict of a source and row id (`Applied rows.key_conflict_at`), so re-applying the row never inflates it. A group that applied before keeps its `lead_id` even when all its rows are now rejected. The domain + name rung matches only when exactly one live lead has that domain and name; otherwise the row makes a new lead, which `Duplicates` blocks. Each `same_as` merge logs `merged` and re-points every lead already merged into the absorbed lead at the survivor, so `merged_into` chains stay one step deep. A `Companies` cell emptied, or a row removed, moves its `companies_tab` fact to `previous` and out of `facts`, so a lower origin can fill it.
- `merge.FindPerson(m, Event) (LeadID, bool)` matches an event's person, with no writes: first by `Attrs["contact_id"]` through `Applied rows` (source `receiver`, row id), then through `Identities` (by email when the event has a well-formed one, and only then by LinkedIn URL, so an unknown email never matches through another lead's URL; an email failing the shape check counts as missing, so the LinkedIn URL decides), then follows `merged_into` to the live lead. `merge.ApplyEventPerson(m, Event) LeadID` does the same and, when no lead matches, creates one under source id `receiver` (never with a malformed email; a LinkedIn URL another lead holds is not written, and counts and logs a key conflict with the lead id only). `Intake` merges receiver rows before resolving persons. An opt-out event is applied beyond the lead `ApplyEventPerson` returns, to every live lead holding one of its identity keys (section 5.3, `events.Apply`).
- `merge.NormalizeEventKeys(api.Event) api.Event` is the one normalizer for event keys, since sources pass them trimmed only: the email lowercased, the LinkedIn URL canonicalized as for input rows, and `Domain` (which may be a URL) reduced to its lowercase host with no scheme, `www.`, port or path. The run's `Intake` applies it once to every event, before `events.Key`, `FindPerson`/`ApplyEventPerson`, and any window or company write, so nothing downstream reads a raw key.
- `merge.ParseOverrides(m) Overrides` (normalized persons, resolved to live leads; `Status`, `Blocked` and `Unmatched` per section 7; each row's `Hash` keys `Applied overrides`) and `merge.Duplicates(m) map[LeadID]bool` (unresolved namesakes, plus every lead in a hand-edited `merged_into` cycle, `merge.Cycles(m)`: `Live` resolves a cycle to its lowest lead id, every lane skips its leads, and the `duplicates` check raises `merge_cycle:<lead>`).
- Read helpers for building `LeadRef`s in bulk: `merge.NewIndex(m, sources)` with `LiveLeads`, `Family`, `Emails`, `LinkedInURLs`, `PrimaryEmail`, `PrimaryLinkedIn`, `PersonKey`, `SourcesSeen`, `ReceiverOnly` and `LeadsSeen`. Build it after `Intake` and `Fold`, right before `Evaluate`, and build it again if the model changes; it does not see later changes. Plus `merge.Live`, `merge.Cycles` and `merge.Resolve`, and `merge.AliasTable(rubricAliases)` with `merge.ResolveHeader(table, header)`, the one header resolver (the rubric check uses it). The CLI writers are `merge.SetStatus`, `AddPair` and `AddRetry` on the model's `Overrides`.
- S7's CSV source only parses files: UTF-8 with an optional BOM stripped, comma-delimited, ragged rows allowed, headers returned in `Headers` as written (one `Headers` slice shared by every row of a file; callers must not change it). The one change to the text is that leading spaces before a cell are trimmed, as core's reader did, so `a, "b"` reads. For event rows it uses the `internal/api` alias table to find `event`, `at` and the person columns. A file the source cannot read safely fails its whole `Fetch` with the fix in the message, rather than being half-read: a line the CSV reader cannot parse, text that is not UTF-8, CR-only line endings, an empty or blank header row, a file over 100 MB, more than 1,000 header columns, or more than 10,000,000 cells (counted before parsing as commas plus line breaks, then per row as the wider of the row and the header, since rows are padded to the header; cells past the last header are dropped and freed). Memory at these caps is around 1 GB. What a failed `Fetch` does to the run is S10a's. S5's Sheet-tab source reads event rows through the same parser (`csv.ParseEvents`), so both sources follow one set of section 5.2 rules; its reject reasons name the sheet row as the line.

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
    SourceEvents []api.Event // step 3 source events, keys already normalized (merge.NormalizeEventKeys), set by S10a before Intake
    EventsShrank bool        // Intake returned ErrEventsShrank: S9's AfterSave step deletes no processed event this run
    NoPush       string            // non-empty: score and save, but push nothing (the reason)
    Problem      func(key, message, fix string, warning bool) // raise an open problem this run
    ReRead       func() (changed []api.LeadID, err error) // calls Hooks.ReRead for S10b's push loop; nil-safe; an error raises step_failed:reread
    Input        rules.Input  // step 6's evaluator input; a re-score (S10b) updates it
    Result       rules.Result // step 6's result; PrePush may update it, and Ranked and the dry-run report follow it
    Pushed       int          // pushes made this run (S10b): (lead, lane) pushes whose every step is now done; RunResult.Pushed
}
type Hooks struct {
    Intake    func(*Run) error                       // step 3 after sources (S9)
    Enrich    func(*Run) error                       // step 4 (S8); skipped on dry-run
    Fold      func(*Run) error                       // step 5; default gives every live lead with no stored status new (S10b)
    Detect    func(*Run) (rules.DetectorResults, error) // step 6, before Evaluate (S9)
    PrePush   func(*Run, []api.LeadID) error   // step 8 (S10b)
    Push      func(*Run) error                       // step 9 (S10b)
    ReRead    func(*Run) (changed []api.LeadID, err error) // before each pushing batch (S15)
    Export    func(*Run) error                       // after Push, before phase 2, every run (S13)
    AfterSave func(*Run) error                       // after phase 2 committed: CSV rewrite (S13), view (S16)
}
func DefaultHooks() Hooks // the production set; each hook slice sets its field here in its own PR
func Chain(fs ...func(*Run) error) func(*Run) error // runs each in order, joining errors; DefaultHooks' AfterSave is a Chain that S13 and S16 each add one function to

// Owned by S10b, used by S13:
func Blocked(r *Run, id api.LeadID) (blocked bool, reason string) // blocked on every lane
func MatchesLane(r *Run, id api.LeadID, laneID string) bool // the lane's `when` held and the lead passes every check that cancels steps there; limits, lookups and pushes_enabled do not count
// RunWith is the test entry point: it adds _http_client to every vendor and
// store block (section 3) and uses the given clock; a test writes each
// block's base_url in its YAML.
func RunWith(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client) (api.RunResult, error)
// RunWithOutput is RunWith writing the summary line (and a dry run's report)
// to out instead of standard output; the e2e suite logs it.
func RunWithOutput(ctx context.Context, opts api.RunOptions, hooks Hooks, now func() time.Time, client *http.Client, out io.Writer) (api.RunResult, error)
```

- **Wiring.** `leadscore.Run`, `leadscore run`, the `serve` timer and the e2e suite all use `DefaultHooks()`. `leadscore run` closes `Stop` from its own SIGTERM handler; `serve` closes it at shutdown.
- **Sources and chunks.** S10a calls every `Source.Fetch` at step 3, puts source events in `SourceEvents` with their keys normalized once by `merge.NormalizeEventKeys` (`Intake` normalizes the events it reads itself, receiver and polled, the same way), normalizes and merges rows (`ingest_chunk_rows` per run across sources in config order, whole row groups only: a group is never split, and the first group is taken whole even when it alone is over the size), and saves `cursor:<source id>` in phase 1 only when all of that source's rows were taken this run and its events were handed to Intake (Intake ran and returned no error; a source with no events needs no Intake). The backlog is accepted rows whose (source, row id, row hash) is not yet in `Applied rows` after this chunk; while it is non-empty, `NoPush` is set.
- **Phases.** Phase 1 commits `People`, `Identities`, `Applied rows`, `Company facts`, `Seen events`, `Window events`, `Outcomes`, `Pushes` (load-time fixes, cancels, retry resets), `Applied overrides`, the `Log` lines written so far (merge's `row_rejected`, `key_conflict` and `merged` among them), and `State` cursors, `last_poll_at`, `first_run_at` (set by the first run), `key_conflicts`, `config_version` and `enrich_count:<day>`: everything merge writes because a row was applied is saved with the row, and the day's enrichment count with the facts its calls bought. The redo after `ErrTooLarge` runs `Enrich` again on the reloaded model: the hook keeps this run's answers, failure stamps, log lines, warning and error on the `Run` and puts them back without calling again, and still counts their calls, so a failed `Enrich` stays failed. After a second `ErrTooLarge` discards the model, the answers and the day's count are put back too, and phase 2 saves them; that replay writes over the discarded model, which no longer holds this run's `Companies` tab changes, so for that run an answer may land on a fact the tab just changed (never over a `companies_tab` fact the model still holds). The next run applies the tab again, and the tab wins. The tier and priority change lines are committed with the first `Ranked` chunk, so a failed `Ranked` write never logs a change twice. Each pushing batch commits `Pushes` and `State.ledger_rows`. Phase 2 commits everything else except the `Export` tables and `Ranked`: right after it, the export tables' changed rows are written in their own chunked commits (section 4, "Export rows": switches to `yes` first, `ErrTooLarge` halves the chunk), then `Ranked` in chunks (12.2).
- **`ErrCommittedWithProblems`.** The commit is done: the model takes the writes as committed, nothing is retried, and the store's message is raised as the `people_tab_check` problem (a warning).
- **`ErrTooLarge`.** On phase 1 only, the run discards the model, reloads, and redoes steps 3 to 6 with half the rows it took (not half the configured chunk); a second `ErrTooLarge` sets `NoPush`. Other commits retry once at the same size, then fail the run.
- **Hook errors.** `Intake` returning `ErrEventsShrank` sets `NoPush`, raises `events_shrank` and continues; any other `Intake` error fails the run before phase 1. A `Fold` error fails the run before phase 1 (no status is safe to score on). An `Intake` or `Fold` failure raises `step_failed:<hook>` as well as `run_failed`. `Enrich`, `Detect` and `Export` errors make the run unhealthy and it continues. A `PrePush` error skips `Push`. `Push`, `ReRead` and `AfterSave` errors make the run unhealthy. A failing hook raises `step_failed:<hook>`.
- **A source that fails.** A `Source.Fetch` error (or a source type not registered, or its factory failing) skips that source this run: its rows and events are dropped, its cursor is not advanced, `source_failed:<source id>` is raised (unhealthy) and logged, and the rest of the run continues. `NoPush` is set for the run whichever kind of source failed: skipped input rows may hold an unmerged duplicate, and skipped events (an events source, or a plug-in source) an opt-out.
- **Stop and the deadline.** Closing `Stop` cancels `PushCtx` like the deadline: the steps left before phase 1 are skipped (phase 1 saves what was merged), nothing is pushed, and the hard stop moves to the save budget from that moment (never past the deadline-based hard stop). A stop raises the warning `run_stopped`; the deadline raises `deadline_passed`. A run cut short keeps every open problem it did not re-check.
- **Dry run.** No lease, no writes, no `Enrich`, no `Push`; `PrePush` is called (S10b skips its lookups when `DryRun`), and the report's planned lanes come from `Run.Result` after it.
- **A run that fails** (an error, a `Ranked` write that fails after its retry, or a panic, which is recovered) after taking the lease writes only `Health` (`last_result` unhealthy and `run_failed` with the reason, keeping the other open problems, since it cannot tell them resolved); one whose export-table or `Ranked` write failed after phase 2 committed still rewrites the export CSVs from the committed tables (S13), and runs no other `AfterSave` step; a run that loses the lease writes nothing more.
- **Skipped runs.** A run that finds the lease held writes nothing. The next run that holds the lease raises `skipped_runs` when more than two `schedule` intervals passed since `last_run_at`.
- **Re-reads.** `ReRead` (S15's default reads the receiver events stored since its last read this run, the first time since `cursor:events`, holding that position on the run only; each event passes the same filter as in `Intake`: the kind lowercased and length-checked, skipped when its `events.Key` is already in `Seen events` or was taken by an earlier re-read this run, and the polling-mode filter of section 5.3) resolves each new event's person with `merge.FindPerson` (skipping unknown persons, which the next run creates, except that an opt-out still reaches every live lead holding one of its keys, as in section 5.3) and applies its effects to `Run.Model`'s `Outcomes` through `events.Apply` (idempotent), without moving cursors, writing `Seen events` or appending `Window events`, and returns the changed leads; S10b re-folds them before the batch. The next run re-applies those events normally.
- **A crash outside the run.** `RecordCrash(ctx, store api.Backend, startAt time.Time, err error) error` writes `Health` for a run that could not (a panic `serve`'s timer recovered): `last_result` unhealthy, `last_run_at` `startAt` and `run_failed` naming the cause, keeping the other open problems, through the run's own `Health` writer and the codec, under the run lease taken for that write. It reads only `State.schema_version` and the `Health` table (never a full `codec.Load`), so it still writes when loading the store fails (a table that cannot be read), and it recovers its own panic into its error. A store from a newer major schema gets no write (an error wrapping `codec.ErrNewerSchema`). When another run holds the lease it writes nothing and returns an error wrapping `ErrLeaseHeld`.
- **`receiver_only_push`.** A step called for a lead the receiver alone reported raises the warning `receiver_only_push:<lead>`; each run's `Fold` raises it again while the lead is still receiver-only, so it clears once a source (or a merge) reports the lead.
- **Run-level checks.** Before scoring, S10a fails the run if `rubric.Fields()` names a field that is not built in, declared, or a loaded column. Hooks raise problems through `Run.Problem`; unreported problem keys are cleared at save.

### 12.7 Events (S9)

- `events.Key(Event) EventID` holds every de-duplication key rule (RFC 6.7). Its input keys are already normalized by `merge.NormalizeEventKeys` (12.5). `internal/events` imports `adapters/apollo`, never the reverse: `adapters/apollo` defines `PolledReplyKey(messageID, label string) EventID` and the body parsers, and `events.Key` calls `PolledReplyKey` for `reply` events.
- `events.Apply(m, lead LeadID, e Event, replyLabels map[string]string) []LeadID` applies a section 5.3 effect: a send or reply to the live lead (following `merged_into`), including the label map and `reply_labels` (passed in as `Config.ReplyLabels`, since the model does not hold the config; config lowercases their keys), the origin rule, the deal fan-out, and the opt-out rule (section 5.3: an opt-out goes to the owners of the event's identity keys, owners outside the resolved lead's family logged as `key_conflict` with ids only). It returns the live leads whose `Outcomes` row or Apollo hold it changed (the re-read folds them again); an opt-out is written on the raw owners of the event's identity keys (before following `merged_into`), on the resolved live lead only when no key has an owner, plus every member of a merge cycle a target is in (section 5.3). It is the only code that writes event effects into `Outcomes` and `People`. The caller resolves the person: `Intake` through `merge.ApplyEventPerson`; S10b and S15 with the lead they looked up or re-read. `lead` may be empty for a company-level deal event, whose domain is normalized. A deal event stores only the stage class from its kind (`open`, `won` or `lost`) in `deal_stage`; the vendor's own stage name is stored nowhere. A polled label the map does not know reads as `replied_unlabelled`.
- A visit with no usable `visited_at` carries `Attrs["no_visited_at"]` and is keyed by person key, page and the UTC received day (RFC 6.7); a `visited_at` later than the received time is clamped to it for storage, but the key reads the time as sent (`Attrs["visited_at"]`), so a retry keys the same. A polled reply with no message id is keyed by person key (contact id first), label and event time (else received time); when Apollo gave no time either (the poller marks it `no_reply_time` and times it at the poll), by person key and label only, so a later poll does not read it as a new reply. The accepted trade-off: a later reply from the same person with the same label and neither a message id nor a time is dropped as a repeat. The stage in a reply key is trimmed and lowercased. Event kinds over 64 characters are refused; `State.last_received:<kind>` is kept only for the reply kinds and `receiver.visit_events`.
- `Intake` logs a stored body the parsers refuse as `event_rejected` (naming its sequence, never a body value), and a source event row with `Kind` empty, or a vendor-only kind (`sent`, `replied*`, `unsubscribed`, `reply`, `optout`, `deal_*`) from a source, as `row_rejected`, once: it records the key `reject|<source id>|<reason>` in `Seen events`, since snapshot sources return the row every run. A failed poll raises `poll_failed:<sink type>` (`poll_failed:none` when `replies: polling` has no poller in the build) and leaves `last_poll_at` unchanged.
- The `Window events` and `Seen events` trims are recorded in phase 2 with the `Log` trim; deleting processed events is S9's `AfterSave` step: skipped when the run saw `ErrEventsShrank`; when `DeleteProcessed` returns a changed cursor it is committed at once under the lease, retried once, and if that still fails the error names the value to set as `State.cursor:events` by hand (until then the next run raises `events_shrank`).
- `events.Parse(raw []RawEvent) ([]Event, []InputRow)` is pure: it returns events and the receiver input rows (source id `receiver`), with no writes.

### 12.8 The Apollo client (S8)

`adapters/apollo/client.go` is the one way into Apollo's API; S12's sink, `Lookup` and `Poller` build on it.

```go
const KeyVariable = "APOLLO_API_KEY"
const DefaultBaseURL = "https://api.apollo.io"
const CallTimeout = 30 * time.Second // every attempt, whatever HTTP client the block gives
const ContactOptOutFlag = false      // S0 confirms; section 6, "Opt-out reads"

// Key from APOLLO_API_KEY; base_url and _http_client from the block. No key
// is an error; base_url without _http_client is refused (for tests only, so
// leadscore.yml cannot send the key elsewhere). Redirects are never followed:
// a 3xx is the reply, so the key header never leaves for another host. A
// typed-nil _http_client counts as absent.
func NewClient(cfg api.Config) (*Client, error)
func NewClientWithKey(cfg api.Config, key string) (*Client, error)

type Request struct {
    Method string
    Path   string     // under the base URL
    Query  url.Values
    Body   any        // sent as JSON when non-nil
}
// Do is the single-shot call: a 2xx body is decoded into out (when non-nil);
// 429 wraps api.ErrRateLimited at once, with no wait; any other non-2xx is a
// *StatusError; a transport failure or timeout is returned wrapped, naming
// only the method, path and cause (never the URL's query).
func (c *Client) Do(ctx context.Context, req Request, out any) error
// DoRetrying is Do with a 429 retried, three attempts in all, waiting
// Retry-After (seconds, capped at 30s; 0 is at once) or 2s doubling. Only 429
// is retried. A 429 on the last attempt wraps api.ErrRateLimited. The wait
// ends early, as ErrWaitStopped (neither a rate limit nor a failure), when the
// context's WithWaitStop context is done.
func (c *Client) DoRetrying(ctx context.Context, req Request, out any) error
func WithWaitStop(ctx, stop context.Context) context.Context // the engine passes Run.PushCtx
var ErrWaitStopped error

type StatusError struct{ Status int; Detail string } // Detail: logredact.VendorErrorDetail of the body only
func (e *StatusError) Body() []byte // the raw reply, to tell refusal reasons apart; never log it
func IsStatus(err error, codes ...int) bool
func (c *Client) AuthHealth(ctx context.Context) error // the apollo-key check's call; never spends a credit
var ErrKeyRefused error                                // AuthHealth: 200 with is_logged_in false
func KeyRefused(err error) bool                        // 401, 403 or ErrKeyRefused
```

The enricher (`enrich.go`) uses `DoRetrying`; sinks, lookups and the poller use `Do` and map its errors to section 1's classes themselves (section 6, "Refusals"). `internal/fakes/apollo` serves `testdata/vendors/apollo`; S12 adds its calls to both.
