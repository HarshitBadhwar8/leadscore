package check

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
)

func init() {
	Register(overridesCheck{})
	Register(duplicatesCheck{})
}

// overridesCheck is the `overrides` check: a lead with
// conflicting status rows or a row of unknown value is blocked on every lane
// (status_conflict:<lead>, a failure); a row naming a person not yet known
// waits for that person (override_unmatched:<row>, a warning); a retry row
// naming a lane the rubric does not have retries nothing
// (override_unknown_lane:<row>, a warning). Messages name lead ids and row
// numbers, never a person's email.
type overridesCheck struct{}

func (overridesCheck) Name() string { return "overrides" }
func (overridesCheck) InRun() bool  { return true }

func (overridesCheck) Run(_ context.Context, env Env) []Problem {
	if env.Model == nil {
		return nil
	}
	ov := merge.ParseOverrides(env.Model)
	var out []Problem
	for _, lead := range sortedLeads(ov.Blocked) {
		out = append(out, Problem{
			Key:     "status_conflict:" + string(lead),
			Message: "lead " + string(lead) + " is blocked on every lane: " + ov.Blocked[lead],
			Fix:     "fix the Overrides cell, or run leadscore set-status <person> <status|none>",
		})
	}
	for _, o := range ov.Unmatched {
		msg := "Overrides row " + strconv.Itoa(o.Row) + " names a person no lead matches yet; it waits until the person appears"
		if o.Invalid != "" {
			msg = "Overrides row " + strconv.Itoa(o.Row) + ": " + o.Invalid + ", and it matches no lead"
		}
		out = append(out, Problem{
			Key:     "override_unmatched:" + strconv.Itoa(o.Row),
			Message: msg,
			Fix:     "check the person's spelling; a row for someone not yet imported can stay",
			Warning: o.Invalid == "",
		})
	}
	if lanes := rubricLanes(env); lanes != nil {
		for _, o := range ov.Rows {
			if o.Action == merge.ActionRetry && o.Value != "" && !lanes[strings.ToLower(o.Value)] {
				out = append(out, Problem{
					Key:     "override_unknown_lane:" + strconv.Itoa(o.Row),
					Message: "Overrides row " + strconv.Itoa(o.Row) + " retries a lane the rubric does not have, so it retries nothing",
					Fix:     "use a lane id from the rubric, or leave value empty for every lane",
					Warning: true,
				})
			}
		}
	}
	return out
}

// rubricLanes returns the rubric's lane ids (the run's rubric, or doctor's
// compiled file), lowercased (ids are unique ignoring case), or nil when there
// is no rubric to read or it does not compile.
func rubricLanes(env Env) map[string]bool {
	r := RubricFor(env)
	if r == nil {
		return nil
	}
	out := map[string]bool{}
	for _, l := range r.Lanes() {
		out[strings.ToLower(l.ID)] = true
	}
	return out
}

// duplicatesCheck is the `duplicates` check: unresolved
// namesakes, which every lane skips (namesake:<lead>), and the running count of
// key conflicts (a warning: pushing is not blocked). A lead in a hand-edited
// merged_into cycle is raised as merge_cycle:<lead> instead.
type duplicatesCheck struct{}

func (duplicatesCheck) Name() string { return "duplicates" }
func (duplicatesCheck) InRun() bool  { return true }

func (duplicatesCheck) Run(_ context.Context, env Env) []Problem {
	if env.Model == nil {
		return nil
	}
	dups := merge.Duplicates(env.Model)
	cycles := merge.Cycles(env.Model)
	var out []Problem
	for _, lead := range sortedLeads(dups) {
		if cycles[lead] {
			out = append(out, Problem{
				Key:     "merge_cycle:" + string(lead),
				Message: "lead " + string(lead) + "'s merged_into leads round in a cycle back to itself; every lane skips it",
				Fix:     "in the store's People table, clear merged_into on one lead of the cycle, then merge them again with leadscore merge",
			})
			continue
		}
		out = append(out, Problem{
			Key: "namesake:" + string(lead),
			Message: "lead " + string(lead) + " shares a company domain and name with another lead (" +
				strconv.Itoa(len(dups)) + " leads in all); every lane skips it",
			Fix: "resolve each pair in Overrides: leadscore mark-distinct <person> <person>, or leadscore merge <person> <person>",
		})
	}
	if n, _ := strconv.Atoi(env.Model.StateValue(merge.StateKeyConflicts)); n > 0 {
		out = append(out, Problem{
			Key: "key_conflicts",
			Message: strconv.Itoa(n) + " input row(s) carried an email or LinkedIn URL that belongs to another lead; " +
				"the key was not written (Log kind key_conflict)",
			Fix:     "check the rows named in Log; merge the leads in Overrides if they are one person",
			Warning: true,
		})
	}
	return out
}

func sortedLeads[V any](m map[api.LeadID]V) []api.LeadID {
	out := make([]api.LeadID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
