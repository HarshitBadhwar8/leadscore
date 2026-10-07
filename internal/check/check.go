// Package check is the framework for doctor checks.
// Each check registers itself under its doctor check name; `doctor` runs them
// all, and every run also runs the ones marked InRun.
package check

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

type Check interface {
	Name() string // unique; the doctor check names
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
	// Columns are the raw input headers the run fetched this time, so the
	// rubric check counts a column every row leaves empty as loaded. Nil in
	// doctor, which reads only the model.
	Columns []string
	// Rubric is the rubric the run is scoring with, so the rubric check judges
	// that one even if the file changed since the run read it. Nil in doctor,
	// which compiles the file.
	Rubric *rules.Rubric
	// Now is the run's clock, so a check judging time (receiver-silence)
	// agrees with the run's other times. Nil in doctor: the wall clock.
	Now func() time.Time
	// Doctor is true when `leadscore doctor` runs the check, false inside a
	// run. A check whose problem is only a warning for the run (the SQLite
	// view's sheet-access), or that doctor cannot judge fully (the rubric
	// field part, with no input headers), reads it.
	Doctor bool
}

// Clock returns the environment's clock: Now, else the wall clock.
func (e Env) Clock() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Problem is written to Health under Key as given, and cleared when a later run
// of the same check stops returning it.
type Problem struct {
	Key, Message, Fix string // Key in the problem-key form <kind>:<id>
	Warning           bool
}

var (
	mu     sync.RWMutex
	checks []Check
	names  = map[string]bool{}
)

// Register adds a check. It panics on a nil check, an empty name, or a name
// already registered: check names are unique.
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
