// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package sinktest is the conformance suite a plug-in sink runs against
// itself: every sink is find-or-create by its step
// key, so a step replayed after a crash, or called twice, leaves one
// vendor-side object, and each kind of vendor failure maps to its error.
//
// The signatures use internal/api's names, which are the same types as
// leadscore.Config and leadscore.Sink (the root aliases them), so this package
// never imports the root.
package sinktest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// FailKind is how Vendor.Fail makes a step's next call fail. RateLimited,
// Transient and Refused map to the sink errors of those names; Other is any
// other error.
type FailKind int

// The FailKind values.
const (
	RateLimited FailKind = iota
	Transient
	Refused
	Other
)

func (k FailKind) String() string {
	switch k {
	case RateLimited:
		return "RateLimited"
	case Transient:
		return "Transient"
	case Refused:
		return "Refused"
	}
	return "Other"
}

// Vendor is the fake vendor a sink under test talks to.
type Vendor interface {
	Count(step string) int           // vendor-side objects created for a step
	Fail(step string, kind FailKind) // make the next call to that step fail this way
}

// Harness is what Run needs: a way to build the sink, its fake vendor, and the
// destinations to test.
type Harness struct {
	New    func(cfg api.Config) (api.Sink, error)
	Vendor Vendor // a fake the sink is pointed at
	Dests  []string
}

// Run replays every step after a simulated crash and asserts one vendor-side
// object, calls Do twice with one key, and checks each FailKind maps to its error.
//
// For each destination in Dests, and each of the sink's Steps(dest) in order,
// it checks:
//
//   - crash replay: Do creates one object; a new sink (h.New, as the next
//     process would build it) calls the same key again, having lost the
//     answer, and gets the same vendor id with no second object;
//   - twice with one key: the same sink calls the key once more, and still one
//     object stands;
//   - errors: for each FailKind, Vendor.Fail makes the next call fail, and Do
//     returns ErrRateLimited, ErrTransient or ErrRefused (by errors.Is) for the
//     first three and an error that is none of them for Other.
//
// Each check uses its own lead (its own email and company domain), so a sink
// that keeps one object per company still sees one key per object. A later
// step gets the vendor ids of the earlier steps in Prior.
//
// Call it from an external test package (package apollo_test, not package
// apollo), so a sink inside this module can be tested without an import cycle.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.New == nil || h.Vendor == nil || len(h.Dests) == 0 {
		t.Fatal("sinktest: the harness needs New, Vendor and at least one destination")
	}
	for _, dest := range h.Dests {
		t.Run(dest, func(t *testing.T) { runDest(t, h, dest) })
	}
}

// reporter is the part of testing.T the suite uses, so its own tests can
// check that a broken sink fails it.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// leads numbers the suite's leads across one test binary.
var leads atomic.Int64

// newLead is a lead of its own, at a company of its own.
func newLead() api.LeadRef {
	lead := leads.Add(1)
	domain := fmt.Sprintf("sinktest-%d.example", lead)
	return api.LeadRef{
		ID:       api.LeadID(fmt.Sprintf("00000000-0000-7000-8000-%012d", lead)),
		Emails:   []string{"person@" + domain},
		FullName: fmt.Sprintf("Sinktest Person %d", lead),
		Title:    "Head of Testing",
		Domain:   domain,
		Status:   "new",
		Fields:   map[string]string{"email": "person@" + domain, "full_name": fmt.Sprintf("Sinktest Person %d", lead)},
	}
}

func runDest(t reporter, h Harness, dest string) {
	t.Helper()
	s, err := h.New(api.Config{})
	if err != nil {
		t.Errorf("%s: New: %v", dest, err)
		return
	}
	steps := s.Steps(dest)
	if len(steps) == 0 {
		t.Errorf("%s: Steps returned no steps", dest)
		return
	}
	for i := range steps {
		crashReplay(t, h, dest, steps[:i+1])
		twice(t, h, dest, steps[:i+1])
		for _, kind := range []FailKind{RateLimited, Transient, Refused, Other} {
			failure(t, h, dest, steps[:i+1], kind)
		}
	}
}

// pushTo runs a lead's earlier steps so the last step gets their ids in
// Prior, and returns the request for the last one.
func pushTo(t reporter, s api.Sink, dest string, ref api.LeadRef, steps []string) (api.StepRequest, bool) {
	t.Helper()
	prior := map[string]string{}
	for _, step := range steps[:len(steps)-1] {
		id, err := do(s, request(dest, ref, step, prior))
		if err != nil || id == "" {
			t.Errorf("%s: step %s (before %s): id %q, err %v", dest, step, steps[len(steps)-1], id, err)
			return api.StepRequest{}, false
		}
		prior[step] = id
	}
	return request(dest, ref, steps[len(steps)-1], prior), true
}

func request(dest string, ref api.LeadRef, step string, prior map[string]string) api.StepRequest {
	p := make(map[string]string, len(prior))
	for k, v := range prior {
		p[k] = v
	}
	return api.StepRequest{
		Key:   api.StepKey{LeadID: ref.ID, LaneID: "sinktest", Step: step},
		Dest:  dest,
		Lead:  ref,
		Prior: p,
	}
}

// do calls the sink with the 30-second bound every outbound call has.
func do(s api.Sink, req api.StepRequest) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Do(ctx, req)
}

// crashReplay: the step is called, the answer is lost, and the next process
// calls the same key again.
func crashReplay(t reporter, h Harness, dest string, steps []string) {
	t.Helper()
	step := steps[len(steps)-1]
	s, err := h.New(api.Config{})
	if err != nil {
		t.Errorf("%s/%s: New: %v", dest, step, err)
		return
	}
	req, ok := pushTo(t, s, dest, newLead(), steps)
	if !ok {
		return
	}
	before := h.Vendor.Count(step)
	first, err := do(s, req)
	if err != nil || first == "" {
		t.Errorf("%s/%s: first call: id %q, err %v", dest, step, first, err)
		return
	}
	if got := h.Vendor.Count(step); got != before+1 {
		t.Errorf("%s/%s: the first call made %d objects, want 1", dest, step, got-before)
	}
	// The crash: the process dies before saving the id, and the next run
	// builds a new sink and calls the same key.
	again, err := h.New(api.Config{})
	if err != nil {
		t.Errorf("%s/%s: New after the crash: %v", dest, step, err)
		return
	}
	second, err := do(again, req)
	if err != nil {
		t.Errorf("%s/%s: the replay after a crash failed: %v", dest, step, err)
		return
	}
	if second != first {
		t.Errorf("%s/%s: the replay after a crash returned %q, want the first object %q", dest, step, second, first)
	}
	if got := h.Vendor.Count(step); got != before+1 {
		t.Errorf("%s/%s: after the replay %d objects stand for one key, want 1", dest, step, got-before)
	}
}

// twice: the same sink calls one key two times.
func twice(t reporter, h Harness, dest string, steps []string) {
	t.Helper()
	step := steps[len(steps)-1]
	s, err := h.New(api.Config{})
	if err != nil {
		t.Errorf("%s/%s: New: %v", dest, step, err)
		return
	}
	req, ok := pushTo(t, s, dest, newLead(), steps)
	if !ok {
		return
	}
	before := h.Vendor.Count(step)
	a, errA := do(s, req)
	b, errB := do(s, req)
	if errA != nil || errB != nil {
		t.Errorf("%s/%s: two calls with one key: %v, %v", dest, step, errA, errB)
		return
	}
	if a != b {
		t.Errorf("%s/%s: two calls with one key returned %q and %q, want one object", dest, step, a, b)
	}
	if got := h.Vendor.Count(step); got != before+1 {
		t.Errorf("%s/%s: two calls with one key made %d objects, want 1", dest, step, got-before)
	}
}

// failure: the vendor fails the next call this way, and Do returns the
// matching error.
func failure(t reporter, h Harness, dest string, steps []string, kind FailKind) {
	t.Helper()
	step := steps[len(steps)-1]
	s, err := h.New(api.Config{})
	if err != nil {
		t.Errorf("%s/%s: New: %v", dest, step, err)
		return
	}
	req, ok := pushTo(t, s, dest, newLead(), steps)
	if !ok {
		return
	}
	h.Vendor.Fail(step, kind)
	_, err = do(s, req)
	want := map[FailKind]error{RateLimited: api.ErrRateLimited, Transient: api.ErrTransient, Refused: api.ErrRefused}[kind]
	switch {
	case err == nil:
		t.Errorf("%s/%s: a %s failure returned no error", dest, step, kind)
	case want != nil && !errors.Is(err, want):
		t.Errorf("%s/%s: a %s failure returned %v, want an error that is %v", dest, step, kind, err, want)
	case want == nil && (errors.Is(err, api.ErrRateLimited) || errors.Is(err, api.ErrTransient) || errors.Is(err, api.ErrRefused)):
		t.Errorf("%s/%s: an Other failure returned %v, which the engine would not count as an attempt", dest, step, err)
	}
}
