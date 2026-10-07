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
// pending more than 24 hours (push_pending, with the count). Rows of a lead
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
			out = append(out, Problem{
				Key:     fmt.Sprintf("push_failed:%s:%s:%s", p.LeadID, p.LaneID, p.Step),
				Message: fmt.Sprintf("lead %s's step %s in lane %s failed %d times and waits for a retry", p.LeadID, p.Step, p.LaneID, p.Attempts),
				Fix:     fmt.Sprintf("fix the cause in the Pushes row's last_error, then run leadscore retry --lane %s %s (or add a retry row in Overrides)", p.LaneID, p.LeadID),
			})
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
			Fix: "check Health for what blocks pushing (pushes_enabled, a backlog, a lookup or sink problem) and the Pushes rows' last_error",
		})
	}
	return out
}
