package leadscore_test

// The public API's proof: a stub adapter of every kind compiles against the public API
// and registers through it, exactly as an external adapter package would.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakesink "github.com/HarshitBadhwar8/leadscore/internal/fakes/sink"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
	"github.com/HarshitBadhwar8/leadscore/storetest"
)

type stubSource struct{}

func (stubSource) ID() string { return "stub" }
func (stubSource) Fetch(ctx context.Context, cursor leadscore.Cursor) ([]leadscore.InputRow, []leadscore.Event, leadscore.Cursor, error) {
	return []leadscore.InputRow{{SourceID: "stub", Headers: []string{"Email"}, Columns: map[string]string{"Email": "x"}}},
		nil, cursor, nil
}

type stubEnricher struct{}

func (stubEnricher) Enrich(ctx context.Context, domains []string, budget int) ([]leadscore.CompanyFacts, error) {
	return nil, leadscore.ErrRateLimited
}

type stubPoller struct{}

func (stubPoller) Poll(ctx context.Context, since time.Time) ([]leadscore.Event, error) {
	return nil, nil
}

type stubLookup struct{}

func (stubLookup) Lookup(ctx context.Context, leads []leadscore.LeadRef) ([]leadscore.Event, map[leadscore.LeadID]error, error) {
	return nil, map[leadscore.LeadID]error{}, nil
}

type stubSink struct{}

func (stubSink) Steps(dest string) []string { return []string{"contact", "enroll"} }
func (stubSink) Do(ctx context.Context, req leadscore.StepRequest) (string, error) {
	return string(req.Key.LeadID) + "/" + req.Key.Step, nil
}

type stubDetector struct{}

func (stubDetector) Name() string { return "stub" }
func (stubDetector) Evaluate(s leadscore.Subject, events []leadscore.Event, now time.Time) (bool, []leadscore.EventID) {
	return len(events) > 0, nil
}

type stubLease struct{}

func (stubLease) Check(ctx context.Context) error   { return nil }
func (stubLease) Release(ctx context.Context) error { return nil }

type stubBackend struct{}

func (stubBackend) ReadTable(ctx context.Context, name string) ([]leadscore.Row, error) {
	return nil, nil
}
func (stubBackend) Lease(ctx context.Context, owner string, ttl time.Duration) (leadscore.RunLease, error) {
	return stubLease{}, nil
}
func (stubBackend) Commit(ctx context.Context, writes []leadscore.TableWrite) error {
	for _, w := range writes {
		if (w.Op == leadscore.OpUpsert || w.Op == leadscore.OpDelete) && len(w.Key) == 0 {
			return fmt.Errorf("%s: no key", w.Table)
		}
	}
	return nil
}
func (stubBackend) LeaseInfo(ctx context.Context) (string, time.Time, error) {
	return "", time.Time{}, nil
}

type stubEventLog struct{}

func (stubEventLog) AppendEvents(ctx context.Context, events []leadscore.RawEvent) error { return nil }
func (stubEventLog) ReadEvents(ctx context.Context, cursor leadscore.Cursor) ([]leadscore.RawEvent, leadscore.Cursor, error) {
	return nil, cursor, nil
}
func (stubEventLog) DeleteProcessed(ctx context.Context, committed leadscore.Cursor, olderThan time.Time) (leadscore.Cursor, error) {
	return committed, nil
}

// Compile-time proof that each stub satisfies its interface.
var (
	_ leadscore.Source         = stubSource{}
	_ leadscore.Enricher       = stubEnricher{}
	_ leadscore.Poller         = stubPoller{}
	_ leadscore.Lookup         = stubLookup{}
	_ leadscore.Sink           = stubSink{}
	_ leadscore.Detector       = stubDetector{}
	_ leadscore.RunLease       = stubLease{}
	_ leadscore.Backend        = stubBackend{}
	_ leadscore.LeaseInspector = stubBackend{}
	_ leadscore.EventLog       = stubEventLog{}
)

func init() {
	leadscore.RegisterSource("stub", func(leadscore.Config) (leadscore.Source, error) { return stubSource{}, nil })
	leadscore.RegisterEnricher("stub", func(leadscore.Config) (leadscore.Enricher, error) { return stubEnricher{}, nil })
	leadscore.RegisterPoller("stub", func(leadscore.Config) (leadscore.Poller, error) { return stubPoller{}, nil })
	leadscore.RegisterLookup("stub", func(leadscore.Config) (leadscore.Lookup, error) { return stubLookup{}, nil })
	leadscore.RegisterSink("stub", func(leadscore.Config) (leadscore.Sink, error) { return stubSink{}, nil })
	leadscore.RegisterDetector("stub", func(params leadscore.Config) (leadscore.Detector, error) { return stubDetector{}, nil })
	leadscore.RegisterBackend("stub", func(leadscore.Config) (leadscore.Backend, leadscore.EventLog, error) {
		return stubBackend{}, stubEventLog{}, nil
	})
}

func TestStubAdaptersRegister(t *testing.T) {
	cfg := leadscore.Config{"type": "stub"}
	if f, ok := api.SourceFactory("stub"); !ok {
		t.Error("source not registered")
	} else if s, err := f(cfg); err != nil || s.ID() != "stub" {
		t.Errorf("source factory = %v, %v", s, err)
	} else if rows, _, _, err := s.Fetch(context.Background(), ""); err != nil || len(rows) != 1 {
		t.Errorf("Fetch = %v, %v", rows, err)
	}
	if f, ok := api.EnricherFactory("stub"); !ok {
		t.Error("enricher not registered")
	} else if e, _ := f(cfg); e == nil {
		t.Error("enricher factory returned nil")
	} else if _, err := e.Enrich(context.Background(), []string{"a.example"}, 1); !errors.Is(err, leadscore.ErrRateLimited) {
		t.Errorf("Enrich err = %v", err)
	}
	if f, ok := api.PollerFactory("stub"); !ok {
		t.Error("poller not registered")
	} else if p, err := f(cfg); err != nil || p == nil {
		t.Errorf("poller factory = %v, %v", p, err)
	} else if _, err := p.Poll(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Errorf("Poll err = %v", err)
	}
	if f, ok := api.LookupFactory("stub"); !ok {
		t.Error("lookup not registered")
	} else if l, err := f(cfg); err != nil || l == nil {
		t.Errorf("lookup factory = %v, %v", l, err)
	} else if _, failed, err := l.Lookup(context.Background(), []leadscore.LeadRef{{ID: "L1"}}); err != nil || failed == nil {
		t.Errorf("Lookup = %v, %v", failed, err)
	}
	if f, ok := api.SinkFactory("stub"); !ok {
		t.Error("sink not registered")
	} else if s, _ := f(cfg); s == nil {
		t.Error("sink factory returned nil")
	} else if steps := s.Steps("sequence/x"); len(steps) != 2 {
		t.Errorf("Steps = %v", steps)
	} else {
		id, err := s.Do(context.Background(), leadscore.StepRequest{
			Key: leadscore.StepKey{LeadID: "L1", LaneID: "warm", Step: "contact"}, Dest: "sequence/x"})
		if err != nil || id != "L1/contact" {
			t.Errorf("Do = %q, %v", id, err)
		}
	}
	if f, ok := api.DetectorFactory("stub"); !ok {
		t.Error("detector not registered")
	} else if d, err := f(leadscore.Config{}); err != nil || d == nil {
		t.Errorf("detector factory = %v, %v", d, err)
	} else if fired, _ := d.Evaluate(leadscore.Subject{Domain: "a.example"}, []leadscore.Event{{Kind: "visit_pricing"}}, time.Now()); !fired || d.Name() != "stub" {
		t.Errorf("Evaluate fired=%v name=%q", fired, d.Name())
	}
	if f, ok := api.BackendFactory("stub"); !ok {
		t.Error("backend not registered")
	} else {
		b, log, err := f(cfg)
		if err != nil || b == nil || log == nil {
			t.Fatalf("backend factory = %v, %v, %v", b, log, err)
		}
		if li, ok := b.(leadscore.LeaseInspector); !ok {
			t.Error("the optional LeaseInspector must be found by type assertion")
		} else if _, _, err := li.LeaseInfo(context.Background()); err != nil {
			t.Errorf("LeaseInfo err = %v", err)
		}
		lease, err := b.Lease(context.Background(), "run-1", time.Minute)
		if err != nil || lease.Check(context.Background()) != nil || lease.Release(context.Background()) != nil {
			t.Errorf("lease round trip failed: %v", err)
		}
		if err := log.AppendEvents(context.Background(), []leadscore.RawEvent{{Kind: "apollo_reply"}}); err != nil {
			t.Errorf("AppendEvents err = %v", err)
		}
		if _, next, err := log.ReadEvents(context.Background(), "c1"); err != nil || next != "c1" {
			t.Errorf("ReadEvents = %q, %v", next, err)
		}
		if got, err := log.DeleteProcessed(context.Background(), "c1", time.Now()); err != nil || got != "c1" {
			t.Errorf("DeleteProcessed = %q, %v", got, err)
		}
		err = b.Commit(context.Background(), []leadscore.TableWrite{
			{Table: "People", Op: leadscore.OpUpsert, Rows: []leadscore.Row{{"lead_id": "L1"}}},
		})
		if err == nil {
			t.Error("stub Commit must refuse an OpUpsert with no Key")
		}
		if b.Commit(context.Background(), []leadscore.TableWrite{
			{Table: "Log", Op: leadscore.OpTrim, Column: "at", Before: time.Now()},
		}) != nil {
			t.Error("OpTrim write refused")
		}
	}
}

func TestRegisterTwicePanicsThroughTheRoot(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering the same type twice must panic")
		}
	}()
	leadscore.RegisterSink("stub", func(leadscore.Config) (leadscore.Sink, error) { return stubSink{}, nil })
}

// The root names are the internal ones, so errors.Is and type identity hold across the two.
func TestReExportsAreTheSameValues(t *testing.T) {
	pairs := []struct{ root, internal error }{
		{leadscore.ErrRateLimited, api.ErrRateLimited},
		{leadscore.ErrTransient, api.ErrTransient},
		{leadscore.ErrRefused, api.ErrRefused},
		{leadscore.ErrTooLarge, api.ErrTooLarge},
		{leadscore.ErrLeaseHeld, api.ErrLeaseHeld},
		{leadscore.ErrLeaseLost, api.ErrLeaseLost},
		{leadscore.ErrEventsShrank, api.ErrEventsShrank},
	}
	for _, p := range pairs {
		if !errors.Is(fmt.Errorf("wrapped: %w", p.root), p.internal) {
			t.Errorf("%v is not the internal error", p.root)
		}
	}
	var b api.Backend = stubBackend{}
	var _ leadscore.Backend = b // identical types: no conversion needed
	if leadscore.OpTrim != api.OpTrim || leadscore.OpReplace != 0 {
		t.Error("WriteOp constants differ from the contract order")
	}
}

func TestRunFailsWithoutConfig(t *testing.T) {
	_, err := leadscore.Run(context.Background(), leadscore.RunOptions{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yml"), Stop: make(chan struct{})})
	if err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("a run with no leadscore.yml must fail loading it, got %v", err)
	}
}

func TestConformanceSuitesAreDeclared(t *testing.T) {
	// storetest is filled and runs against real stores; here only its
	// signature is held, since the stubs store nothing.
	var _ func(*testing.T, func(*testing.T) (leadscore.Backend, leadscore.EventLog)) = storetest.Run
	// sinktest is filled: it runs here against the fake vendor through
	// the root's names, as an adopter's sink package would call it.
	t.Run("sinktest", func(t *testing.T) {
		v := fakesink.New(map[string][]string{"sequence/x": {"contact", "enroll"}})
		sinktest.Run(t, sinktest.Harness{
			New:    func(leadscore.Config) (leadscore.Sink, error) { return v.Sink(), nil },
			Vendor: v,
			Dests:  []string{"sequence/x"},
		})
	})
	_ = []sinktest.FailKind{sinktest.RateLimited, sinktest.Transient, sinktest.Refused, sinktest.Other}
	_ = storetest.Schema
}

// The built-in SQLite store registers through the root package, so a custom
// build that only calls leadscore.Main gets it.
func TestBuiltInSQLiteStoreRegistered(t *testing.T) {
	if _, ok := api.BackendFactory("sqlite"); !ok {
		t.Error("the sqlite backend is not registered by the root package")
	}
}
