// Package check is the framework for doctor checks (contracts section 12.4).
// Each check registers itself under its section 10 name; `doctor` runs them
// all, and every run also runs the ones marked InRun.
package check

import (
	"context"
	"fmt"
	"sync"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

type Check interface {
	Name() string // unique; the section 10 names
	InRun() bool  // also runs inside every run
	Run(ctx context.Context, env Env) []Problem
}

// Env is what a check may read. Store and Events use the internal/api names,
// which are the same types as leadscore.Backend and leadscore.EventLog (the
// root package aliases them); importing the root here would be a cycle.
type Env struct {
	Config *config.Config
	Model  *model.Model // nil when doctor could not load the store
	Store  api.Backend
	Events api.EventLog
}

// Problem is written to Health under Key as given, and cleared when a later run
// of the same check stops returning it.
type Problem struct {
	Key, Message, Fix string // Key in the section 4 form <kind>:<id>
	Warning           bool
}

var (
	mu     sync.RWMutex
	checks []Check
	names  = map[string]bool{}
)

// Register adds a check. It panics on a nil check, an empty name, or a name
// already registered: check names are unique (section 10).
func Register(c Check) {
	if c == nil {
		panic("check: Register(nil)")
	}
	name := c.Name()
	if name == "" {
		panic("check: Register with an empty name")
	}
	mu.Lock()
	defer mu.Unlock()
	if names[name] {
		panic(fmt.Sprintf("check: %q registered twice", name))
	}
	names[name] = true
	checks = append(checks, c)
}

// All returns every registered check in registration order.
func All() []Check {
	mu.RLock()
	defer mu.RUnlock()
	return append([]Check(nil), checks...)
}

// InRun returns the checks that also run inside every run, in registration order.
func InRun() []Check {
	var out []Check
	for _, c := range All() {
		if c.InRun() {
			out = append(out, c)
		}
	}
	return out
}
