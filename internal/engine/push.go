// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// pushHook is the Push hook (step 9, the ledger rules).
//
// It first cancels every pending step whose lead now fails a cancelling check
// (the built-in checks, the lane's `when`, the Apollo-held rule, the cold-lane
// statuses, the deal rule and the one cold push) or whose lane left the
// rubric; that runs whether or not this run pushes. Pushing then waits when
// pushes_enabled is off or NoPush is set, and only leads looked up this run
// push. Pushes go in budget order (non-cold first, then cold, each by lane
// priority, score and lead id) in batches of 25 leads. Before each batch the
// run checks the deadline, Stop and the lease, then re-reads events (ReRead)
// and the Overrides tab and folds statuses again; a failed re-read stops
// pushing for the run. The pre-batch write marks every step about to be
// called with intent_run (and first_started_at on a push's first start); the
// post-batch write records each result, sets called_at on every step called
// and clears intent_run. Just before each push the lead's checks run again
// on the in-memory model, so a deal step called earlier in the run blocks a
// colleague's cold push. Calls use Run.Ctx, so a call in flight finishes on
// Stop; no new call starts once PushCtx is done.
func pushHook(r *Run) error {
	if r.DryRun {
		return nil
	}
	r.invalidate()
	cancelPass(r, r.view())
	st := r.pushing
	if r.NoPush != "" || !r.Config.PushesEnabled || st == nil {
		return nil
	}
	v := r.view()
	routed, _, _ := v.routeAll()
	var items []item
	for _, it := range routed {
		if st.lookedUp[it.lead] {
			items = append(items, it)
		}
	}
	order(items)
	items = budget(items, v.newPushes())

	p := &pusher{r: r, sinks: map[string]*builtSink{}, stopped: map[string]bool{}}
	defer r.invalidate()
	for _, b := range batches(items) {
		if r.PushCtx.Err() != nil {
			return nil // the deadline or Stop: the rest waits for the next run
		}
		if err := r.Lease.Check(r.Ctx); err != nil {
			return fmt.Errorf("checking the lease before a pushing batch: %w", err)
		}
		if !p.reread() {
			return nil
		}
		if err := p.batch(b); err != nil {
			return err
		}
	}
	return nil
}

// cancelPass cancels the pending steps of live leads that now fail a
// cancelling check in their lane, and of lanes removed from the rubric
// (the ledger rules). A merged lead's rows are left: its never-called
// steps were cancelled at load, and no step is called for it.
func cancelPass(r *Run, v *view) {
	m := r.Model
	cancelled := map[[2]string]bool{}
	for _, k := range sortedPushKeys(m) {
		p := m.Pushes[k]
		if p.State != statePending || m.People[model.Key(p.LeadID)].MergedInto != "" {
			continue
		}
		why := "the lane was removed from the rubric"
		if l, ok := v.lanes[p.LaneID]; ok {
			why = v.laneCheck(p.LeadID, l)
		}
		if why == "" {
			continue
		}
		p.State, p.LastError, p.UpdatedAt = stateCancelled, why, r.Now()
		v.putPush(p)
		pk := [2]string{string(p.LeadID), p.LaneID}
		if !cancelled[pk] {
			cancelled[pk] = true
			r.log("info", logPushCancelled, p.LeadID, fmt.Sprintf("lane %s: pending steps cancelled: %s", p.LaneID, why))
		}
	}
}

// batches splits the pushes into batches of at most 25 leads, in order.
func batches(items []item) [][]item {
	var out [][]item
	var cur []item
	leads := map[api.LeadID]bool{}
	for _, it := range items {
		if !leads[it.lead] && len(leads) == batchLeads {
			out, cur, leads = append(out, cur), nil, map[api.LeadID]bool{}
		}
		leads[it.lead] = true
		cur = append(cur, it)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// pusher is one run's push loop.
type pusher struct {
	r       *Run
	sinks   map[string]*builtSink // by sink type
	stopped map[string]bool       // sink types that hit a rate limit this run
}

type builtSink struct {
	sink api.Sink
	err  error
}

// sink returns the sink a lane pushes to and the lane's steps, building each
// sink type once per run from its sinks.<type> block. A sink that cannot be
// built leaves its lanes' pushes waiting, with a problem raised.
func (p *pusher) sink(laneID, typ, dest string) (api.Sink, []string, bool) {
	b, ok := p.sinks[typ]
	if !ok {
		b = &builtSink{}
		p.sinks[typ] = b
		if f, registered := sinkFactory(typ); !registered {
			b.err = fmt.Errorf("this build has no %q sink", typ) // lane_sink_unregistered is raised at run start
		} else {
			block := p.r.Config.Sinks[typ]
			if block == nil {
				block = api.Config{}
			}
			b.sink, b.err = f(block)
			if b.err != nil {
				p.r.Problem("sink_failed:"+typ, "the "+typ+" sink could not be built, so its lanes wait: "+errText(b.err),
					fmt.Sprintf("check sinks.%s in leadscore.yml and the sink's key", typ), false)
			}
		}
	}
	if b.err != nil {
		return nil, nil, false
	}
	steps := b.sink.Steps(dest)
	if len(steps) == 0 {
		p.r.Problem("sink_failed:"+typ, fmt.Sprintf("the %s sink has no steps for lane %s's destination %s, so the lane waits", typ, laneID, dest),
			"check the lane's push in the rubric", false)
		return nil, nil, false
	}
	return b.sink, steps, true
}

// reread re-reads events (the ReRead hook) and the Overrides tab and folds
// statuses again before a batch. It reports false when either read failed:
// pushing then stops for the run.
func (p *pusher) reread() bool {
	r := p.r
	if r.ReRead != nil {
		if _, err := r.ReRead(); err != nil {
			r.log("warn", "push_stopped", "", "pushing stopped for this run: the re-read of events before a batch failed")
			return false
		}
	}
	rows, err := r.Store.ReadTable(r.Ctx, model.TableOverrides)
	if err == nil {
		err = r.Model.Load(model.TableOverrides, rows)
	}
	if err != nil {
		r.Problem("step_failed:reread", "re-reading Overrides before a pushing batch failed, so pushing stopped for this run: "+errText(err),
			"check the store; the next run pushes again", false)
		return false
	}
	r.invalidate()
	foldStatuses(r, r.view())
	return true
}

// mark is a ledger row as it was before the pre-batch write.
type mark struct {
	key     model.Key
	old     model.Push
	existed bool
}

// batch is one pushing batch: the pre-batch write, the calls, the post-batch
// write.
func (p *pusher) batch(b []item) error {
	r, m := p.r, p.r.Model
	v := r.view()
	now := r.Now()
	var marks []mark
	var ready []item
	for _, it := range b {
		if p.stopped[it.lane.Sink] {
			continue
		}
		_, steps, ok := p.sink(it.lane.ID, it.lane.Sink, it.lane.Dest)
		if !ok {
			continue
		}
		started := false
		var first model.Push
		for _, step := range steps {
			if row, ok := m.Pushes[stepKey(it.lead, it.lane.ID, step)]; ok && !row.FirstStartedAt.IsZero() &&
				(!started || row.FirstStartedAt.Before(first.FirstStartedAt)) {
				first, started = row, true
			}
		}
		firstAt := now
		if started {
			firstAt = first.FirstStartedAt
		}
		for _, step := range steps {
			k := stepKey(it.lead, it.lane.ID, step)
			row, existed := m.Pushes[k]
			if !existed {
				row = model.Push{LeadID: it.lead, LaneID: it.lane.ID, Step: step, LaneKind: it.lane.Kind, Dest: it.lane.Dest, State: statePending}
			}
			if row.State == stateCancelled && row.CalledAt.IsZero() && row.IntentRun == "" {
				row.State, row.LastError, row.Dest = statePending, "", it.lane.Dest // reselected for the lane (the ledger rules)
			}
			if row.State != statePending {
				continue
			}
			if it.lane.Kind == kindCold {
				row.LaneKind = kindCold // never downgraded
			}
			marks = append(marks, mark{key: k, old: row, existed: existed})
			if existed {
				marks[len(marks)-1].old = m.Pushes[k]
			}
			row.IntentRun, row.UpdatedAt = r.ID, now
			if row.FirstStartedAt.IsZero() {
				row.FirstStartedAt = firstAt
			}
			v.putPush(row)
		}
		ready = append(ready, it)
	}
	if len(marks) == 0 {
		return nil
	}
	if err := commitPushes(r, "the write before a pushing batch"); err != nil {
		// Nothing was called: undo the marks so no phase 2 write records an
		// intent that never ran.
		for _, mk := range marks {
			if mk.existed {
				m.Put(model.TablePushes, mk.old)
			} else {
				m.Delete(model.TablePushes, mk.key.Parts())
			}
		}
		return err
	}

	for _, it := range ready {
		p.pushOne(it)
	}

	after := r.Now()
	for _, mk := range marks {
		row, ok := m.Pushes[mk.key]
		if !ok || row.IntentRun != r.ID {
			continue
		}
		row.IntentRun, row.UpdatedAt = "", after
		v.putPush(row)
	}
	return commitPushes(r, "the write after a pushing batch")
}

func stepKey(lead api.LeadID, lane, step string) model.Key {
	return model.K(string(lead), lane, step)
}

// pushOne pushes one lead to one lane: the lead's checks once more on the
// in-memory model, then each step not yet done, in order, each given the
// vendor ids of the steps before it (Prior) and this sink's done steps for
// other leads at the company (Related).
func (p *pusher) pushOne(it item) {
	r, m := p.r, p.r.Model
	v := r.view()
	if r.PushCtx.Err() != nil || p.stopped[it.lane.Sink] {
		return
	}
	if why := v.laneCheck(it.lead, it.lane); why != "" {
		p.cancel(it, why)
		return
	}
	s, steps, ok := p.sink(it.lane.ID, it.lane.Sink, it.lane.Dest)
	if !ok {
		return
	}
	prior := map[string]string{}
	for _, step := range steps {
		k := stepKey(it.lead, it.lane.ID, step)
		row := m.Pushes[k]
		if row.State == stateDone {
			prior[step] = row.VendorID
			continue
		}
		if row.State != statePending || r.PushCtx.Err() != nil {
			return
		}
		if v.dealStepRow(row) && v.dealWaits(it.lead) {
			r.log("info", "push_waiting", it.lead, fmt.Sprintf("lane %s: the deal step waits: another lead's deal step at the company was called and has no result yet", it.lane.ID))
			return
		}
		req := api.StepRequest{
			Key:     api.StepKey{LeadID: it.lead, LaneID: it.lane.ID, Step: step},
			Dest:    it.lane.Dest,
			Lead:    v.leadRef(it.lead),
			Prior:   copyMap(prior),
			Related: v.related(it.lead, it.lane.Sink),
		}
		id, err := s.Do(r.Ctx, req)
		if err == nil && id == "" {
			err = errors.New("the sink returned no vendor id")
		}
		if !p.record(it, k, id, err) {
			return
		}
		prior[step] = id
	}
	r.Pushed++
	if it.lane.Kind == kindCold {
		o := m.Outcomes[model.Key(it.lead)]
		if o.ContactedAt.IsZero() {
			o.LeadID, o.ContactedAt = it.lead, r.Now()
			m.Put(model.TableOutcomes, o)
		}
	}
}

// record writes one call's result to the ledger row (the ledger rules)
// and reports whether the push goes on to its next step.
func (p *pusher) record(it item, k model.Key, id string, err error) bool {
	r := p.r
	v := r.view()
	now := r.Now()
	row := r.Model.Pushes[k]
	if row.CalledAt.IsZero() {
		row.CalledAt = now // the call was made; never cleared
	}
	if v.idx.ReceiverOnly(it.lead) {
		receiverOnlyPush(r, it.lead)
	}
	row.UpdatedAt = now
	msg := ""
	if err != nil {
		msg = errText(err)
	}
	next := false
	switch {
	case err == nil:
		row.State, row.VendorID, row.LastError = stateDone, id, ""
		next = true
	case errors.Is(err, api.ErrRateLimited):
		// Stays pending with no attempt counted, and the sink stops for the run.
		row.LastError = msg
		p.stopped[it.lane.Sink] = true
	case errors.Is(err, api.ErrTransient), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		row.LastError = msg // stays pending, no attempt counted
	case errors.Is(err, api.ErrRefused):
		row.State, row.LastError = stateCancelled, "refused: "+msg
		r.log("info", logPushRefused, it.lead, fmt.Sprintf("lane %s step %s: the vendor refused: %s", it.lane.ID, row.Step, msg))
	default:
		row.Attempts++
		row.LastError = msg
		if row.Attempts >= maxAttempts {
			row.State = stateFailed
			fp := check.PushFailed(row)
			r.Problem(fp.Key, fp.Message, fp.Fix, fp.Warning)
		}
		r.log("warn", logPushFailed, it.lead, fmt.Sprintf("lane %s step %s: attempt %d failed: %s", it.lane.ID, row.Step, row.Attempts, msg))
	}
	v.putPush(row)
	return next
}

// cancel cancels a lead's pending steps in a lane, with the reason.
func (p *pusher) cancel(it item, why string) {
	r := p.r
	v := r.view()
	for _, row := range v.rows(it.lead) {
		if row.LaneID != it.lane.ID || row.State != statePending {
			continue
		}
		row.State, row.LastError, row.UpdatedAt = stateCancelled, why, r.Now()
		v.putPush(row)
	}
	r.log("info", logPushCancelled, it.lead, fmt.Sprintf("lane %s: pending steps cancelled just before the push: %s", it.lane.ID, why))
}

// related is StepRequest.Related: this sink's done steps for other leads at
// the lead's company, any lane, including steps finished earlier in this
// batch. A deal a lookup has shown won or lost is left out: only deals at an
// open stage are included.
func (v *view) related(id api.LeadID, sinkType string) []api.LedgerRef {
	d := v.domain(id)
	if d == "" {
		return nil
	}
	own := map[api.LeadID]bool{}
	for _, f := range v.family(id) {
		own[f] = true
	}
	c := v.company(d)
	var out []api.LedgerRef
	for _, l := range c.leads {
		if own[l] {
			continue
		}
		for _, row := range v.rows(l) {
			if row.State != stateDone || c.closed[row.VendorID] || !v.rowSink(row, sinkType) {
				continue
			}
			out = append(out, api.LedgerRef{
				Key:  api.StepKey{LeadID: row.LeadID, LaneID: row.LaneID, Step: row.Step},
				Dest: row.Dest, VendorID: row.VendorID, State: row.State,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.LeadID != b.LeadID {
			return a.LeadID < b.LeadID
		}
		if a.LaneID != b.LaneID {
			return a.LaneID < b.LaneID
		}
		return a.Step < b.Step
	})
	return out
}

// doneSteps is LeadRef.Done for a lookup of a sink type: the type's done
// steps for the lead and every lead merged into it, so a lookup can read the
// vendor records the lead already has (a HubSpot contact id). For the HubSpot
// lookup a contact step counts by its own step and destination (contacts or
// deals), whatever its lane says now, so renaming or removing a lane never
// hides a contact the lead has.
func (v *view) doneSteps(id api.LeadID, sinkType string) []api.LedgerRef {
	var out []api.LedgerRef
	for _, row := range v.familyRows(id) {
		hubspotContact := sinkType == dealSink && row.Step == "contact" && (row.Dest == dealDest || row.Dest == "contacts")
		if row.State != stateDone || row.VendorID == "" || !hubspotContact && !v.rowSink(row, sinkType) {
			continue
		}
		out = append(out, api.LedgerRef{
			Key:  api.StepKey{LeadID: row.LeadID, LaneID: row.LaneID, Step: row.Step},
			Dest: row.Dest, VendorID: row.VendorID, State: row.State,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.LeadID != b.LeadID {
			return a.LeadID < b.LeadID
		}
		if a.LaneID != b.LaneID {
			return a.LaneID < b.LaneID
		}
		return a.Step < b.Step
	})
	return out
}

// rowSink reports a row of a lane pushing to the sink type. A row of a lane
// since removed from the rubric counts for the deals sink when its
// destination is `deals` (its deal stays the company's); otherwise its sink
// can no longer be told.
func (v *view) rowSink(p model.Push, sinkType string) bool {
	if l, ok := v.lanes[p.LaneID]; ok {
		return l.Sink == sinkType
	}
	return sinkType == dealSink && p.Dest == dealDest
}

// commitPushes commits the ledger and State.ledger_rows under the lease
// (the run's commit rules: each pushing batch commits Pushes and
// ledger_rows), retrying once.
func commitPushes(r *Run, what string) (err error) {
	// ledger_rows is raised with the rows it counts, and put back when the
	// commit fails: the caller may undo the rows, and a count saved without
	// them would read as a ledger that shrank.
	before, had := r.Model.State[model.Key(ledgerRowsKey)]
	defer func() {
		if err == nil {
			return
		}
		if had {
			r.Model.Put(model.TableState, before)
		} else {
			r.Model.Delete(model.TableState, []string{ledgerRowsKey})
		}
	}()
	setLedgerRows(r.Model)
	writes := codec.Encode(r.Model, model.TablePushes, model.TableState+":"+ledgerRowsKey)
	if len(writes) == 0 {
		return nil
	}
	try := func() error {
		if err := r.Lease.Check(r.Ctx); err != nil {
			return err
		}
		return r.Store.Commit(r.Ctx, writes)
	}
	err = try()
	if errors.Is(err, api.ErrCommittedWithProblems) {
		r.Problem("people_tab_check", errText(err), "open the tab the message names and check its rows", true)
		err = nil // every write landed
	}
	if err != nil && !errors.Is(err, api.ErrLeaseLost) && r.Ctx.Err() == nil {
		err = try()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	r.Model.Committed(writes)
	return nil
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
