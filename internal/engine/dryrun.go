package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// report prints the dry run: one line per lead whose verdict,
// status or planned lane differs from the last run's Ranked, then totals. It
// names leads by id only, since stdout may be a hosted log.
func (x *exec) report() {
	w := x.s.out
	r := x.run
	fmt.Fprintln(w, "dry run: no lease taken, nothing written, no enrichment, no lookups and no pushes")
	if !x.scored {
		fmt.Fprintln(w, "dry run: nothing was scored (the deadline passed or the run was stopped first)")
		return
	}
	derived := r.Rubric.DerivedNames()
	var added, changed, same int
	lanes := map[string]int{}
	// Highest score first, so the leads that matter most lead the report.
	refs := append([]api.LeadRef(nil), r.Input.Leads...)
	sort.SliceStable(refs, func(i, j int) bool {
		return r.Model.Ranked[model.Key(refs[i].ID)].Score > r.Model.Ranked[model.Key(refs[j].ID)].Score
	})
	for _, ref := range refs {
		k := model.Key(ref.ID)
		row := r.Model.Ranked[k]
		lane := row.Lane
		if lane == "" {
			lane = "none"
		}
		lanes[lane]++
		old, had := x.oldRanked[k]
		if !had {
			added++
			fmt.Fprintf(w, "new      %s  %s\n", ref.ID, summary(row, derived))
			continue
		}
		diffs := differences(old, row, derived)
		if len(diffs) == 0 {
			same++
			continue
		}
		changed++
		fmt.Fprintf(w, "changed  %s  %s\n", ref.ID, strings.Join(diffs, "; "))
	}
	names := make([]string, 0, len(lanes))
	for l := range lanes {
		names = append(names, l)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, l := range names {
		parts[i] = fmt.Sprintf("%s %d", l, lanes[l])
	}
	fmt.Fprintf(w, "totals: %d lead(s) scored: %d new, %d changed, %d unchanged; planned lanes: %s\n",
		len(r.Input.Leads), added, changed, same, strings.Join(parts, ", "))
	if r.NoPush != "" {
		fmt.Fprintf(w, "pushing would wait this run: %s\n", r.NoPush)
	}
}

// summary describes a new lead's row: every derived value, the score, status
// and planned lane.
func summary(row model.RankedRow, derived []string) string {
	var parts []string
	for _, n := range derived {
		parts = append(parts, n+"="+shown(row.Derived[n]))
	}
	parts = append(parts, "score="+model.FormatFloat(row.Score), "status="+shown(row.Status), "lane="+shown(row.Lane))
	return strings.Join(parts, " ")
}

// differences lists what changed between the last run's row and this one.
func differences(old, cur model.RankedRow, derived []string) []string {
	var out []string
	diff := func(name, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("%s %s -> %s", name, shown(a), shown(b)))
		}
	}
	for _, n := range derived {
		diff(n, old.Derived[n], cur.Derived[n])
	}
	diff("score", model.FormatFloat(old.Score), model.FormatFloat(cur.Score))
	diff("status", old.Status, cur.Status)
	diff("lane", old.Lane, cur.Lane)
	return out
}
