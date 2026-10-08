// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package detect

import (
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func win(key, lead, domain, kind string, at time.Time) model.WindowEvent {
	subj := "company"
	if lead != "" {
		subj = "lead"
	}
	return model.WindowEvent{EventKey: key, Subject: subj, LeadID: api.LeadID(lead), Domain: domain, Kind: kind, At: at}
}

func setup(t *testing.T, ws ...model.WindowEvent) (*model.Model, []api.LeadRef) {
	t.Helper()
	m := model.New()
	for _, id := range []string{"A", "B"} {
		m.Put(model.TablePeople, model.Person{LeadID: api.LeadID(id), CreatedAt: now.Add(-100 * day)})
	}
	m.Put(model.TablePeople, model.Person{LeadID: "OLD", CreatedAt: now.Add(-100 * day), MergedInto: "A"})
	for _, w := range ws {
		m.Put(model.TableWindowEvents, w)
	}
	return m, []api.LeadRef{{ID: "A", Domain: "example.com"}, {ID: "B", Domain: "example.com"}}
}

// Windows are (now - window, now]: an event exactly a window ago is out, one
// at now is in, and a future one is out.
func TestCountInWindowBoundaries(t *testing.T) {
	ws := []model.WindowEvent{
		win("1", "A", "", "visit_site", now.Add(-7*day)),                // exactly at the edge: out
		win("2", "A", "", "visit_site", now.Add(-7*day+time.Second)),    // in
		win("3", "A", "", "VISIT_Pricing", now),                         // in, matched lowercased
		win("4", "A", "", "visit_site", now.Add(time.Second)),           // future: out
		win("5", "A", "", "sent", now.Add(-time.Hour)),                  // another kind
		win("6", "OLD", "", "visit_docs", now.Add(-time.Hour)),          // a merged lead's event counts for A
		win("7", "B", "example.com", "visit_site", now.Add(-time.Hour)), // B's own
	}
	m, leads := setup(t, ws...)
	spec := func(least int) rules.DetectorSpec {
		return rules.DetectorSpec{Name: "hot", Kind: "count_in_window", Subject: "lead", Event: "Visit_*", Window: 7 * day, Min: least}
	}
	for min, want := range map[int]bool{3: true, 4: false} {
		res, errs := Evaluate([]rules.DetectorSpec{spec(min)}, m, leads, now)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		if res.Leads["A"]["hot"] != want {
			t.Errorf("min %d: fired %v, want %v", min, res.Leads["A"]["hot"], want)
		}
	}
}

// A company-subject detector sees every event at the domain, lead events
// included.
func TestCompanySubjectSeesLeadEvents(t *testing.T) {
	m, leads := setup(t,
		win("1", "A", "example.com", "visit_site", now.Add(-day)),
		win("2", "B", "example.com", "visit_site", now.Add(-day)),
		win("3", "", "example.com", "visit_site", now.Add(-day)),
		win("4", "", "example.org", "visit_site", now.Add(-day)),
	)
	res, _ := Evaluate([]rules.DetectorSpec{{Name: "busy", Kind: "count_in_window", Subject: "company", Event: "visit_site", Window: 7 * day, Min: 3}}, m, leads, now)
	if !res.Companies["example.com"]["busy"] || res.Companies["example.org"]["busy"] {
		t.Errorf("%+v", res.Companies)
	}
	if len(res.Leads) != 0 {
		t.Errorf("a company detector fired for leads: %+v", res.Leads)
	}
}

func TestFirstSeen(t *testing.T) {
	m, leads := setup(t)
	p := m.People["A"]
	p.FirstSeen = map[string]time.Time{"visit_pricing": now.Add(-2 * day), "visit_docs": now.Add(-40 * day)}
	m.Put(model.TablePeople, p)
	p = m.People["B"]
	p.FirstSeen = map[string]time.Time{"visit_pricing": now.Add(-20 * day)}
	m.Put(model.TablePeople, p)
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "example.com", FirstSeen: map[string]time.Time{"visit_pricing": now.Add(-day)}})

	specs := []rules.DetectorSpec{
		{Name: "new_pricing", Kind: "first_seen", Subject: "lead", Event: "visit_pricing", Within: 7 * day},
		{Name: "new_any", Kind: "first_seen", Subject: "lead", Event: "visit_*", Within: 7 * day}, // earliest matching kind decides
		{Name: "new_co", Kind: "first_seen", Subject: "company", Event: "visit_pricing", Within: 7 * day},
	}
	res, _ := Evaluate(specs, m, leads, now)
	if !res.Leads["A"]["new_pricing"] || res.Leads["B"]["new_pricing"] {
		t.Errorf("new_pricing: %+v", res.Leads)
	}
	if res.Leads["A"]["new_any"] {
		t.Error("A first visited 40 days ago; visit_* is not new")
	}
	if !res.Companies["example.com"]["new_co"] {
		t.Errorf("company first_seen: %+v", res.Companies)
	}
}

func TestChange(t *testing.T) {
	m, leads := setup(t)
	m.Put(model.TableCompanyFacts, model.CompanyFact{Domain: "example.com",
		Facts:    map[string]model.Fact{"funding_stage": {Value: "Series B", Origin: "enrichment", At: now.Add(-3 * day)}, "name": {Value: "Ex", At: now.Add(-day)}},
		Previous: map[string]model.Fact{"funding_stage": {Value: "Series A", Origin: "enrichment", At: now.Add(-200 * day)}},
	})
	s := func(from, to *string, within time.Duration) rules.DetectorSpec {
		return rules.DetectorSpec{Name: "raised", Kind: "change", Subject: "company", Field: "funding_stage", Within: within, From: from, To: to}
	}
	str := func(v string) *string { return &v }
	for _, c := range []struct {
		spec rules.DetectorSpec
		want bool
	}{
		{s(nil, nil, 7*day), true},
		{s(str("series a"), str("SERIES B"), 7*day), true},
		{s(str("Seed"), nil, 7*day), false},
		{s(nil, nil, 2*day), false},
	} {
		res, _ := Evaluate([]rules.DetectorSpec{c.spec}, m, leads, now)
		if res.Companies["example.com"]["raised"] != c.want {
			t.Errorf("%+v: got %v", c.spec, !c.want)
		}
	}
	// A fact set for the first time is not a change.
	res, _ := Evaluate([]rules.DetectorSpec{{Name: "renamed", Kind: "change", Subject: "lead", Field: "name", Within: 7 * day}}, m, leads, now)
	if res.Leads["A"]["renamed"] {
		t.Error("a first value fired change")
	}
}

type evenDetector struct{}

func (evenDetector) Name() string { return "even" }
func (evenDetector) Evaluate(_ api.Subject, evs []api.Event, _ time.Time) (bool, []api.EventID) {
	return len(evs) > 0 && len(evs)%2 == 0, nil
}

func init() {
	// Registered with capitals: the registry stores kinds lowercased, as the
	// compiler gives them.
	api.RegisterDetector("Test_Even_Count", func(api.Config) (api.Detector, error) { return evenDetector{}, nil })
}

// A registered kind receives the subject's window events; a missing one does
// not fire and is reported. Kind names arrive lowercased from the compiler.
func TestRegisteredAndMissingKinds(t *testing.T) {
	m, leads := setup(t, win("1", "A", "", "x", now), win("2", "A", "", "y", now), win("3", "B", "", "x", now))
	specs := []rules.DetectorSpec{
		{Name: "even", Kind: "test_even_count", Subject: "lead"},
		{Name: "gone", Kind: "not_in_this_build", Subject: "lead"},
		{Name: "counted", Kind: "count_in_window", Subject: "lead", Event: "x", Window: day, Min: 1},
	}
	res, errs := Evaluate(specs, m, leads, now)
	if !res.Leads["A"]["even"] || res.Leads["B"]["even"] {
		t.Errorf("registered kind: %+v", res.Leads)
	}
	if res.Leads["A"]["gone"] || len(errs) != 1 {
		t.Errorf("missing kind: %+v %v", res.Leads, errs)
	}
	if !res.Leads["A"]["counted"] || !res.Leads["B"]["counted"] {
		t.Errorf("built-in kind: %+v", res.Leads)
	}
}
