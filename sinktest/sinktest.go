// Package sinktest is the conformance suite a plug-in sink runs against itself
// (contracts section 1). S10b fills Run.
//
// The signatures use internal/api's names, which are the same types as
// leadscore.Config and leadscore.Sink (the root aliases them), so this package
// never imports the root.
package sinktest

import (
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

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
//
// Call it from an external test package (package apollo_test, not package
// apollo), so a sink inside this module can be tested without an import cycle.
func Run(t *testing.T, h Harness) {
	t.Skip("built in S10b")
}
