// Package leadscore is the public surface of the leadscore outbound engine: the
// plug-in interfaces, shared types, errors, the adapter registry, Run and Main.
//
// Built-in adapters register in their init functions. A custom build imports its
// own adapter package (which registers itself) and calls Main.
//
// Every name here is defined in internal/api and re-exported, so the engine can
// use the same types without importing this package. This surface is frozen at
// v0.1.0: interfaces never gain methods; a new capability is a separate optional
// interface found by type assertion.
package leadscore

import (
	"context"
	"errors"
	"os"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/cli"
)

// Identifiers.
type (
	LeadID  = api.LeadID  // UUIDv7, minted when a person is first seen
	EventID = api.EventID // an event's de-duplication key
	Cursor  = api.Cursor  // opaque progress marker; each source or store encodes its own
	StepKey = api.StepKey
)

// Inputs and what engines and adapters see of a lead.
type (
	InputRow     = api.InputRow
	Event        = api.Event
	RawEvent     = api.RawEvent
	CompanyFacts = api.CompanyFacts
	LeadRef      = api.LeadRef
	Verdict      = api.Verdict
	LedgerRef    = api.LedgerRef
	StepRequest  = api.StepRequest
	Subject      = api.Subject
)

// Plug-in interfaces.
type (
	Source         = api.Source
	Enricher       = api.Enricher
	Poller         = api.Poller
	Lookup         = api.Lookup
	Sink           = api.Sink
	Detector       = api.Detector
	RunLease       = api.RunLease
	Backend        = api.Backend
	LeaseInspector = api.LeaseInspector
	EventLog       = api.EventLog
)

// Table-level storage.
type (
	Row        = api.Row
	WriteOp    = api.WriteOp
	TableWrite = api.TableWrite
)

const (
	OpReplace = api.OpReplace // rewrite the whole table
	OpAppend  = api.OpAppend  // add rows
	OpUpsert  = api.OpUpsert  // insert or update by Key columns
	OpDelete  = api.OpDelete  // delete rows matching Key columns
	OpTrim    = api.OpTrim    // delete rows whose Column is before Before
)

// Sink errors. Classify with errors.Is. Any other error counts one attempt
// toward `failed`. None of these says the vendor did nothing; the ledger records
// whether the call went out.
var (
	ErrRateLimited = api.ErrRateLimited // stays pending, no attempt counted, stop this sink for the run
	ErrTransient   = api.ErrTransient   // stays pending, no attempt counted
	ErrRefused     = api.ErrRefused     // vendor said no for a reason retrying cannot change
)

// Store errors.
var (
	ErrTooLarge     = api.ErrTooLarge
	ErrLeaseHeld    = api.ErrLeaseHeld
	ErrLeaseLost    = api.ErrLeaseLost
	ErrEventsShrank = api.ErrEventsShrank // engine scores but does not push
)

// Config is an adapter's block from leadscore.yml.
type Config = api.Config

type (
	RunOptions = api.RunOptions
	RunResult  = api.RunResult
)

// Registering the same type twice panics.
func RegisterSource(typ string, f func(Config) (Source, error)) { api.RegisterSource(typ, f) }

func RegisterEnricher(typ string, f func(Config) (Enricher, error)) { api.RegisterEnricher(typ, f) }

func RegisterPoller(typ string, f func(Config) (Poller, error)) { api.RegisterPoller(typ, f) }

func RegisterLookup(typ string, f func(Config) (Lookup, error)) { api.RegisterLookup(typ, f) }

func RegisterSink(typ string, f func(Config) (Sink, error)) { api.RegisterSink(typ, f) }

func RegisterDetector(kind string, f func(params Config) (Detector, error)) {
	api.RegisterDetector(kind, f)
}

func RegisterBackend(typ string, f func(Config) (Backend, EventLog, error)) {
	api.RegisterBackend(typ, f)
}

// errRunNotBuilt is what Run returns until the run loop exists.
var errRunNotBuilt = errors.New("leadscore: Run is not built yet (slice S10a)")

// Run executes one run, as `leadscore run` does, with the production hooks.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	return RunResult{}, errRunNotBuilt
}

// Main is the CLI entry point.
func Main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
