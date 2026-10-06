// Package sinktest is the conformance suite a plug-in sink runs against itself
// (contracts section 1). S10b fills Run.
package sinktest

import (
	"testing"

	"github.com/HarshitBadhwar8/leadscore"
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
	New    func(cfg leadscore.Config) (leadscore.Sink, error)
	Vendor Vendor // a fake the sink is pointed at
	Dests  []string
}

// Run replays every step after a simulated crash and asserts one vendor-side
// object, calls Do twice with one key, and checks each FailKind maps to its error.
func Run(t *testing.T, h Harness) {
	t.Skip("built in S10b")
}
