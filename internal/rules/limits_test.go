package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Everyday loops over a settings list or a written list are within the cost
// limit; a loop over lead-sized data three deep is not (TestLoadErrors).
func TestCostEstimateAllowsEverydayLoops(t *testing.T) {
	r := mustCompile(t, `version: 1
lanes: []
settings:
  title_words: [head, vp, director]
derive:
  senior:
    - { when: { expr: "has(lead.title) && settings.title_words.exists(w, lead.title.contains(w))" }, then: true }
    - { when: { expr: "has(lead.title) && ['head', 'vp'].exists(w, lead.title.contains(w))" }, then: true }
    - else: false
`)
	got := values(t, r, Input{Leads: []api.LeadRef{lead("a", "", 0, map[string]string{"title": "vp sales"}), lead("b", "", 0, nil)}})
	if got["a"]["senior"] != true || got["b"]["senior"] != false {
		t.Errorf("got %v", got)
	}
}

// The run-time cost limit stops a condition that reaches it: false, with a warning.
func TestRunTimeCostLimit(t *testing.T) {
	defer func(old uint64) { runCostLimit = old }(runCostLimit)
	runCostLimit = 5
	r := mustCompile(t, derive1(`{ expr: "['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'].exists(w, w == 'z')" }`))
	res := r.Evaluate(Input{Leads: []api.LeadRef{lead("a", "", 0, nil)}})
	if res.Verdicts["a"].Values["x"] != 0.0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ran over the cost limit") {
		t.Errorf("x = %v, warnings %q", res.Verdicts["a"].Values["x"], res.Warnings)
	}
}

// A timeout that fires mid-run stops the evaluation between companies and
// leads, well before a full run would finish.
func TestEvaluateContextStopsMidRun(t *testing.T) {
	src := mustCompile(t, rollupBase+"{ field: company.legacy_seen, eq: true }\n        then: true\n      - else: false\n")
	in := Input{}
	for i := 0; i < 60_000; i++ {
		in.Leads = append(in.Leads, lead(fmt.Sprint(i), fmt.Sprintf("c%d.example", i/2), i,
			map[string]string{"warehouse_software": "paper", "fleet_size": "12"}))
	}
	start := time.Now()
	if _, err := src.EvaluateContext(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)

	ctx, cancel := context.WithTimeout(context.Background(), full/20)
	defer cancel()
	start = time.Now()
	_, err := src.EvaluateContext(ctx, in)
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if took > full/2 {
		t.Errorf("a %v timeout returned after %v; a full run takes %v", full/20, took, full)
	}
}
