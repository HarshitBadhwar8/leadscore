package check

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func init() {
	Register(pushesCheck{now: time.Now})
}

// pendingWarnAfter is how long a step may stay pending before the `pushes`
// check reports it (contracts section 11).
const pendingWarnAfter = 24 * time.Hour

// pushesCheck is the `pushes` check (contracts section 10): a failed step
// (push_failed:<lead>:<lane>:<step>, until a `retry` row resets it) and steps
// pending more than 24 hours (push_pending, with the count, a warning). Rows of a lead
// merged into another are left out: no step is called for such a lead.
// Messages name lead ids, never emails.
type pushesCheck struct {
	now func() time.Time
}

func (pushesCheck) Name() string { return "pushes" }
func (pushesCheck) InRun() bool  { return true }

func (c pushesCheck) Run(_ context.Context, env Env) []Problem {
	if env.Model == nil {
		return nil
	}
	m := env.Model
	keys := make([]model.Key, 0, len(m.Pushes))
	for k := range m.Pushes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var out []Problem
	pending := 0
	var oldest time.Time
	now := c.now()
	for _, k := range keys {
		p := m.Pushes[k]
		if m.People[model.Key(p.LeadID)].MergedInto != "" {
			continue
		}
		switch p.State {
		case "failed":
			out = append(out, PushFailed(p))
		case "pending":
			since := p.FirstStartedAt
			if since.IsZero() {
				since = p.UpdatedAt
			}
			if !since.IsZero() && now.Sub(since) > pendingWarnAfter {
				pending++
				if oldest.IsZero() || since.Before(oldest) {
					oldest = since
				}
			}
		}
	}
	if pending > 0 {
		out = append(out, Problem{
			Key: "push_pending",
			Message: fmt.Sprintf("%d step(s) have been pending for more than 24 hours, the oldest since %s",
				pending, model.FormatTime(oldest)),
			Fix:     "check Health for what blocks pushing (pushes_enabled, a backlog, a lookup or sink problem) and the Pushes rows' last_error",
			Warning: true, // contracts section 11: the pending-push warning
		})
	}
	return out
}

// PushFailed is the push_failed:<lead>:<lane>:<step> problem for a failed
// ledger row: the one message both this check and the push loop raise.
func PushFailed(p model.Push) Problem {
	return Problem{
		Key: fmt.Sprintf("push_failed:%s:%s:%s", p.LeadID, p.LaneID, p.Step),
		Message: fmt.Sprintf("lead %s's step %s in lane %s failed %d times and waits for a retry; the last error: %s",
			p.LeadID, p.Step, p.LaneID, p.Attempts, p.LastError),
		Fix: fmt.Sprintf("fix the cause, then run leadscore retry --lane %s %s (or add a retry row in Overrides)", p.LaneID, p.LeadID),
	}
}

// LedgerShrank is the ledger_shrank problem: the ledger holds fewer rows than
// State.ledger_rows says were saved. The store check raises it; the run's
// fold blocks pushing on the same comparison.
func LedgerShrank(rows, saved int) Problem {
	return Problem{
		Key: "ledger_shrank",
		Message: fmt.Sprintf("the ledger (Pushes) has %d rows, but %d were saved before: rows were deleted, so the run cannot tell who was already contacted, and nothing is pushed",
			rows, saved),
		Fix: "restore the deleted Pushes rows from a backup or the spreadsheet's version history; pushing waits until then",
	}
}
