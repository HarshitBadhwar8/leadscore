// Package leadscore is the public surface of the leadscore outbound engine: the
// plug-in interfaces, shared types, errors, the adapter registry, Run and Main.
//
// Built-in adapters register in their init functions. A custom build imports its
// own adapter package (which registers itself) and calls Main.
//
// Every name here is defined in internal/api and re-exported as a type alias or
// a thin wrapper, so the engine can use the same types without importing this
// package. The doc comments below repeat the contract (contracts section 1).
// This surface is frozen at v0.1.0: interfaces never gain methods; a new
// capability is a separate optional interface found by type assertion.
package leadscore

import (
	"context"
	"os"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/cli"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"

	// The built-in stores register here: their packages are internal, so a
	// custom build cannot import them, and every build gets them this way.
	_ "github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	_ "github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// LeadID is a lead's id: a UUIDv7, minted when a person is first seen.
type LeadID = api.LeadID

// EventID is an event's de-duplication key.
type EventID = api.EventID

// Cursor is an opaque progress marker; each source or store encodes its own.
type Cursor = api.Cursor

// StepKey names one push step: the lead, the lane's stable id from the rubric,
// and one of the sink's Steps(dest).
type StepKey = api.StepKey

// InputRow is one row from a source: its source id, its headers in file order
// as written, and every column as raw text keyed by header as written. The
// engine, not the source, applies aliases and computes the per-row id.
type InputRow = api.InputRow

// Event is something that happened to a person or a company. ID is empty from
// sources (the engine sets it). Kind is a section 5.3 kind, or empty with
// Attrs["reject"] set for a rejected source row. Email and LinkedInURL are the
// person keys, both empty for a company-only event. Domain is the person's
// employer domain, or the company for a company-only event. At and ReceivedAt
// are UTC. Origin is "receiver", "polling", "hubspot", "apollo_lookup", or a
// source id. Attrs is kind-specific: stage, label, message_id, page, deal_id,
// contact_id, full_name, title, company, reject.
type Event = api.Event

// RawEvent is one stored receiver request. Seq is assigned by the store on
// append (empty when appending) and is a complete resume cursor: reading from
// it returns exactly the events after this one, across every partition the
// store keeps. Body has the secret already removed.
type RawEvent = api.RawEvent

// CompanyFacts is what an enricher learned about a company domain. Region is
// the vendor's country, trimmed, with no bucketing. FundingStage is one of the
// section 6 values, or empty. Employees is nil when unknown. Extra holds other
// facts; the Apollo enricher writes latest_funding_at. NotFound means the vendor
// had no record (retried after max age).
type CompanyFacts = api.CompanyFacts

// LeadRef is what engines and adapters see of a lead. Emails holds every email
// for the lead and every lead merged into it, primary first. Domain is the
// company domain, empty when the lead has none. Status is the folded status.
// Fields holds merged fields by resolved name. SourcesSeen counts distinct
// channels. ConflictFields are fields whose sources disagreed. CompanyDealID is
// the stored open or won deal at the lead's company, if any. Verdict is nil
// before scoring.
type LeadRef = api.LeadRef

// Verdict is a lead's score: every derived name in Values (numbers are
// float64; nil means "no value"), both score halves (AccountScore is 0 when the
// lead has no company), and the reasons in order.
type Verdict = api.Verdict

// LedgerRef is one push-ledger step: key, destination, vendor id and state
// (pending, done, failed, cancelled).
type LedgerRef = api.LedgerRef

// StepRequest is one call to Sink.Do. Dest is the part after "<sink>:" in the
// lane's push target; Prior holds vendor ids from this push's earlier steps, by
// step name; Related holds this sink's done steps for other leads at the same
// company, any lane, including steps finished earlier in this batch (only deals
// at an open stage).
type StepRequest = api.StepRequest

// Subject is what a detector evaluates: a lead, or a company when Lead is nil.
type Subject = api.Subject

// Source returns input rows and events. Fetch returns rows and events after
// cursor; snapshot sources (CSV, Sheet tabs) ignore the cursor and return
// everything every run.
type Source = api.Source

// Enricher returns company facts. Enrich calls domains in the order given and
// stops only on a rate limit, when it returns the facts so far and
// ErrRateLimited. A failure on one domain is skipped. The caller counts calls
// made as the index of the last domain tried, plus one.
type Enricher = api.Enricher

// Poller reads outcomes on a schedule (Apollo reply polling), at run step 3.
// since is the start of the window to read; the poller uses it as given.
type Poller = api.Poller

// Lookup checks leads just before pushing, at run step 8, on every email in
// LeadRef.Emails. failed names leads whose lookup failed; the engine blocks
// those leads. err means the whole lookup failed.
type Lookup = api.Lookup

// Sink pushes leads to a vendor. Steps returns the ordered steps for one
// destination. Do must be find-or-create by req.Key: calling it twice with the
// same key leaves one vendor-side object. Classify its errors with
// ErrRateLimited, ErrTransient and ErrRefused.
type Sink = api.Sink

// Detector decides whether a signal fired. Evaluate gets the subject's window
// events: a lead's own, or for a company every event whose domain is the
// company, lead events included.
type Detector = api.Detector

// RunLease is a held lease. Check returns ErrLeaseLost if the lease expired or
// another owner took it. Release gives the lease up only if this owner still
// holds it.
type RunLease = api.RunLease

// Backend is table-level storage.
//
// ReadTable returns every row of a table; a missing table returns no rows and no
// error. Lease takes the run lease or returns ErrLeaseHeld; an expired lease is
// taken over; it must be a real compare-and-swap, so two callers can never both
// hold it. Commit applies every write all-or-nothing (one Sheets batchUpdate,
// one SQL transaction); it rejects an OpUpsert or OpDelete with no Key before
// applying anything; an OpAppend to a keyed section 4 table of a key the table
// already holds (or that the same commit already wrote) fails the whole commit;
// it creates a missing table, and appends a missing column,
// the first time a write names it; it returns ErrTooLarge rather than splitting.
type Backend = api.Backend

// LeaseInspector is optional on a Backend; doctor uses it to show the lease
// without taking it.
type LeaseInspector = api.LeaseInspector

// EventLog is the store's append-only log of raw receiver requests.
//
// AppendEvents stores a batch all-or-nothing, in order, and returns only once it
// is durable; it retries the store's "slow down" answers until ctx is done.
// ReadEvents returns events after cursor and the next cursor: an event not yet
// returned is returned by a later read from the saved cursor, and sequence
// numbers are never reused, even after deletion; it returns ErrEventsShrank when
// a partition named in the cursor is missing or holds fewer rows than the cursor
// says were read. DeleteProcessed removes events at or below committed that are
// older than olderThan and returns committed with any deleted partition dropped;
// the engine saves the returned cursor.
type EventLog = api.EventLog

// Row is one table row, keyed by column name.
type Row = api.Row

// WriteOp is the kind of a TableWrite.
type WriteOp = api.WriteOp

// TableWrite is one write in a Backend.Commit. Table is the section 4 table
// name exactly; Key names the columns for OpUpsert and OpDelete (required for
// both); Column and Before are for OpTrim only.
type TableWrite = api.TableWrite

const (
	OpReplace = api.OpReplace // rewrite the whole table
	OpAppend  = api.OpAppend  // add rows; on a keyed section 4 table, a key the table already holds fails the commit
	OpUpsert  = api.OpUpsert  // insert or update by Key columns
	OpDelete  = api.OpDelete  // delete rows matching Key columns
	OpTrim    = api.OpTrim    // delete rows whose Column is before Before
)

// Sink errors. Classify with errors.Is. Any other error counts one attempt
// toward `failed`. None of these says the vendor did nothing; the ledger records
// whether the call went out.
var (
	// ErrRateLimited: the step stays pending, no attempt is counted, and this
	// sink stops for the run. An Enricher also returns it on a rate limit.
	ErrRateLimited = api.ErrRateLimited
	// ErrTransient: the step stays pending and no attempt is counted.
	ErrTransient = api.ErrTransient
	// ErrRefused: the vendor said no for a reason retrying cannot change.
	ErrRefused = api.ErrRefused
)

// Store errors.
var (
	// ErrTooLarge: Commit would exceed the store's limit; the store does not split.
	ErrTooLarge = api.ErrTooLarge
	// ErrCommittedWithProblems: Commit saved every write, but a people-owned
	// table needs a person to look at it; treat the commit as done, never resend.
	ErrCommittedWithProblems = api.ErrCommittedWithProblems
	// ErrLeaseHeld: another owner holds a live run lease.
	ErrLeaseHeld = api.ErrLeaseHeld
	// ErrLeaseLost: the lease expired or another owner took it.
	ErrLeaseLost = api.ErrLeaseLost
	// ErrEventsShrank: the event log shrank below a saved cursor; the engine
	// scores but does not push.
	ErrEventsShrank = api.ErrEventsShrank
)

// Config is an adapter's block from leadscore.yml: a source gets its sources[]
// entry, the enricher gets enrich, the store gets store, a sink of type T gets
// sinks.T.
type Config = api.Config

// RunOptions configures Run. ConfigPath is leadscore.yml or a hosted bundle
// (with a bundle, RubricPath is ignored); empty means the default locations.
// Closing Stop stops new vendor calls and the run saves within its budget (the
// graceful stop); cancelling ctx is the hard stop.
type RunOptions = api.RunOptions

// RunResult is a run's outcome. Healthy is a run that finished, or skipped
// because another run held the lease (Skipped). Problems are the open problems
// written to Health. Pushed is the number of pushes made this run.
type RunResult = api.RunResult

// RegisterSource registers a Source factory under a config `type:`. It panics on
// an empty type, a nil factory, or a type already registered.
func RegisterSource(typ string, f func(Config) (Source, error)) { api.RegisterSource(typ, f) }

// RegisterEnricher registers an Enricher factory under a config `type:`. It
// panics on an empty type, a nil factory, or a type already registered.
func RegisterEnricher(typ string, f func(Config) (Enricher, error)) { api.RegisterEnricher(typ, f) }

// RegisterPoller registers a Poller factory under a sink type. It panics on an
// empty type, a nil factory, or a type already registered.
func RegisterPoller(typ string, f func(Config) (Poller, error)) { api.RegisterPoller(typ, f) }

// RegisterLookup registers a Lookup factory under a sink type. It panics on an
// empty type, a nil factory, or a type already registered.
func RegisterLookup(typ string, f func(Config) (Lookup, error)) { api.RegisterLookup(typ, f) }

// RegisterSink registers a Sink factory under a sink type. It panics on an
// empty type, a nil factory, or a type already registered.
func RegisterSink(typ string, f func(Config) (Sink, error)) { api.RegisterSink(typ, f) }

// RegisterDetector registers a Detector factory under a rubric detector kind;
// it receives the detector's `params:`. The kind is stored lowercased, since
// rubric kinds compare lowercased. It panics on an empty kind, a nil factory,
// or a kind already registered (two kinds differing only in case collide).
func RegisterDetector(kind string, f func(params Config) (Detector, error)) {
	api.RegisterDetector(kind, f)
}

// RegisterBackend registers a store factory under a `store.type`. It panics on
// an empty type, a nil factory, or a type already registered.
func RegisterBackend(typ string, f func(Config) (Backend, EventLog, error)) {
	api.RegisterBackend(typ, f)
}

// Run executes one run, as `leadscore run` does, with the production hooks.
// It prints the run's summary line (and, on a dry run, the report) to stdout.
// The error is non-nil when the run failed; a run that finished unhealthy
// returns a nil error with Healthy false.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	// As the CLI does: mask the exact key values before anything can log.
	logredact.MaskEnvSecrets(os.Getenv)
	return engine.RunTo(ctx, opts, os.Stdout)
}

// Main is the CLI entry point.
func Main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
