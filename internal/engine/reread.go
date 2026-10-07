package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// reReadHook is the ReRead hook (S15, contracts section 12.6): before each
// pushing batch it reads the receiver events stored since the last read (the
// first time, since Intake's cursor:events), and applies their effects to the
// run's model, so an opt-out or reply that arrives while the run pushes
// blocks the next batch.
//
// Each event passes the filter Intake uses (keyEvent and suppressed): an
// event whose key is already in Seen events, or was taken by an earlier
// re-read this run, is skipped, as Intake skips a redelivery, so a re-sent
// reply can never move a status back. Its person is found with
// merge.FindPerson, with no writes: an unknown person is skipped (the next
// run's Intake creates the lead), except that an opt-out still reaches every
// live lead holding one of its identity keys, as Intake would apply it.
//
// Its read position lives on the run only: it moves no cursor:events and
// writes no Seen events or Window events row, so the next run takes the same
// events again through Intake, and applying an event twice changes nothing.
// It returns the live leads whose Outcomes or Apollo hold changed. The push
// loop folds every status again after it, and reads opt-outs from Outcomes
// across each lead's merge family.
func reReadHook(r *Run) ([]api.LeadID, error) {
	if r.Events == nil || r.DryRun {
		return nil, nil
	}
	m := r.Model
	st := r.reread
	if st == nil {
		st = &rereadState{cursor: api.Cursor(m.StateValue(eventsCursorKey)), seen: map[api.EventID]bool{}}
		r.reread = st
	}
	raws, next, err := r.Events.ReadEvents(r.Ctx, st.cursor)
	if err != nil {
		return nil, fmt.Errorf("re-reading the event log: %w", err)
	}
	st.cursor = next
	set := map[api.LeadID]bool{}
	parsed, _ := events.Parse(raws)
	for _, e := range parsed {
		if e.Kind == "" {
			continue // a rejected body: the next run's Intake logs it
		}
		e, why := keyEvent(merge.NormalizeEventKeys(e))
		if why != "" || st.seen[e.ID] {
			continue
		}
		if _, seen := m.SeenEvents[model.Key(e.ID)]; seen {
			continue
		}
		st.seen[e.ID] = true
		if suppressed(r, []api.Event{e}) {
			continue
		}
		lead, _ := merge.FindPerson(m, e)
		for _, id := range events.Apply(m, lead, e, r.Config.ReplyLabels) {
			set[id] = true
		}
	}
	changed := make([]api.LeadID, 0, len(set))
	for id := range set {
		changed = append(changed, id)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })
	return changed, nil
}

// rereadState is the re-read's position for one run: the event log cursor
// after its last read, and the keys it took.
type rereadState struct {
	cursor api.Cursor
	seen   map[api.EventID]bool
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
