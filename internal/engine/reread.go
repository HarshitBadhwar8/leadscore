package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// reReadHook is the ReRead hook (S15, contracts section 12.6): before each
// pushing batch it reads the receiver events stored since Intake read the
// log, and applies their effects to the run's model, so an opt-out or reply
// that arrives while the run pushes blocks the next batch.
//
// Each event's person is found with merge.FindPerson, with no writes: an
// unknown person is skipped (the next run's Intake creates the lead), except
// for an opt-out, which still reaches every live lead holding one of its
// identity keys, as Intake would apply it. Under `replies: polling` a
// receiver reply has no effect, as in Intake. It moves no cursor and writes
// no Seen events or Window events row: the next run takes the same events
// again through Intake, and applying an event twice changes nothing.
//
// It returns the live leads whose Outcomes or Apollo hold changed. The push
// loop folds every status again after it, and reads opt-outs from Outcomes
// directly, so a changed lead in a merge family blocks the whole family.
func reReadHook(r *Run) ([]api.LeadID, error) {
	if r.Events == nil || r.DryRun {
		return nil, nil
	}
	m := r.Model
	cursor := api.Cursor(m.StateValue(eventsCursorKey))
	raws, _, err := r.Events.ReadEvents(r.Ctx, cursor)
	if err != nil {
		return nil, fmt.Errorf("re-reading the event log: %w", err)
	}
	if len(raws) == 0 {
		return nil, nil
	}

	outcomes := make(map[model.Key]model.Outcome, len(m.Outcomes))
	for k, o := range m.Outcomes {
		outcomes[k] = o
	}
	held := map[model.Key]bool{}
	for k, p := range m.People {
		held[k] = !p.ApolloHeldAt.IsZero()
	}

	parsed, _ := events.Parse(raws)
	for _, e := range parsed {
		e.Kind = strings.ToLower(e.Kind)
		if e.Kind == "" || strings.HasPrefix(e.Kind, "deal_") || suppressed(r, []api.Event{e}) {
			continue // a rejected body (Intake logs it), or a reply that polling owns
		}
		e = merge.NormalizeEventKeys(e)
		lead, _ := merge.FindPerson(m, e)
		events.Apply(m, lead, e, r.Config.ReplyLabels)
	}

	set := map[api.LeadID]bool{}
	for k, o := range m.Outcomes {
		if old, ok := outcomes[k]; !ok || !reflect.DeepEqual(old, o) {
			set[merge.Live(m, api.LeadID(k))] = true
		}
	}
	for k, p := range m.People {
		if !p.ApolloHeldAt.IsZero() && !held[k] {
			set[merge.Live(m, api.LeadID(k))] = true
		}
	}
	changed := make([]api.LeadID, 0, len(set))
	for id := range set {
		changed = append(changed, id)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })
	return changed, nil
}

// receiverOnlyPushKind is the problem raised for a push to a lead known only
// from receiver webhooks (contracts section 4, RFC 7 "Security").
const receiverOnlyPushKind = "receiver_only_push"

// receiverOnlyPush raises receiver_only_push:<lead>, a warning: anyone with
// the receiver secret can forge a webhook-only lead and its positive reply,
// which a non-cold lane acts on (Decisions Log row 79: warn only).
func receiverOnlyPush(r *Run, lead api.LeadID) {
	r.Problem(receiverOnlyPushKind+":"+string(lead),
		fmt.Sprintf("lead %s is known only from receiver webhooks and was pushed; anyone with the receiver secret can forge such a lead and its replies", lead),
		"check the lead is real; it clears once a source reports the lead. Keep the receiver secret private, and rotate it if it may have leaked", true)
}

// keepReceiverOnlyPushes raises again every receiver_only_push problem still
// open in Health whose lead is still known only from the receiver, so the
// flag stays until a source reports the lead (or a merge gives it one), not
// only for the run that pushed. The fold calls it each run.
func keepReceiverOnlyPushes(r *Run) {
	var idx *merge.Index
	for _, row := range r.Model.Health {
		id, ok := strings.CutPrefix(row.Key, receiverOnlyPushKind+":")
		if row.Kind != healthProblem || !ok {
			continue
		}
		lead := merge.Live(r.Model, api.LeadID(id))
		if _, known := r.Model.People[model.Key(lead)]; !known {
			continue
		}
		if idx == nil {
			idx = merge.NewIndex(r.Model, r.Config.Sources)
		}
		if idx.ReceiverOnly(lead) {
			receiverOnlyPush(r, lead)
		}
	}
}
