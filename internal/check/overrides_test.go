package check

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// people builds a model holding the given rows, merged in one run.
func people(t *testing.T, rows ...api.InputRow) *model.Model {
	t.Helper()
	m := model.New()
	var ns []merge.Normalized
	for _, r := range rows {
		n, _ := merge.Normalize(r, nil)
		ns = append(ns, n)
	}
	merge.Apply(m, ns, merge.ApplyCtx{Now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), RunID: "r1"})
	return m
}

func row(cols ...string) api.InputRow {
	r := api.InputRow{SourceID: "leads", Columns: map[string]string{}}
	for i := 0; i+1 < len(cols); i += 2 {
		r.Headers = append(r.Headers, cols[i])
		r.Columns[cols[i]] = cols[i+1]
	}
	return r
}

func byKey(ps []Problem) map[string]Problem {
	out := map[string]Problem{}
	for _, p := range ps {
		out[p.Key] = p
	}
	return out
}

func TestOverridesCheck(t *testing.T) {
	m := people(t, row("email", "ada@acme.io"), row("email", "bo@acme.io"))
	ada, _ := merge.Resolve(m, "ada@acme.io")
	bo, _ := merge.Resolve(m, "bo@acme.io")
	for _, o := range []model.Override{
		{Person: "ada@acme.io", Action: "status", Value: "unsubscribed"},
		{Person: "Ada@acme.io", Action: "status", Value: "replied_positive"},
		{Person: "bo@acme.io", Action: "status", Value: "unsubscibed"},
		{Person: "dee@acme.io", Action: "status", Value: "unsubscribed"},
		{Person: "eve@acme.io", Action: "stauts", Value: "unsubscribed"},
	} {
		m.Put(model.TableOverrides, o)
	}
	ps := byKey(overridesCheck{}.Run(context.Background(), Env{Model: m}))
	for _, k := range []string{"status_conflict:" + string(ada), "status_conflict:" + string(bo)} {
		if p, ok := ps[k]; !ok || p.Warning {
			t.Errorf("%s = %+v, want a failure", k, p)
		}
	}
	if p := ps["override_unmatched:4"]; !p.Warning {
		t.Errorf("an unknown person waits: %+v, want a warning", p)
	}
	if p, ok := ps["override_unmatched:5"]; !ok || p.Warning {
		t.Errorf("an invalid row for an unknown person fails: %+v", p)
	}
	if len(ps) != 4 {
		t.Errorf("problems = %v", ps)
	}
	for _, p := range ps {
		if strings.Contains(p.Message, "@") {
			t.Errorf("a problem names a person: %s", p.Message)
		}
	}
	if got := (overridesCheck{}).Run(context.Background(), Env{}); got != nil {
		t.Error("no model, no problems")
	}
}

func TestDuplicatesCheck(t *testing.T) {
	m := people(t,
		row("email", "priya.r@acme.io", "name", "Priya R"),
		row("email", "priya.raj@acme.io", "name", "Priya R"),
	)
	ps := byKey(duplicatesCheck{}.Run(context.Background(), Env{Model: m}))
	if len(ps) != 2 {
		t.Fatalf("problems = %v, want both namesakes", ps)
	}
	for k, p := range ps {
		if !strings.HasPrefix(k, "namesake:") || p.Warning {
			t.Errorf("%s = %+v", k, p)
		}
	}
	m.Put(model.TableOverrides, model.Override{Person: "priya.r@acme.io", Action: "distinct", Value: "priya.raj@acme.io"})
	m.SetState(merge.StateKeyConflicts, "3")
	ps = byKey(duplicatesCheck{}.Run(context.Background(), Env{Model: m}))
	if p, ok := ps["key_conflicts"]; len(ps) != 1 || !ok || !p.Warning || !strings.Contains(p.Message, "3 input row") {
		t.Errorf("problems = %v, want only the key-conflict count as a warning", ps)
	}
}
