package sink_test

import (
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakesink "github.com/HarshitBadhwar8/leadscore/internal/fakes/sink"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// The fake sink passes the public conformance suite, run from an external
// test package as every sink runs it.
func TestFakeSinkConforms(t *testing.T) {
	v := fakesink.New(map[string][]string{"sequence/A": {"contact", "enroll"}, "deals": {"contact", "deal"}})
	sinktest.Run(t, sinktest.Harness{
		New:    func(api.Config) (api.Sink, error) { return v.Sink(), nil },
		Vendor: v,
		Dests:  []string{"sequence/A", "deals", "anything"},
	})
}
