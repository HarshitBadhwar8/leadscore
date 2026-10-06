// Package api defines the public surface of leadscore: its types, interfaces,
// errors and adapter registry (contracts section 1), plus the built-in header
// alias table (contracts section 2).
//
// It lives under internal/ so the engine can use these types without importing
// the root package, which would be a cycle. The root package re-exports every
// public name here as a type alias or a thin wrapper; godoc shows them there.
// Section 1 is frozen at v0.1.0: these interfaces never gain methods.
package api

import (
	"context"
	"errors"
	"time"
)

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
	ID          EventID // empty from sources; the engine sets it (section 12.7)
	Kind        string  // section 5.3; empty with Attrs["reject"] set for a rejected source row
	Email       string  // person keys; both empty for a company-only event
	LinkedInURL string
	Domain      string    // the person's employer domain, or the company for a company-only event
	At          time.Time // when it happened, UTC
	ReceivedAt  time.Time
	Origin      string            // "receiver", "polling", "hubspot", "apollo_lookup", or a source id
	Attrs       map[string]string // kind-specific: stage, label, message_id, page, deal_id, contact_id, full_name, title, company, reject
}

type RawEvent struct {
	// Seq is assigned by the store on append (empty when appending) and is a
	// complete resume cursor: reading from it returns exactly the events after
	// this one, across every partition the store keeps.
	Seq        Cursor
	Kind       string // "apollo_visit" or "apollo_reply" (section 5.1)
	ReceivedAt time.Time
	Body       []byte // the request body, secret already removed
}

type CompanyFacts struct {
	Domain       string
	Name         string
	Region       string            // the vendor's country, trimmed; no bucketing
	FundingStage string            // one of the values in section 6, or empty
	Employees    *int              // nil when unknown
	Extra        map[string]string // other facts; the Apollo enricher writes latest_funding_at
	FetchedAt    time.Time
	NotFound     bool // vendor had no record; retried after max age
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
	ConflictFields []string // fields whose sources disagreed (People.conflicts)
	CompanyDealID  string   // the stored open or won deal at the lead's company, if any
	Verdict        *Verdict // nil before scoring
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
	Key   StepKey
	Dest  string // the part after "<sink>:" in the lane's push target
	Lead  LeadRef
	Prior map[string]string // vendor ids from this push's earlier steps, by step name
	// Related is this sink's done steps for other leads at the same company, any
	// lane, including steps finished earlier in this batch; only deals at an open
	// stage are included.
	Related []LedgerRef
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
	Table  string // the section 4 table name exactly
	Op     WriteOp
	Key    []string // columns for OpUpsert and OpDelete; required for both
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
