// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// pushRun is what PrePush hands to Push and to Ranked: who may push, and
// each lead's planned lane and skipped-lane reasons.
type pushRun struct {
	lookedUp map[api.LeadID]bool // leads whose lookups passed (every provisional lead on a dry run)
	planned  map[api.LeadID]string
	reasons  map[api.LeadID][]string
}

// prePushHook is the PrePush hook (step 8, the ledger rules). It makes the
// provisional selection (every open push, plus new pushes within the limits
// and a 10% margin), calls every configured Lookup for those leads when this
// run may push, and sends the deal lookup one live lead per company with a
// stored open deal or an export row, pushes on or off. A lead whose lookup
// failed waits; lookup events are applied through events.Apply, statuses are
// folded again, and the run re-derives and re-scores when a status changed.
// It then plans every lead's lane for Ranked and the dry-run report. A dry
// run calls no Lookup: its planned lanes assume lookups pass.
//
// The engine's candidate list is not needed: the selection reads every
// scored lead and its open rows itself.
func prePushHook(r *Run, _ []api.LeadID) error {
	st := &pushRun{lookedUp: map[api.LeadID]bool{}}
	r.pushing = st
	r.invalidate()
	v := r.view()
	items, _, _ := v.routeAll()
	order(items)
	provisional := map[api.LeadID]bool{}
	for _, it := range budget(items, withMargin(v.newPushes())) {
		provisional[it.lead] = true
	}

	if r.DryRun {
		st.lookedUp = provisional
	} else if err := lookups(r, v, provisional, st); err != nil {
		return err
	}

	r.invalidate()
	_, st.planned, st.reasons = r.view().routeAll()
	return nil
}

// lookups runs step 8's lookups and applies what they learn.
func lookups(r *Run, v *view, provisional map[api.LeadID]bool, st *pushRun) error {
	pushing := r.Config.PushesEnabled && r.NoPush == ""
	var types []string
	for typ := range r.Config.Sinks {
		if _, ok := lookupFactory(typ); ok {
			types = append(types, typ)
		}
	}
	sort.Strings(types)

	var cands []api.LeadID
	sent := map[api.LeadID]bool{} // the candidates every lookup gets
	if pushing {
		sent = provisional
		for id := range provisional {
			cands = append(cands, id)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i] < cands[j] })
	}
	failed := map[api.LeadID]bool{}
	for _, typ := range types {
		ids := cands
		if typ == dealLookup {
			ids = append(append([]api.LeadID(nil), cands...), dealCompanies(v, sent)...)
		}
		if len(ids) == 0 {
			continue
		}
		refs := make([]api.LeadRef, 0, len(ids))
		for _, id := range ids {
			ref := v.leadRef(id)
			ref.Done = v.doneSteps(id, typ)
			refs = append(refs, ref)
		}
		evs, bad, err := lookupOne(r.PushCtx, typ, r.Config.Sinks[typ], refs)
		if err != nil {
			for _, id := range ids {
				failed[id] = true
			}
			r.Problem("lookup_failed:"+typ, fmt.Sprintf("the %s lookup failed, so its %d lead(s) wait: %s", typ, len(ids), errText(err)),
				"check the key and the vendor's status; the next run looks them up again", false)
			r.log("warn", logLookupFailed, "", fmt.Sprintf("the %s lookup failed for %d lead(s): %s", typ, len(ids), errText(err)))
			continue
		}
		for id, e := range bad {
			failed[id] = true
			r.log("warn", logLookupFailed, id, fmt.Sprintf("the %s lookup failed for this lead, which waits: %s", typ, errText(e)))
		}
		if len(bad) > 0 {
			r.Problem("lookup_failed:"+typ, fmt.Sprintf("the %s lookup failed for %d lead(s); they wait for the next run", typ, len(bad)),
				"see the Log's lookup_failed lines; the next run looks them up again", true)
		}
		for _, id := range applyLookupEvents(r, typ, evs, bad) {
			failed[id] = true
		}
	}
	for id := range provisional {
		if !failed[id] && (pushing || len(types) == 0) {
			st.lookedUp[id] = true
		}
	}

	r.invalidate()
	changed := foldStatuses(r, r.view())
	if len(changed) == 0 {
		return nil
	}
	// A status a lookup changed can change derived values, scores and lanes:
	// derive and score again with the new statuses.
	status := map[api.LeadID]string{}
	for _, id := range changed {
		status[id] = r.Model.Outcomes[model.Key(id)].Status
	}
	for i := range r.Input.Leads {
		if s, ok := status[r.Input.Leads[i].ID]; ok {
			r.Input.Leads[i].Status = s
		}
	}
	res, err := r.Rubric.EvaluateContext(r.PushCtx, r.Input)
	if err != nil {
		return fmt.Errorf("scoring again after the lookups: %w", err)
	}
	r.Result = res
	return nil
}

func lookupOne(ctx context.Context, typ string, block api.Config, leads []api.LeadRef) ([]api.Event, map[api.LeadID]error, error) {
	f, _ := lookupFactory(typ)
	l, err := f(block)
	if err != nil {
		return nil, nil, err
	}
	return l.Lookup(ctx, leads)
}

// dealCompanies picks, for the deal lookup, one live lead (the lowest id) at
// each company with a stored open deal (in Outcomes, or a called deal step in
// the ledger, even one that timed out) or a row in any Export table, unless a
// candidate there is already being looked up (the ledger rules). A
// closed-lost deal, or a company found with no open or won deal, then
// releases the company even when no lead there is a candidate (step 8 of
// the run).
func dealCompanies(v *view, sent map[api.LeadID]bool) []api.LeadID {
	domains := map[string]bool{}
	for _, id := range v.idx.LiveLeads() {
		if d := v.domain(id); d != "" && !domains[d] && (v.companyDealID(id) != "" || v.deal(id, "")) {
			domains[d] = true
		}
	}
	for _, rows := range v.m.Exports {
		for _, row := range rows {
			if d := v.domain(v.live(row.LeadID)); d != "" {
				domains[d] = true
			}
		}
	}
	var out []api.LeadID
	for d := range domains {
		var pick api.LeadID
		covered := false
		for _, id := range v.m.PeopleAt(d) {
			if v.m.People[model.Key(id)].MergedInto != "" {
				continue
			}
			covered = covered || sent[id]
			if pick == "" || id < pick {
				pick = id
			}
		}
		if pick != "" && !covered {
			out = append(out, pick)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// leadRef is the lead as a sink or lookup sees it: its scored LeadRef with
// the current folded status and its company's stored deal.
func (v *view) leadRef(id api.LeadID) api.LeadRef {
	var ref api.LeadRef
	if i, ok := v.refs[id]; ok {
		ref = v.r.Input.Leads[i]
	} else {
		ref = api.LeadRef{ID: id, Emails: v.idx.Emails(id), LinkedInURLs: v.idx.LinkedInURLs(id), Domain: v.domain(id)}
	}
	if verdict, ok := v.r.Result.Verdicts[id]; ok {
		ref.Verdict = &verdict
	}
	ref.Status = v.status(id)
	ref.CompanyDealID = v.companyDealID(id)
	return ref
}

// applyLookupEvents applies a lookup's events (the event kinds optout
// and deal_*), each to the lead it names, through events.Apply. Lookup events
// are not de-duplicated: applying them is idempotent. An event with no time
// takes the run's clock, so an opt-out is never stored at the zero time.
//
// A lookup that failed for some leads may have read their companies only in
// part: its deal_lost for a domain with a failed lead is dropped, so a
// company is never released on an incomplete answer. A deal_lost for a
// company whose deal step was called less than dealSearchLag ago with no
// deal id back is dropped too, since that deal may not show in search yet;
// the company's live leads are returned, to wait as if their lookup failed.
func applyLookupEvents(r *Run, typ string, evs []api.Event, failed map[api.LeadID]error) []api.LeadID {
	origin := events.LookupOrigin(typ)
	v := r.view()
	unread := map[string]bool{}
	for id := range failed {
		if d := r.Model.People[model.Key(merge.Live(r.Model, id))].Fields[model.CompanyDomainField].Value; d != "" {
			unread[d] = true
		}
	}
	var wait []api.LeadID
	held := map[string]bool{}
	for _, e := range evs {
		e = merge.NormalizeEventKeys(e)
		e.Kind = strings.ToLower(e.Kind)
		e.Origin = origin
		if e.ReceivedAt.IsZero() {
			e.ReceivedAt = r.Now()
		}
		if e.At.IsZero() || strings.HasPrefix(e.Kind, "deal_") {
			// deal_checked_at is the lookup time (the status rules): a
			// release compares it with the deal step's called_at.
			e.At = r.Now()
		}
		if e.Kind != "optout" && !strings.HasPrefix(e.Kind, "deal_") {
			r.log("warn", "event_ignored", "", fmt.Sprintf("the %s lookup returned a %s event; lookups report only optout and deal_* events", typ, clip(logredact.Redact(e.Kind))))
			continue
		}
		if e.Kind == "deal_lost" && unread[e.Domain] {
			r.log("info", "event_ignored", "", fmt.Sprintf("the %s lookup failed for a lead at a company it reported without a deal; the company stays held until a full lookup", typ))
			continue
		}
		if e.Kind == "deal_lost" && e.Domain != "" && v.recentDealCall(e.Domain, r.Now()) {
			if !held[e.Domain] {
				held[e.Domain] = true
				for _, id := range v.companyLeads(e.Domain) {
					if r.Model.People[model.Key(id)].MergedInto == "" {
						wait = append(wait, id)
						r.log("info", logLookupFailed, id, fmt.Sprintf("the %s lookup found no open deal at this lead's company, but a deal step there was called "+
							"less than %s ago and its deal may not show in search yet: the company stays held and the lead waits", typ, dealSearchLag))
					}
				}
			}
			continue
		}
		lead, _ := merge.FindPerson(r.Model, e)
		events.Apply(r.Model, lead, e, r.Config.ReplyLabels)
	}
	return wait
}
