// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package api

import (
	"strings"
	"testing"
)

func mustPanic(t *testing.T, wantSubstr string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", wantSubstr)
		}
		if s, _ := r.(string); !strings.Contains(s, wantSubstr) {
			t.Fatalf("panic = %v, want it to contain %q", r, wantSubstr)
		}
	}()
	f()
}

func TestRegisterDuplicatePanicsForEveryKind(t *testing.T) {
	cases := []struct {
		name     string
		register func()
	}{
		{"source", func() { RegisterSource("dup-test", func(Config) (Source, error) { return nil, nil }) }},
		{"enricher", func() { RegisterEnricher("dup-test", func(Config) (Enricher, error) { return nil, nil }) }},
		{"poller", func() { RegisterPoller("dup-test", func(Config) (Poller, error) { return nil, nil }) }},
		{"lookup", func() { RegisterLookup("dup-test", func(Config) (Lookup, error) { return nil, nil }) }},
		{"sink", func() { RegisterSink("dup-test", func(Config) (Sink, error) { return nil, nil }) }},
		{"detector", func() { RegisterDetector("dup-test", func(Config) (Detector, error) { return nil, nil }) }},
		{"backend", func() {
			RegisterBackend("dup-test", func(Config) (Backend, EventLog, error) { return nil, nil, nil })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Register once per process, so the test also holds under -count=N.
			if !contains(RegisteredTypes(c.name), "dup-test") {
				c.register()
			}
			if !contains(RegisteredTypes(c.name), "dup-test") {
				t.Fatalf("RegisteredTypes(%q) = %v, want it to hold dup-test", c.name, RegisteredTypes(c.name))
			}
			mustPanic(t, "called twice", c.register)
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestRegisterRejectsEmptyNameAndNilFactory(t *testing.T) {
	mustPanic(t, "empty type", func() {
		RegisterSink("", func(Config) (Sink, error) { return nil, nil })
	})
	mustPanic(t, "nil factory", func() { RegisterSink("nil-test", nil) })
	if _, ok := SinkFactory("nil-test"); ok {
		t.Fatal("a nil factory must not be registered")
	}
}

func TestFactoryLookupMissing(t *testing.T) {
	if _, ok := SourceFactory("never-registered"); ok {
		t.Fatal("lookup of an unregistered type must report false")
	}
	if got := RegisteredTypes("no-such-kind"); got != nil {
		t.Fatalf("RegisteredTypes(unknown kind) = %v, want nil", got)
	}
}

// Detector kinds are stored lowercased, as the rubric compiler lowercases them,
// and two that differ only in case collide.
func TestDetectorKindsAreLowercased(t *testing.T) {
	RegisterDetector("Mixed_Kind_Test", func(Config) (Detector, error) { return nil, nil })
	if _, ok := DetectorFactory("mixed_kind_test"); !ok {
		t.Error("a kind registered with capitals is not found by its lowercase name")
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a kind differing only in case must panic")
		}
	}()
	RegisterDetector("MIXED_KIND_TEST", func(Config) (Detector, error) { return nil, nil })
}
