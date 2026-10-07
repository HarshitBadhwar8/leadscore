package api

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// registry maps an adapter type name to its factory. Registration normally
// happens in init functions, but a mutex keeps a late registration safe.
type registry[F any] struct {
	kind string
	mu   sync.RWMutex
	m    map[string]F
}

func newRegistry[F any](kind string) *registry[F] {
	return &registry[F]{kind: kind, m: map[string]F{}}
}

// register panics on an empty name, a nil factory, or a name already taken: a
// duplicate means two packages claim one config `type:`, and silently keeping
// either would run the wrong adapter.
func (r *registry[F]) register(name string, f F, isNil bool) {
	if name == "" {
		panic(fmt.Sprintf("leadscore: Register%s with an empty type", r.kind))
	}
	if isNil {
		panic(fmt.Sprintf("leadscore: Register%s(%q) with a nil factory", r.kind, name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("leadscore: Register%s called twice for type %q", r.kind, name))
	}
	r.m[name] = f
}

func (r *registry[F]) lookup(name string) (F, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.m[name]
	return f, ok
}

func (r *registry[F]) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	sources   = newRegistry[func(Config) (Source, error)]("Source")
	enrichers = newRegistry[func(Config) (Enricher, error)]("Enricher")
	pollers   = newRegistry[func(Config) (Poller, error)]("Poller")
	lookups   = newRegistry[func(Config) (Lookup, error)]("Lookup")
	sinks     = newRegistry[func(Config) (Sink, error)]("Sink")
	detectors = newRegistry[func(params Config) (Detector, error)]("Detector")
	backends  = newRegistry[func(Config) (Backend, EventLog, error)]("Backend")
)

// Each Register* panics on an empty type, a nil factory, or a type already
// registered.
func RegisterSource(typ string, f func(Config) (Source, error)) {
	sources.register(typ, f, f == nil)
}

func RegisterEnricher(typ string, f func(Config) (Enricher, error)) {
	enrichers.register(typ, f, f == nil)
}

func RegisterPoller(typ string, f func(Config) (Poller, error)) {
	pollers.register(typ, f, f == nil)
}

func RegisterLookup(typ string, f func(Config) (Lookup, error)) {
	lookups.register(typ, f, f == nil)
}

func RegisterSink(typ string, f func(Config) (Sink, error)) {
	sinks.register(typ, f, f == nil)
}

// RegisterDetector stores the kind lowercased, as the rubric compares detector
// kinds (contracts section 2): two kinds that differ only in case collide and
// panic as a duplicate.
func RegisterDetector(kind string, f func(params Config) (Detector, error)) {
	detectors.register(strings.ToLower(kind), f, f == nil)
}

func RegisterBackend(typ string, f func(Config) (Backend, EventLog, error)) {
	backends.register(typ, f, f == nil)
}

// The lookups below are for the engine; the root package does not re-export them.

func SourceFactory(typ string) (func(Config) (Source, error), bool) { return sources.lookup(typ) }

func EnricherFactory(typ string) (func(Config) (Enricher, error), bool) {
	return enrichers.lookup(typ)
}

func PollerFactory(typ string) (func(Config) (Poller, error), bool) { return pollers.lookup(typ) }

func LookupFactory(typ string) (func(Config) (Lookup, error), bool) { return lookups.lookup(typ) }

func SinkFactory(typ string) (func(Config) (Sink, error), bool) { return sinks.lookup(typ) }

func DetectorFactory(kind string) (func(params Config) (Detector, error), bool) {
	return detectors.lookup(strings.ToLower(kind))
}

func BackendFactory(typ string) (func(Config) (Backend, EventLog, error), bool) {
	return backends.lookup(typ)
}

// RegisteredTypes lists the registered names of one adapter kind, sorted. kind
// is one of source, enricher, poller, lookup, sink, detector, backend.
func RegisteredTypes(kind string) []string {
	switch kind {
	case "source":
		return sources.names()
	case "enricher":
		return enrichers.names()
	case "poller":
		return pollers.names()
	case "lookup":
		return lookups.names()
	case "sink":
		return sinks.names()
	case "detector":
		return detectors.names()
	case "backend":
		return backends.names()
	}
	return nil
}
