package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakesink "github.com/HarshitBadhwar8/leadscore/internal/fakes/sink"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// laneRubric has one lane of each kind and both vendors' rule-bearing
// destinations: a non-cold HubSpot deals lane for positive replies, a cold
// Apollo sequence lane for tier 1 (two steps), a lower cold lane on the
// plain fake sink for everyone, and an export lane.
const laneRubric = `version: 1
derive:
  tier:
    - when: { field: title, contains: head }
      then: 1
    - else: 2
score:
  contact:
    - { when: { field: title, contains: head }, points: 5 }
lanes:
  - { id: warm, kind: non-cold, priority: 30, when: { field: status, eq: replied_positive }, push: "hubspot:deals" }
  - id: seq-a
    kind: cold
    priority: 20
    when: { all: [ { field: tier, eq: 1 }, { field: receiver_only, eq: false } ] }
    push: "apollo:sequence/A"
  - { id: seq-b, kind: cold, priority: 10, when: { field: receiver_only, eq: false }, push: "fake:b" }
  - { id: list, kind: export, priority: 1, when: { field: tier, lte: 2 }, push: "export:list" }
`

// laneConfig is the world's leadscore.yml body: the CSV source, the three
// fake sinks, pushes on.
const laneConfig = `sources:
  - { id: leads, type: csv, path: leads.csv }
sinks:
  apollo: {}
  hubspot: {}
  fake: {}
pushes_enabled: true
`

// world is an install with fake sinks (and, when enabled, fake lookups) behind
// the apollo, hubspot and fake sink types.
type world struct {
	*install
	apollo, hubspot, fake *fakesink.Vendor
	lookups               map[string]*fakesink.Vendor
	clock                 func() time.Time
	cfg                   string // leadscore.yml's body as last written by config
	reread                func(*Run) ([]api.LeadID, error)
}

// newWorld writes the config, the lane rubric and leads.csv (lines after
// the header Email,Name,Title,Domain).
func newWorld(t *testing.T, leads ...string) *world {
	t.Helper()
	w := &world{
		install: newInstall(t, laneConfig, laneRubric),
		apollo:  fakesink.New(map[string][]string{"sequence/A": {"contact", "enroll"}}),
		hubspot: fakesink.New(map[string][]string{"deals": {"contact", "deal"}, "contacts": {"contact"}}),
		fake:    fakesink.New(nil),
		lookups: map[string]*fakesink.Vendor{},
	}
	w.leads(leads...)
	sinks := map[string]*fakesink.Vendor{"apollo": w.apollo, "hubspot": w.hubspot, "fake": w.fake}
	oldSink, oldLookup := sinkFactory, lookupFactory
	sinkFactory = func(typ string) (func(api.Config) (api.Sink, error), bool) {
		if v, ok := sinks[typ]; ok {
			return func(api.Config) (api.Sink, error) { return v.Sink(), nil }, true
		}
		return api.SinkFactory(typ)
	}
	lookupFactory = func(typ string) (func(api.Config) (api.Lookup, error), bool) {
		if v, ok := w.lookups[typ]; ok {
			return func(api.Config) (api.Lookup, error) { return v.Lookup(), nil }, true
		}
		return nil, false
	}
	t.Cleanup(func() { sinkFactory, lookupFactory = oldSink, oldLookup })
	return w
}

// leads rewrites leads.csv.
func (w *world) leads(lines ...string) {
	w.write("leads.csv", csvText(append([]string{"Email,Name,Title,Domain"}, lines...)...))
}

// lookup turns on a fake lookup for a sink type (its vendor's lookup).
func (w *world) lookup(typ string) *fakesink.Vendor {
	v := map[string]*fakesink.Vendor{"apollo": w.apollo, "hubspot": w.hubspot, "fake": w.fake}[typ]
	w.lookups[typ] = v
	return v
}

// run executes one run with DefaultHooks (and the world's ReRead hook, when
// a test sets one in place of the default).
func (w *world) run(mod ...func(*api.RunOptions, *settings)) (api.RunResult, string, error) {
	w.t.Helper()
	hooks := DefaultHooks()
	if w.reread != nil {
		hooks.ReRead = w.reread
	}
	mods := append([]func(*api.RunOptions, *settings){func(_ *api.RunOptions, s *settings) {
		if w.clock != nil {
			s.now = w.clock
		}
	}}, mod...)
	return w.install.run(hooks, mods...)
}

// mustRun runs and fails the test on a run error.
func (w *world) mustRun(mod ...func(*api.RunOptions, *settings)) (api.RunResult, string) {
	w.t.Helper()
	res, out, err := w.run(mod...)
	if err != nil {
		w.t.Fatalf("run: %v\n%s", err, out)
	}
	return res, out
}

// id returns the lead id an email belongs to.
func (w *world) id(email string) api.LeadID {
	w.t.Helper()
	for _, r := range w.rows(model.TableIdentities) {
		if r["key"] == strings.ToLower(email) {
			return api.LeadID(r["lead_id"])
		}
	}
	w.t.Fatalf("no lead has %s", email)
	return ""
}

// push returns a ledger row, or nil.
func (w *world) push(email, lane, step string) api.Row {
	w.t.Helper()
	id := string(w.id(email))
	for _, r := range w.rows(model.TablePushes) {
		if r["lead_id"] == id && r["lane_id"] == lane && r["step"] == step {
			return r
		}
	}
	return nil
}

// outcome returns a lead's Outcomes row.
func (w *world) outcome(email string) api.Row {
	w.t.Helper()
	id := string(w.id(email))
	for _, r := range w.rows(model.TableOutcomes) {
		if r["lead_id"] == id {
			return r
		}
	}
	return api.Row{}
}

// ranked returns a lead's Ranked row.
func (w *world) ranked(email string) api.Row {
	w.t.Helper()
	for _, r := range w.rows(model.TableRanked) {
		if r["email"] == strings.ToLower(email) {
			return r
		}
	}
	return api.Row{}
}

// edit loads the store, changes the model, and commits the change, as
// another process (a person, the CLI, an earlier run) would.
func (w *world) edit(f func(m *model.Model)) {
	w.t.Helper()
	s := w.store()
	m, err := codec.Load(context.Background(), s)
	if err != nil {
		w.t.Fatal(err)
	}
	f(m)
	writes := codec.Encode(m)
	if len(writes) == 0 {
		return
	}
	if err := s.Commit(context.Background(), writes); err != nil {
		w.t.Fatal(err)
	}
}

// override appends an Overrides row.
func (w *world) override(person, action, value, note string) {
	w.edit(func(m *model.Model) {
		m.Put(model.TableOverrides, model.Override{Person: person, Action: action, Value: value, Note: note})
	})
}

// calls lists a vendor's Do calls as "email lane step", in order.
func calls(v *fakesink.Vendor) []string {
	var out []string
	for _, c := range v.Calls() {
		email := ""
		if len(c.Lead.Emails) > 0 {
			email = c.Lead.Emails[0]
		}
		out = append(out, fmt.Sprintf("%s %s %s", email, c.Key.LaneID, c.Key.Step))
	}
	return out
}

// pushedTo lists the leads (by email) a vendor holds an object for, for a step.
func pushedTo(w *world, v *fakesink.Vendor, step string) []string {
	byID := map[api.LeadID]string{}
	for _, r := range w.rows(model.TableIdentities) {
		byID[api.LeadID(r["lead_id"])] = r["key"]
	}
	seen := map[string]bool{}
	for _, c := range v.Calls() {
		if _, ok := v.Object(c.Key); ok && c.Key.Step == step {
			seen[byID[c.Key.LeadID]] = true
		}
	}
	var out []string
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// rubric rewrites rubric.yml with old replaced by repl.
func (w *world) rubric(old, repl string) {
	w.t.Helper()
	if !strings.Contains(laneRubric, old) {
		w.t.Fatalf("the lane rubric has no %q", old)
	}
	w.write("rubric.yml", strings.Replace(laneRubric, old, repl, 1))
}

// config rewrites leadscore.yml with old replaced by repl.
func (w *world) config(old, repl string) {
	w.t.Helper()
	if w.cfg == "" {
		w.cfg = laneConfig
	}
	if !strings.Contains(w.cfg, old) {
		w.t.Fatalf("the config has no %q:\n%s", old, w.cfg)
	}
	w.cfg = strings.Replace(w.cfg, old, repl, 1)
	w.install.config(w.cfg)
}

func stopAfter(stop chan struct{}) func(*api.RunOptions, *settings) {
	return func(o *api.RunOptions, _ *settings) { o.Stop = stop }
}

func coldPushes(w *world, email string) []string {
	var lanes []string
	for _, r := range w.rows(model.TablePushes) {
		if r["lead_id"] == string(w.id(email)) && r["lane_kind"] == kindCold && holdsCold(mustPush(w.t, r), true) {
			lanes = append(lanes, r["lane_id"])
		}
	}
	sort.Strings(lanes)
	return dedupe(lanes)
}

func mustPush(t *testing.T, r api.Row) model.Push {
	t.Helper()
	m := model.New()
	if err := m.Load(model.TablePushes, []api.Row{r}); err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Pushes {
		return p
	}
	return model.Push{}
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || xs[i-1] != x {
			out = append(out, x)
		}
	}
	return out
}

// closeStop closes Stop once, as SIGTERM does, and gives the run's stop
// watcher a moment to cancel PushCtx, so the next call check sees it.
func closeStop(stop chan struct{}) {
	select {
	case <-stop:
		return
	default:
	}
	close(stop)
	time.Sleep(50 * time.Millisecond)
}
