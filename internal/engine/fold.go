package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// ledgerRowsKey is the State key holding the highest committed ledger row
// count (contracts section 4); a run never lowers it.
const ledgerRowsKey = "ledger_rows"

// Unsubscribed origin a manual Overrides row sets (contracts section 7).
const unsubManual = "manual"

// Log kinds the fold and the push loop write.
const (
	logResubscribe   = "resubscribe"
	logRetry         = "retry"
	logPushFailed    = "push_failed"
	logPushRefused   = "push_refused"
	logPushCancelled = "push_cancelled"
	logLookupFailed  = "lookup_failed"
)

// foldHook is the Fold hook (step 5). It first fixes the ledger as loaded
// (contracts section 8): a ledger with fewer rows than State.ledger_rows
// blocks pushing; a row whose intent_run another run left gets called_at and
// loses intent_run; a merged lead's pending rows never called are
// cancelled, as are pending rows of a lane removed from the rubric; a row
// of a lane that is now cold becomes cold (never the reverse); and a pending
// row whose lane's destination changed moves to it if never called, else is
// cancelled. It then
// applies each new `retry` and `resubscribe` Overrides row once (Applied
// overrides), and folds every live lead's status (contracts section 7).
// Everything it changes is saved in phase 1. Last, it keeps open the
// receiver_only_push problems whose lead is still known only from webhooks.
func foldHook(r *Run) error {
	r.invalidate()
	r.pushing = nil
	v := r.view()
	loadLedger(r, v)
	applyRetries(r, v)
	applyResubscribes(r, v)
	foldStatuses(r, v)
	r.invalidate()
	keepReceiverOnlyPushes(r)
	return nil
}

// loadLedger is the load-time ledger work (contracts section 8).
func loadLedger(r *Run, v *view) {
	m, now := r.Model, r.Now()
	n := len(m.Pushes)
	stored, _ := strconv.Atoi(m.StateValue(ledgerRowsKey))
	if n < stored {
		// The store check (in run) raises ledger_shrank on the same comparison.
		r.blockPushes("the ledger has fewer rows than it had")
	}
	setLedgerRows(m)
	for _, k := range sortedPushKeys(m) {
		p := m.Pushes[k]
		changed := false
		if p.IntentRun != "" && p.IntentRun != r.ID {
			// Another run marked the step and never recorded the result: the
			// call may have gone out, so it now counts as called.
			if p.CalledAt.IsZero() {
				p.CalledAt = now
			}
			p.IntentRun, changed = "", true
		}
		lane, inRubric := v.lanes[p.LaneID]
		if inRubric && lane.Kind == kindCold && p.LaneKind != kindCold {
			p.LaneKind, changed = kindCold, true // a lane that became cold: never downgraded after
		}
		switch {
		case p.State != statePending:
		case m.People[model.Key(p.LeadID)].MergedInto != "" && p.CalledAt.IsZero() && p.IntentRun == "":
			p.State, p.LastError, changed = stateCancelled, "the lead was merged into another lead", true
		case !inRubric:
			p.State, p.LastError, changed = stateCancelled, "the lane was removed from the rubric", true
		case lane.Dest != p.Dest && p.CalledAt.IsZero() && p.IntentRun == "":
			p.Dest, changed = lane.Dest, true // never called: it goes to the lane's new destination
		case lane.Dest != p.Dest:
			// Called at the old destination: the vendor may hold the person
			// there, so the new one is never called for this push.
			p.State, p.LastError, changed = stateCancelled, "the lane's destination changed after the step was called", true
		}
		if changed {
			p.UpdatedAt = now
			m.Put(model.TablePushes, p)
		}
	}
}

// setLedgerRows raises State.ledger_rows to the ledger's row count; it is
// never lowered, so a ledger that shrank stays flagged until restored.
func setLedgerRows(m *model.Model) {
	stored, _ := strconv.Atoi(m.StateValue(ledgerRowsKey))
	if n := len(m.Pushes); n > stored {
		m.SetState(ledgerRowsKey, strconv.Itoa(n))
	}
}

func sortedPushKeys(m *model.Model) []model.Key {
	out := make([]model.Key, 0, len(m.Pushes))
	for k := range m.Pushes {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// applyRetries applies each `retry` row not yet applied, once: the failed
// steps of the person's lead (every lead for `*`) in the row's lane (every
// lane when empty) return to pending with no attempts. A row naming a person
// not yet known waits.
func applyRetries(r *Run, v *view) {
	m, now := r.Model, r.Now()
	for _, o := range v.ov.Rows {
		if o.Action != merge.ActionRetry || o.Invalid != "" || !o.Matched() || applied(m, o.Hash) {
			continue
		}
		leads := []api.LeadID{o.Lead}
		if o.Person == "*" {
			leads = v.idx.LiveLeads()
		}
		n := 0
		for _, id := range leads {
			for _, p := range v.rows(id) {
				if p.State != stateFailed || (o.Value != "" && !strings.EqualFold(p.LaneID, o.Value)) {
					continue
				}
				p.State, p.Attempts, p.LastError, p.UpdatedAt = statePending, 0, "", now
				m.Put(model.TablePushes, p)
				n++
			}
		}
		markApplied(r, o.Hash)
		lane := o.Value
		if lane == "" {
			lane = "every lane"
		}
		r.log("info", logRetry, o.Lead, fmt.Sprintf("Overrides row %d (retry %s) reset %d failed step(s) to pending", o.Row, lane, n))
	}
}

// applyResubscribes applies each `resubscribe` row not yet applied, once
// (contracts section 7): it clears the lead's opt-out only where its origin
// is `manual`; an automated opt-out (an event or a lookup) stays. A
// `resubscribe` row while a manual `unsubscribed` row still names the lead
// does nothing (the opt-out keeps its date) and is used up; the log says what
// each row did.
func applyResubscribes(r *Run, v *view) {
	m := r.Model
	for _, o := range v.ov.Rows {
		if o.Action != merge.ActionStatus || o.Value != merge.Resubscribe || o.Invalid != "" || o.Lead == "" || applied(m, o.Hash) {
			continue
		}
		cleared, kept := 0, 0
		stays := v.ov.Status[o.Lead] == statusUnsubscribed
		for _, f := range v.family(o.Lead) {
			if stays {
				break // the unsubscribed row still names the lead: its opt-out keeps its date
			}
			out := m.Outcomes[model.Key(f)]
			if out.UnsubscribedAt.IsZero() {
				continue
			}
			if out.UnsubscribedOrigin != unsubManual {
				kept++
				continue
			}
			out.UnsubscribedAt, out.UnsubscribedOrigin = time.Time{}, ""
			m.Put(model.TableOutcomes, out)
			cleared++
		}
		markApplied(r, o.Hash)
		msg := fmt.Sprintf("Overrides row %d (resubscribe) cleared %d manual opt-out(s)", o.Row, cleared)
		if kept > 0 {
			msg += fmt.Sprintf("; %d opt-out(s) from an event or lookup stay, since automation's opt-outs are never undone", kept)
		}
		if stays {
			msg = fmt.Sprintf("Overrides row %d (resubscribe) did nothing: an unsubscribed row in Overrides still names the lead, so it stays unsubscribed", o.Row)
		}
		r.log("info", logResubscribe, o.Lead, msg)
	}
}

func applied(m *model.Model, hash string) bool {
	_, ok := m.AppliedOverrides[model.Key(hash)]
	return ok
}

func markApplied(r *Run, hash string) {
	r.Model.Put(model.TableAppliedOverrides, model.AppliedOverride{RowHash: hash, AppliedAt: r.Now(), RunID: r.ID})
}

// foldStatuses folds every live lead's status and returns the leads whose
// status changed. A manual `unsubscribed` row sets unsubscribed_at (origin
// manual) only when the lead's family has none. It applies no one-shot row,
// so the push loop calls it again after each re-read.
func foldStatuses(r *Run, v *view) []api.LeadID {
	m, now := r.Model, r.Now()
	var changed []api.LeadID
	for _, id := range v.idx.LiveLeads() {
		if v.ov.Status[id] == statusUnsubscribed && !v.unsubscribed(id) {
			o := m.Outcomes[model.Key(id)]
			o.LeadID, o.UnsubscribedAt, o.UnsubscribedOrigin = id, now, unsubManual
			m.Put(model.TableOutcomes, o)
		}
		status, contactedAt := v.fold(id)
		o := m.Outcomes[model.Key(id)]
		dirty := o.LeadID != id
		o.LeadID = id
		if o.ContactedAt.IsZero() && !contactedAt.IsZero() {
			o.ContactedAt, dirty = contactedAt, true
		}
		if o.Status != status {
			o.Status, o.StatusAt, dirty = status, now, true
			changed = append(changed, id)
		}
		if dirty {
			m.Put(model.TableOutcomes, o)
		}
	}
	return changed
}

// fold is one live lead's status: the first rule of contracts section 7 that
// applies, read across the lead and every lead merged into it. It also
// returns when a completed cold push contacted the lead, so the fold keeps
// that fact in Outcomes.
func (v *view) fold(id api.LeadID) (string, time.Time) {
	var reply string
	var replyAt, contacted time.Time
	unsub := false
	for _, f := range v.family(id) {
		o := v.m.Outcomes[model.Key(f)]
		unsub = unsub || !o.UnsubscribedAt.IsZero()
		if o.ReplyStatus != "" && (reply == "" || o.ReplyAt.After(replyAt)) {
			reply, replyAt = o.ReplyStatus, o.ReplyAt
		}
		if !o.ContactedAt.IsZero() && (contacted.IsZero() || o.ContactedAt.Before(contacted)) {
			contacted = o.ContactedAt
		}
	}
	pushedAt := v.coldPushedAt(id)
	if contacted.IsZero() {
		contacted = pushedAt
	}
	switch {
	case unsub:
		return statusUnsubscribed, pushedAt
	case v.ov.Blocked[id] != "":
		return statusBlocked, pushedAt
	}
	if s, ok := v.ov.Status[id]; ok {
		return s, pushedAt
	}
	switch {
	case v.deal(id, ""):
		return statusDeal, pushedAt
	case reply != "":
		return reply, pushedAt
	case !contacted.IsZero():
		return statusContacted, pushedAt
	}
	return statusNew, pushedAt
}

// coldPushedAt is when a cold push of the lead's family completed (every
// step done), the earliest; zero when none has.
func (v *view) coldPushedAt(id api.LeadID) time.Time {
	type push struct{ lead, lane string }
	n, done := map[push]int{}, map[push]int{}
	last := map[push]time.Time{}
	for _, p := range v.familyRows(id) {
		if !v.isCold(p) {
			continue
		}
		k := push{string(p.LeadID), p.LaneID}
		n[k]++
		if p.State == stateDone {
			done[k]++
			if p.UpdatedAt.After(last[k]) {
				last[k] = p.UpdatedAt
			}
		}
	}
	var at time.Time
	for k, c := range n {
		if done[k] == c && (at.IsZero() || last[k].Before(at)) {
			at = last[k]
		}
	}
	return at
}

// blockPushes sets NoPush for the run, keeping every reason.
func (r *Run) blockPushes(reason string) {
	if r.NoPush == "" {
		r.NoPush = reason
	} else if !strings.Contains(r.NoPush, reason) {
		r.NoPush += "; " + reason
	}
}

// log appends a Log line with a lead id, never an email.
func (r *Run) log(level, kind string, lead api.LeadID, msg string) {
	r.Model.Put(model.TableLog, model.LogEntry{At: r.Now(), RunID: r.ID, Level: level, LeadID: lead, Kind: kind,
		Message: msg, RubricVersion: r.Rubric.Version()})
}
