package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func init() { Register(apolloSequences{getenv: os.Getenv}) }

// apolloSequences is the `apollo-sequences` check (contracts section 10): for
// an install with lanes pushing to Apollo, sinks.apollo.mailbox_id must be
// one of the team's sending mailboxes (apollo-sequences:mailbox), and every
// such lane must name one sequence that exists, by its exact name
// (apollo-sequences:<lane id>). A lane whose name does not resolve waits
// (the sink returns ErrTransient), so this check is what tells the team.
//
// A missing key is the secrets check's to report, and a rubric that does not
// compile the rubric check's. A key Apollo refuses (401, 403: not a master
// key) fails as apollo-sequences:key; a call that gets no answer (429, 5xx,
// network) is the warning apollo-sequences:unreachable: it says nothing about
// the names.
type apolloSequences struct{ getenv func(string) string }

func (apolloSequences) Name() string { return "apollo-sequences" }
func (apolloSequences) InRun() bool  { return true }

func (a apolloSequences) Run(ctx context.Context, env Env) []Problem {
	if env.Config == nil {
		return nil
	}
	block, ok := env.Config.Sinks["apollo"]
	if !ok {
		return nil
	}
	lanes := apolloLanes(env)
	key := a.getenv(apollo.KeyVariable)
	if len(lanes) == 0 || strings.TrimSpace(key) == "" {
		return nil
	}
	c, err := apollo.NewClientWithKey(block, key)
	if err != nil {
		return []Problem{{Key: "apollo-sequences:config", Message: "the sinks.apollo block cannot be used: " + err.Error(),
			Fix: "fix sinks.apollo in leadscore.yml"}}
	}
	unreachable := func(err error) []Problem {
		if apollo.KeyRefused(err) {
			// S0 confirms: sequences and mailboxes need a master key, and
			// Apollo answers 401 or 403 to any other.
			return []Problem{{Key: "apollo-sequences:key",
				Message: "Apollo refused the key for sequences and mailboxes, so nothing can be enrolled: " + err.Error(),
				Fix:     "use a master API key in " + apollo.KeyVariable}}
		}
		return []Problem{{Key: "apollo-sequences:unreachable", Warning: true,
			Message: "Apollo did not answer, so the mailbox and sequence names were not checked: " + err.Error(),
			Fix:     "nothing if it clears on the next run; otherwise check Apollo's status and the network"}}
	}

	var out []Problem
	mailbox, err := apollo.MailboxID(block)
	if err != nil {
		out = append(out, Problem{Key: "apollo-sequences:mailbox", Message: err.Error(),
			Fix: "set sinks.apollo.mailbox_id to the id of the mailbox sequences send from (quoted)"})
	} else {
		ids, err := c.EmailAccountIDs(ctx)
		if err != nil {
			return unreachable(err)
		}
		if !slices.Contains(ids, mailbox) {
			out = append(out, Problem{Key: "apollo-sequences:mailbox",
				Message: "sinks.apollo.mailbox_id is not one of this Apollo account's mailboxes, so no enrollment can send",
				Fix:     "check sinks.apollo.mailbox_id against Apollo's email accounts"})
		}
	}

	resolved := map[string]error{}
	for _, l := range lanes {
		name, ok := apollo.SequenceName(l.Dest)
		if !ok {
			continue // the rubric compiler refuses any other Apollo destination
		}
		err, done := resolved[name]
		if !done {
			_, err = c.ResolveSequence(ctx, name)
			resolved[name] = err
		}
		switch {
		case err == nil:
		case errors.Is(err, apollo.ErrSequenceNotFound):
			out = append(out, Problem{Key: "apollo-sequences:" + l.ID,
				Message: fmt.Sprintf("lane %s names the sequence %q, which Apollo does not have (names must match exactly), so its leads wait", l.ID, name),
				Fix:     "check the sequence name in the lane's push"})
		case errors.Is(err, apollo.ErrSequenceAmbiguous):
			out = append(out, Problem{Key: "apollo-sequences:" + l.ID,
				Message: fmt.Sprintf("lane %s names the sequence %q, and Apollo has more than one by that name, so its leads wait", l.ID, name),
				Fix:     "rename one of the sequences in Apollo, or name a unique one"})
		default:
			return append(out, unreachable(err)...)
		}
	}
	return out
}

// apolloLanes are the rubric's lanes pushing to the apollo sink: the run's
// rubric, or in doctor the file compiled; none when it does not compile.
func apolloLanes(env Env) []rules.Lane {
	r := RubricFor(env)
	if r == nil {
		return nil
	}
	var out []rules.Lane
	for _, l := range r.Lanes() {
		if l.Sink == "apollo" {
			out = append(out, l)
		}
	}
	return out
}
