package check

import (
	"context"
	"sort"
	"strconv"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
)

func init() {
	Register(overridesCheck{})
	Register(duplicatesCheck{})
}

// overridesCheck is the `overrides` check (contracts section 10): a lead with
// conflicting status rows or a row of unknown value is blocked on every lane
// (status_conflict:<lead>, a failure); a row naming a person not yet known
// waits for that person (override_unmatched:<row>, a warning). Messages name
// lead ids and row numbers, never a person's email.
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
	return out
}

// duplicatesCheck is the `duplicates` check (contracts section 10): unresolved
// namesakes, which every lane skips (namesake:<lead>), and the running count of
// key conflicts (a warning: pushing is not blocked).
type duplicatesCheck struct{}

func (duplicatesCheck) Name() string { return "duplicates" }
func (duplicatesCheck) InRun() bool  { return true }

func (duplicatesCheck) Run(_ context.Context, env Env) []Problem {
	if env.Model == nil {
		return nil
	}
	dups := merge.Duplicates(env.Model)
	var out []Problem
	for _, lead := range sortedLeads(dups) {
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
