// Package sink is an in-memory fake vendor for tests: a find-or-create Sink
// and a Lookup over one shared state, so the push loop, the pre-push lookups
// and the sinktest suite run with no vendor account (contracts section 1).
//
// The sink keeps one object per step key, as a real sink must; a step can be
// made to fail before the vendor acts (Fail) or after it acted (FailAfter, a
// timeout once the object exists). The lookup reports opt-outs by email and a
// deal stage per company domain.
package sink

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// Vendor is the fake's shared state. Create it with New.
type Vendor struct {
	mu      sync.Mutex
	steps   map[string][]string                    // dest -> steps
	objects map[string]map[api.StepKey]string      // step -> key -> vendor id
	fails   map[string][]failure                   // step -> queued failures
	calls   []api.StepRequest                      // every Do, in order
	next    int                                    // the last vendor id handed out
	optOuts map[string]bool                        // emails opted out
	deals   map[string]deal                        // domain -> deal
	failed  map[api.LeadID]error                   // per-lead lookup failures
	lookErr error                                  // the whole lookup fails
	looked  [][]api.LeadRef                        // every Lookup call's leads
	before  func(context.Context, api.StepRequest) // runs at the start of every Do
	byCo    bool                                   // the lookup reads deals by company
	reuse   map[string]bool                        // steps that reuse a Related object of the same step
}

type failure struct {
	kind  sinktest.FailKind
	after bool // the vendor acted, then the call failed
}

type deal struct{ id, stage string }

// New returns a fake vendor whose sink has the given steps per destination;
// a destination not listed has the one step "push".
func New(steps map[string][]string) *Vendor {
	return &Vendor{
		steps:   steps,
		objects: map[string]map[api.StepKey]string{},
		fails:   map[string][]failure{},
		optOuts: map[string]bool{},
		deals:   map[string]deal{},
		failed:  map[api.LeadID]error{},
		reuse:   map[string]bool{},
	}
}

// ReuseRelated makes the step reuse the object of a done step of the same
// name in StepRequest.Related instead of creating one, as a deals sink keeps
// one deal per company (contracts section 1, Related).
func (v *Vendor) ReuseRelated(step string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reuse[step] = true
}

// Sink returns a new sink over the vendor, as a fresh process would build it.
func (v *Vendor) Sink() api.Sink { return fakeSink{v} }

// Lookup returns a lookup over the vendor.
func (v *Vendor) Lookup() api.Lookup { return fakeLookup{v} }

// Count is the number of vendor-side objects created for a step
// (sinktest.Vendor).
func (v *Vendor) Count(step string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	distinct := map[string]bool{}
	for _, id := range v.objects[step] {
		distinct[id] = true
	}
	return len(distinct)
}

// Fail makes the next call to the step fail this way before the vendor acts
// (sinktest.Vendor).
func (v *Vendor) Fail(step string, kind sinktest.FailKind) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fails[step] = append(v.fails[step], failure{kind: kind})
}

// FailAfter makes the next call to the step create its object and then fail
// this way: a timeout after the vendor acted.
func (v *Vendor) FailAfter(step string, kind sinktest.FailKind) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fails[step] = append(v.fails[step], failure{kind: kind, after: true})
}

// Before sets a function run at the start of every Do, before the vendor
// acts and without the vendor's lock held (a test closes Stop there).
func (v *Vendor) Before(f func(context.Context, api.StepRequest)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.before = f
}

// Calls returns every Do request so far, in order.
func (v *Vendor) Calls() []api.StepRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]api.StepRequest(nil), v.calls...)
}

// Object returns the vendor id held for a step key, if any.
func (v *Vendor) Object(key api.StepKey) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	id, ok := v.objects[key.Step][key]
	return id, ok
}

// OptOut makes the lookup report an opt-out for the email (lowercased).
func (v *Vendor) OptOut(email string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.optOuts[strings.ToLower(email)] = true
}

// SetDeal makes the lookup report a deal at the company: stage is open, won
// or lost.
func (v *Vendor) SetDeal(domain, id, stage string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deals[domain] = deal{id: id, stage: stage}
}

// DealsByCompany makes the lookup read deals per company, as the HubSpot
// lookup does: a company with no open or won deal is reported as deal_lost
// with no deal id (contracts section 5.3).
func (v *Vendor) DealsByCompany() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.byCo = true
}

// FailLookup makes the lookup report the lead as failed.
func (v *Vendor) FailLookup(lead api.LeadID, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failed[lead] = err
}

// FailAllLookups makes every later Lookup call fail as a whole (nil clears it).
func (v *Vendor) FailAllLookups(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lookErr = err
}

// Looked returns the leads of every Lookup call so far.
func (v *Vendor) Looked() [][]api.LeadRef {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([][]api.LeadRef(nil), v.looked...)
}

// LookedIDs returns the ids of every lead any Lookup call was given, sorted.
func (v *Vendor) LookedIDs() []api.LeadID {
	seen := map[api.LeadID]bool{}
	for _, call := range v.Looked() {
		for _, l := range call {
			seen[l.ID] = true
		}
	}
	out := make([]api.LeadID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Err is the error a FailKind maps to, as a real sink returns it.
func Err(kind sinktest.FailKind) error {
	switch kind {
	case sinktest.RateLimited:
		return fmt.Errorf("fake vendor: 429: %w", api.ErrRateLimited)
	case sinktest.Transient:
		return fmt.Errorf("fake vendor: timeout: %w", api.ErrTransient)
	case sinktest.Refused:
		return fmt.Errorf("fake vendor: already in another sequence: %w", api.ErrRefused)
	}
	return errors.New("fake vendor: 400 bad request")
}

type fakeSink struct{ v *Vendor }

func (s fakeSink) Steps(dest string) []string {
	if st, ok := s.v.steps[dest]; ok {
		return append([]string(nil), st...)
	}
	return []string{"push"}
}

func (s fakeSink) Do(ctx context.Context, req api.StepRequest) (string, error) {
	v := s.v
	v.mu.Lock()
	before := v.before
	v.mu.Unlock()
	if before != nil {
		before(ctx, req)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, req)
	var f *failure
	if q := v.fails[req.Key.Step]; len(q) > 0 {
		f, v.fails[req.Key.Step] = &q[0], q[1:]
	}
	if f != nil && !f.after {
		return "", Err(f.kind)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	objs := v.objects[req.Key.Step]
	if objs == nil {
		objs = map[api.StepKey]string{}
		v.objects[req.Key.Step] = objs
	}
	id, found := objs[req.Key]
	if !found && v.reuse[req.Key.Step] {
		for _, rel := range req.Related {
			if rel.Key.Step == req.Key.Step && rel.State == "done" && rel.VendorID != "" {
				id, found = rel.VendorID, true
				objs[req.Key] = id // the same object, now found by this key too
				break
			}
		}
	}
	if !found {
		v.next++
		id = fmt.Sprintf("%s-%d", req.Key.Step, v.next)
		objs[req.Key] = id
	}
	if f != nil {
		return "", Err(f.kind)
	}
	return id, nil
}

type fakeLookup struct{ v *Vendor }

func (l fakeLookup) Lookup(_ context.Context, leads []api.LeadRef) ([]api.Event, map[api.LeadID]error, error) {
	v := l.v
	v.mu.Lock()
	defer v.mu.Unlock()
	v.looked = append(v.looked, append([]api.LeadRef(nil), leads...))
	if v.lookErr != nil {
		return nil, nil, v.lookErr
	}
	var evs []api.Event
	failed := map[api.LeadID]error{}
	domains := map[string]bool{}
	for _, lead := range leads {
		if err, ok := v.failed[lead.ID]; ok {
			failed[lead.ID] = err
			continue
		}
		for _, e := range lead.Emails {
			if v.optOuts[strings.ToLower(e)] {
				evs = append(evs, api.Event{Kind: "optout", Email: e})
			}
		}
		if lead.Domain == "" || domains[lead.Domain] {
			continue
		}
		if d, ok := v.deals[lead.Domain]; ok {
			domains[lead.Domain] = true
			evs = append(evs, api.Event{Kind: "deal_" + d.stage, Domain: lead.Domain,
				Attrs: map[string]string{"deal_id": d.id, "stage": d.stage}})
		} else if v.byCo {
			domains[lead.Domain] = true
			evs = append(evs, api.Event{Kind: "deal_lost", Domain: lead.Domain, Attrs: map[string]string{}})
		}
	}
	return evs, failed, nil
}
