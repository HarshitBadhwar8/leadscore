package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// The one-cold-push rule row by row (the ledger rules).
func TestHoldsCold(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		p    model.Push
		cold bool
		want bool
	}{
		{"pending, never called", model.Push{State: statePending}, true, false},
		{"pending, marked (intent_run)", model.Push{State: statePending, IntentRun: "run-1"}, true, true},
		{"pending, called", model.Push{State: statePending, CalledAt: now}, true, true},
		{"done", model.Push{State: stateDone, CalledAt: now}, true, true},
		{"failed", model.Push{State: stateFailed, CalledAt: now}, true, true},
		{"cancelled, never called", model.Push{State: stateCancelled}, true, false},
		{"cancelled, marked", model.Push{State: stateCancelled, IntentRun: "run-1"}, true, true},
		{"cancelled, called", model.Push{State: stateCancelled, CalledAt: now}, true, true},
		{"non-cold, done", model.Push{State: stateDone, CalledAt: now}, false, false},
	}
	for _, c := range cases {
		if got := holdsCold(c.p, c.cold); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// unitRun is a Run over a bare model with the lane rubric, for view tests.
func unitRun(t *testing.T, m *model.Model) *Run {
	t.Helper()
	rb, err := rules.Compile([]byte(laneRubric))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return &Run{ID: "this-run", Model: m, Rubric: rb, Config: &config.Config{}, Now: func() time.Time { return now }}
}

func person(m *model.Model, id api.LeadID, domain string) {
	m.Put(model.TablePeople, model.Person{LeadID: id, CreatedAt: time.Now().UTC(),
		Fields: map[string]model.Field{model.CompanyDomainField: {Value: domain}}})
}

// The deal rule and intent_run: a deal step this run marked but has not
// called is no deal yet; one another run left marked is; a called one is,
// until a later lookup finds the deal lost; a contact step never is; the
// lead's own lane is left out.
func TestDealRuleWithIntentRows(t *testing.T) {
	now := time.Now().UTC()
	for _, c := range []struct {
		name   string
		row    model.Push
		lostAt time.Time
		except string
		want   bool
	}{
		{"marked by this run, not called", model.Push{Step: "deal", IntentRun: "this-run"}, time.Time{}, "", false},
		{"marked by another run", model.Push{Step: "deal", IntentRun: "other-run"}, time.Time{}, "", true},
		{"called", model.Push{Step: "deal", CalledAt: now}, time.Time{}, "", true},
		{"called, then found lost", model.Push{Step: "deal", CalledAt: now}, now.Add(time.Minute), "", false},
		{"called, lost found before the call", model.Push{Step: "deal", CalledAt: now}, now.Add(-time.Minute), "", true},
		{"contact step called", model.Push{Step: "contact", CalledAt: now}, time.Time{}, "", false},
		{"own lane", model.Push{Step: "deal", CalledAt: now}, time.Time{}, "warm", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := model.New()
			person(m, "a", "acme.example")
			person(m, "b", "acme.example")
			row := c.row
			row.LeadID, row.LaneID, row.LaneKind, row.Dest, row.State = "a", "warm", kindNonCold, "deals", statePending
			m.Put(model.TablePushes, row)
			if !c.lostAt.IsZero() {
				m.Put(model.TableOutcomes, model.Outcome{LeadID: "b", DealStage: "lost", DealCheckedAt: c.lostAt})
			}
			v := newView(unitRun(t, m))
			lead := api.LeadID("b")
			if c.except != "" {
				lead = "a"
			}
			if got := v.deal(lead, c.except); got != c.want {
				t.Errorf("deal %v, want %v", got, c.want)
			}
		})
	}
}

// Review 9: a deal event from a lookup is stamped with the run's clock, so
// deal_checked_at is the lookup time whatever time the vendor gave.
func TestLookupDealEventStampedWithTheRunClock(t *testing.T) {
	m := model.New()
	person(m, "a", "acme.example")
	r := unitRun(t, m)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	applyLookupEvents(r, "hubspot", []api.Event{{Kind: "deal_lost", Domain: "acme.example", At: old, ReceivedAt: old}}, nil)
	if got := m.Outcomes["a"].DealCheckedAt; !got.Equal(r.Now().Truncate(time.Millisecond)) {
		t.Errorf("deal_checked_at %v, want the run's clock %v", got, r.Now())
	}
}

// Review 10: Related keeps a colleague's done deal step even when its lane
// has left the rubric (its deal is still the company's).
func TestRelatedKeepsDealsOfRemovedLanes(t *testing.T) {
	m := model.New()
	person(m, "a", "acme.example")
	person(m, "b", "acme.example")
	m.Put(model.TablePushes, model.Push{LeadID: "a", LaneID: "gone", Step: "deal", LaneKind: kindNonCold, Dest: "deals", State: stateDone, VendorID: "D1"})
	m.Put(model.TablePushes, model.Push{LeadID: "a", LaneID: "gone-seq", Step: "enroll", LaneKind: kindCold, Dest: "sequence/X", State: stateDone, VendorID: "E1"})
	v := newView(unitRun(t, m))
	rel := v.related("b", dealSink)
	if len(rel) != 1 || rel[0].VendorID != "D1" {
		t.Errorf("Related %v, want the removed lane's deal", rel)
	}
	if got := v.related("b", apolloSink); len(got) != 0 {
		t.Errorf("a removed lane's row of an unknown sink: %v", got)
	}
}

// Final review 1: a done deal step holds its company even after its lane is
// edited to push to hubspot:contacts; the row's own step and destination
// decide, not what the lane says now.
func TestDealRowSurvivesLaneEdit(t *testing.T) {
	m := model.New()
	person(m, "a", "acme.example")
	person(m, "b", "acme.example")
	m.Put(model.TablePushes, model.Push{LeadID: "a", LaneID: "warm", Step: "deal", LaneKind: kindNonCold, Dest: "deals",
		State: stateDone, VendorID: "D1", CalledAt: time.Now().UTC()})
	r := unitRun(t, m)
	rb, err := rules.Compile([]byte(strings.Replace(laneRubric, `push: "hubspot:deals"`, `push: "hubspot:contacts"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.Rubric = rb
	if !newView(r).deal("b", "") {
		t.Error("editing the lane to contacts dropped the company's deal hold")
	}
}

// Final review 3: a lookup that failed for some leads releases none of their
// companies: its deal_lost for a domain with a failed lead is dropped.
func TestPartialLookupFailureKeepsDealHolds(t *testing.T) {
	m := model.New()
	person(m, "a", "acme.example")
	person(m, "c", "cyan.example")
	m.Put(model.TableOutcomes, model.Outcome{LeadID: "a", DealID: "D1", DealStage: "open"})
	m.Put(model.TableOutcomes, model.Outcome{LeadID: "c", DealID: "D2", DealStage: "open"})
	r := unitRun(t, m)
	evs := []api.Event{
		{Kind: "deal_lost", Domain: "acme.example", Attrs: map[string]string{}},
		{Kind: "deal_lost", Domain: "cyan.example", Attrs: map[string]string{}},
	}
	applyLookupEvents(r, "hubspot", evs, map[api.LeadID]error{"a": errors.New("timeout")})
	if s := m.Outcomes["a"].DealStage; s != "open" {
		t.Errorf("acme (a lead's lookup failed) was released: %q", s)
	}
	if s := m.Outcomes["c"].DealStage; s != "lost" {
		t.Errorf("cyan was not released: %q", s)
	}
}
