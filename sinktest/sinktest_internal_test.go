// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sinktest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// recorder collects what the suite reports, so these tests can check the
// suite fails a broken sink.
type recorder struct{ errs []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, a ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, a...))
}

// vendor is a small sink with switches that break it in one way each.
type vendor struct {
	mu        sync.Mutex
	objects   map[string]map[api.StepKey]string
	n         int
	fail      map[string]FailKind
	createNew bool // never finds: a second call creates a second object
	wrongErr  bool // maps every failure to a plain error
	forget    bool // a new sink instance does not see the old objects
}

func (v *vendor) Count(step string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.objects[step])
}
func (v *vendor) Fail(step string, k FailKind) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail[step] = k
}

type sink struct {
	v    *vendor
	seen map[api.StepKey]string // what this instance created, for forget
}

func (s *sink) Steps(string) []string { return []string{"one", "two"} }
func (s *sink) Do(_ context.Context, req api.StepRequest) (string, error) {
	v := s.v
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.fail[req.Key.Step]; ok {
		delete(v.fail, req.Key.Step)
		if v.wrongErr {
			return "", errors.New("boom")
		}
		return "", map[FailKind]error{RateLimited: api.ErrRateLimited, Transient: api.ErrTransient, Refused: api.ErrRefused, Other: errors.New("400")}[k]
	}
	if req.Key.Step == "two" && req.Prior["one"] == "" {
		return "", errors.New("no prior")
	}
	if v.objects[req.Key.Step] == nil {
		v.objects[req.Key.Step] = map[api.StepKey]string{}
	}
	lookup := v.objects[req.Key.Step]
	if v.forget {
		if id, ok := s.seen[req.Key]; ok {
			return id, nil
		}
	} else if id, ok := lookup[req.Key]; ok && !v.createNew {
		return id, nil
	}
	v.n++
	id := fmt.Sprintf("id-%d", v.n)
	k := req.Key
	if v.createNew || v.forget {
		k.LaneID = fmt.Sprintf("%s#%d", k.LaneID, v.n) // a separate object each time
	}
	lookup[k] = id
	s.seen[req.Key] = id
	return id, nil
}

func check(v *vendor) []string {
	v.objects, v.fail = map[string]map[api.StepKey]string{}, map[string]FailKind{}
	r := &recorder{}
	runDest(r, Harness{New: func(api.Config) (api.Sink, error) { return &sink{v: v, seen: map[api.StepKey]string{}}, nil }, Vendor: v, Dests: []string{"d"}}, "d")
	return r.errs
}

func TestSuitePassesAGoodSink(t *testing.T) {
	if errs := check(&vendor{}); len(errs) > 0 {
		t.Errorf("a good sink failed: %v", errs)
	}
}

func TestSuiteCatchesBrokenSinks(t *testing.T) {
	cases := map[string]struct {
		v    *vendor
		want string
	}{
		"creates on every call": {&vendor{createNew: true}, "two calls with one key"},
		"forgets after a crash": {&vendor{forget: true}, "after the replay"},
		"maps failures wrongly": {&vendor{wrongErr: true}, "failure returned boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			errs := check(c.v)
			if !strings.Contains(strings.Join(errs, "\n"), c.want) {
				t.Errorf("the suite missed it; reported %v", errs)
			}
		})
	}
}
