// Package storetest is the conformance suite a plug-in store runs against
// itself (contracts section 1). S4 fills Schema and Run.
//
// The signatures use internal/api's names, which are the same types as
// leadscore.Backend and leadscore.EventLog (the root aliases them), so this
// package never imports the root.
package storetest

import (
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

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
//
// Call it from an external test package (package sqlite_test, not package
// sqlite), so a store inside this module can be tested without an import cycle.
func Run(t *testing.T, open func(t *testing.T) (api.Backend, api.EventLog)) {
	t.Skip("built in S4")
}
