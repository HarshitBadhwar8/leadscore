package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/detect"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// State keys Intake writes (contracts section 4).
const (
	eventsCursorKey    = "cursor:events"
	lastPollKey        = "last_poll_at"
	lastReceivedPrefix = "last_received:"
)

// Fixed values (contracts section 11). Variables so tests can change them.
var (
	pollInterval    = 6 * time.Hour
	windowRetention = 90 * 24 * time.Hour  // Window events, and the age past which processed events are deleted
	seenRetention   = 365 * 24 * time.Hour // Seen events
)

// Log kinds Intake writes.
const (
	logRowRejected   = "row_rejected"   // a source event row that cannot be an event
	logEventRejected = "event_rejected" // a stored receiver request that cannot be applied
	logEventNoPerson = "event_unmatched"
)

// intake is the Intake hook (step 3, contracts section 12.7). It reads the
// event log from cursor:events, parses it, merges the receiver rows, polls
// for replies when due, and then takes every event (receiver, polled and the
// sources' Run.SourceEvents) once: keyed, skipped when already in Seen events,
// its person resolved (and created for the kinds that create one), its effect
// applied, and a Window events row and first-seen times written. The keys,
// the cursor and the last poll time go to phase 1 with everything else.
//
// An event log that shrank below the cursor returns ErrEventsShrank unchanged,
// after every other event was taken: the run then scores but does not push.
func intake(r *Run) error {
	m := r.Model
	now := r.Now()
	cursor := api.Cursor(m.StateValue(eventsCursorKey))
	raws, next, err := r.Events.ReadEvents(r.Ctx, cursor)
	var shrank error
	switch {
	case errors.Is(err, api.ErrEventsShrank):
		shrank, raws = err, nil
	case err != nil:
		return fmt.Errorf("reading the event log: %w", err)
	}

	parsed, rows := events.Parse(raws)
	if len(rows) > 0 {
		aliases := r.Rubric.Aliases()
		norm := make([]merge.Normalized, 0, len(rows))
		for _, row := range rows {
			norm = append(norm, merge.Normalize(row, aliases))
		}
		merge.Apply(m, norm, merge.ApplyCtx{Now: now, RunID: r.ID, Sources: r.Config.Sources, Aliases: aliases})
	}

	in := &intaker{r: r, m: m, now: now, taken: map[api.EventID]bool{}, received: map[string]time.Time{}}
	for _, e := range parsed {
		in.take(merge.NormalizeEventKeys(e))
	}
	for _, e := range poll(r) {
		in.take(merge.NormalizeEventKeys(e))
	}
	for _, e := range r.SourceEvents {
		in.take(e) // normalized by the run when it fetched the sources
	}

	if shrank == nil && next != cursor {
		m.SetState(eventsCursorKey, string(next))
	}
	for kind, at := range in.received {
		key := lastReceivedPrefix + kind
		if old, err := model.ParseTime(m.StateValue(key)); err != nil || old.Before(at) {
			m.SetState(key, model.FormatTime(at))
		}
	}
	return shrank
}

// intaker takes the events of one Intake.
type intaker struct {
	r        *Run
	m        *model.Model
	now      time.Time
	taken    map[api.EventID]bool
	received map[string]time.Time // newest receiver event per kind
}

func (in *intaker) take(e api.Event) {
	m, r := in.m, in.r
	if e.Origin == events.OriginReceiver && e.Kind != "" {
		if t := e.ReceivedAt.UTC(); t.After(in.received[strings.ToLower(e.Kind)]) {
			in.received[strings.ToLower(e.Kind)] = t
		}
	}
	if e.Kind == "" {
		in.reject(e, e.Attrs[events.AttrReject])
		return
	}
	e.Kind = strings.ToLower(e.Kind)
	if e.Origin != events.OriginReceiver && e.Origin != events.OriginPolling && events.VendorOnly(e.Kind) {
		in.reject(e, fmt.Sprintf("a %s event may only come from a vendor, not from source %s", e.Kind, e.Origin))
		return
	}

	e.ID = events.Key(e)
	if in.taken[e.ID] {
		return
	}
	in.taken[e.ID] = true
	if _, seen := m.SeenEvents[model.Key(e.ID)]; seen {
		return
	}
	m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: string(e.ID), FirstReceivedAt: in.now, RunID: r.ID})

	// With replies by polling, a receiver reply is keyed but has no effect and
	// no window row, so one reply never counts twice (contracts section 5.3).
	if r.Config.Replies == "polling" && e.Origin == events.OriginReceiver && (e.Kind == "replied" || e.Kind == "replied_positive") {
		return
	}

	var lead api.LeadID
	if events.CreatesLead(e.Kind) {
		lead = merge.ApplyEventPerson(m, e)
	} else if id, ok := merge.FindPerson(m, e); ok {
		lead = id
	}
	if lead == "" && (e.Email != "" || e.LinkedInURL != "") && events.CreatesLead(e.Kind) {
		in.log("warn", logEventNoPerson, "", fmt.Sprintf("a %s event (from %s) names no usable person key; it was not applied to any lead", e.Kind, e.Origin))
	}
	events.Apply(m, lead, e, r.Config.ReplyLabels)

	at := e.At.UTC()
	if at.IsZero() {
		at = e.ReceivedAt.UTC()
	}
	domain := e.Domain
	if domain == "" && lead != "" {
		domain = m.People[model.Key(lead)].Fields[model.CompanyDomainField].Value
	}
	if lead == "" && domain == "" {
		return
	}
	if lead != "" {
		in.firstSeenLead(lead, e.Kind, at)
	}
	if domain != "" {
		in.firstSeenCompany(domain, e.Kind, at)
	}
	if !at.After(in.now.Add(-windowRetention)) {
		return // older than any window: it would be trimmed at once
	}
	if _, ok := m.WindowEvents[model.Key(e.ID)]; ok {
		return
	}
	w := model.WindowEvent{EventKey: string(e.ID), Subject: "company", LeadID: lead, Domain: domain, Kind: e.Kind, At: at, Attrs: windowAttrs(e.Attrs)}
	if lead != "" {
		w.Subject = "lead"
	}
	m.Put(model.TableWindowEvents, w)
}

// windowAttrs keeps an event's attributes for detectors, without the person's
// name, title and company, which People already holds.
func windowAttrs(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		switch k {
		case "full_name", "title", "company", events.AttrReject:
			continue
		}
		out[k] = v
	}
	return out
}

func (in *intaker) firstSeenLead(lead api.LeadID, kind string, at time.Time) {
	p, ok := in.m.People[model.Key(lead)]
	if !ok {
		return
	}
	if cur, has := p.FirstSeen[kind]; has && !at.Before(cur) {
		return
	}
	p.FirstSeen = withTime(p.FirstSeen, kind, at)
	in.m.Put(model.TablePeople, p)
}

func (in *intaker) firstSeenCompany(domain, kind string, at time.Time) {
	cf, ok := in.m.CompanyFacts[model.Key(domain)]
	if !ok {
		cf = model.CompanyFact{Domain: domain}
	}
	if cur, has := cf.FirstSeen[kind]; has && !at.Before(cur) {
		return
	}
	cf.FirstSeen = withTime(cf.FirstSeen, kind, at)
	in.m.Put(model.TableCompanyFacts, cf)
}

// withTime returns a copy of times with kind set to at.
func withTime(times map[string]time.Time, kind string, at time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(times)+1)
	for k, v := range times {
		out[k] = v
	}
	out[kind] = at
	return out
}

// reject logs an event that cannot be applied. A receiver request is read
// once (the cursor moves past it); a source row comes back every run from a
// snapshot source, so it is logged once, under a key in Seen events.
func (in *intaker) reject(e api.Event, reason string) {
	if reason == "" {
		reason = "no event kind"
	}
	kind := logEventRejected
	if e.Origin != events.OriginReceiver && e.Origin != events.OriginPolling {
		kind = logRowRejected
		key := model.Key("reject|" + e.Origin + "|" + reason)
		if _, seen := in.m.SeenEvents[key]; seen {
			return
		}
		in.m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: string(key), FirstReceivedAt: in.now, RunID: in.r.ID})
		reason = "source " + e.Origin + ": " + reason
	}
	in.log("warn", kind, "", reason)
}

func (in *intaker) log(level, kind string, lead api.LeadID, msg string) {
	in.m.Put(model.TableLog, model.LogEntry{At: in.now, RunID: in.r.ID, Level: level, LeadID: lead, Kind: kind,
		Message: logredact.Redact(msg), RubricVersion: in.r.Rubric.Version()})
}

// poll reads replies with every configured Poller when `replies: polling` and
// the last poll is older than the polling interval; never on a dry run. The
// window starts at the earlier of now − (sequence_length + window_margin) and
// last_poll_at − window_margin, so an outage longer than the window loses
// nothing. last_poll_at moves only when every poller succeeded. Polled replies
// come back ordered by received time, then message id, then label.
func poll(r *Run) []api.Event {
	cfg, m := r.Config, r.Model
	if cfg.Replies != "polling" || r.DryRun {
		return nil
	}
	now := r.Now()
	last, err := model.ParseTime(m.StateValue(lastPollKey))
	if err != nil {
		last = time.Time{}
	}
	if !last.IsZero() && now.Sub(last) < pollInterval {
		return nil
	}
	since := now.Add(-(cfg.Polling.SequenceLength + cfg.Polling.WindowMargin))
	if !last.IsZero() {
		if t := last.Add(-cfg.Polling.WindowMargin); t.Before(since) {
			since = t
		}
	}

	var types []string
	for typ := range cfg.Sinks {
		if _, ok := api.PollerFactory(typ); ok {
			types = append(types, typ)
		}
	}
	sort.Strings(types)
	if len(types) == 0 {
		r.Problem("poll_failed:none", "replies: polling is set, but no configured sink has a reply poller in this build, so no replies are read",
			"configure the sink that sends (sinks.apollo), or set replies: receiver", false)
		return nil
	}
	var out []api.Event
	ok := true
	for _, typ := range types {
		evs, err := pollOne(r.Ctx, typ, cfg.Sinks[typ], since)
		if err != nil {
			ok = false
			r.Problem("poll_failed:"+typ, fmt.Sprintf("reading replies from %s failed: %v", typ, err),
				"check the key and the vendor's status; the next poll reads the same window again", false)
			continue
		}
		for _, e := range evs {
			if e.Origin == "" {
				e.Origin = events.OriginPolling
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.Before(b.ReceivedAt)
		}
		if a.Attrs["message_id"] != b.Attrs["message_id"] {
			return a.Attrs["message_id"] < b.Attrs["message_id"]
		}
		return a.Attrs["label"] < b.Attrs["label"]
	})
	if ok {
		m.SetState(lastPollKey, model.FormatTime(now))
	}
	return out
}

func pollOne(ctx context.Context, typ string, block api.Config, since time.Time) ([]api.Event, error) {
	f, _ := api.PollerFactory(typ)
	p, err := f(block)
	if err != nil {
		return nil, err
	}
	return p.Poll(ctx, since)
}

// detectHook is the Detect hook (step 6): every rubric detector over the live
// leads and their companies. A registered kind missing from the build raises
// step_failed:detect and does not fire; the others still count.
func detectHook(r *Run) (rules.DetectorResults, error) {
	leads := leadRefs(r.Model, merge.NewIndex(r.Model, r.Config.Sources))
	res, errs := detect.Evaluate(r.Rubric.Detectors(), r.Model, leads, r.Now())
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		r.Problem("step_failed:detect", strings.Join(msgs, "; "),
			"use a build that registers the detector kind, or remove the detector from the rubric", false)
	}
	return res, nil
}

// trimEvents records the Window events (90 days) and Seen events (one year)
// retention trims; the run writes them in phase 2.
func trimEvents(r *Run) {
	r.Model.Trim(model.TableWindowEvents, "at", r.Now().Add(-windowRetention))
	r.Model.Trim(model.TableSeenEvents, "first_received_at", r.Now().Add(-seenRetention))
}

// deleteProcessed is S9's AfterSave step (RFC 6.6): it deletes stored events
// at or below the committed cursor that are older than the window retention.
// Every fact a later run needs is already in Window events, Seen events and
// Outcomes. When the store drops a partition from the cursor, the new cursor
// is committed at once, under the lease, so the next run never reads a
// cursor naming a partition that is gone.
func deleteProcessed(r *Run) error {
	if r.DryRun || r.Events == nil {
		return nil
	}
	cur := r.Model.StateValue(eventsCursorKey)
	if cur == "" {
		return nil
	}
	if r.Lease != nil {
		if err := r.Lease.Check(r.Ctx); err != nil {
			return fmt.Errorf("deleting processed events: %w", err)
		}
	}
	next, err := r.Events.DeleteProcessed(r.Ctx, api.Cursor(cur), r.Now().Add(-windowRetention))
	if err != nil {
		return fmt.Errorf("deleting processed events: %w", err)
	}
	if string(next) == cur {
		return nil
	}
	r.Model.SetState(eventsCursorKey, string(next))
	writes := codec.Encode(r.Model, model.TableState+":"+eventsCursorKey)
	if r.Lease != nil {
		if err := r.Lease.Check(r.Ctx); err != nil {
			return fmt.Errorf("saving the event cursor after deleting processed events: %w", err)
		}
	}
	if err := r.Store.Commit(r.Ctx, writes); err != nil {
		return fmt.Errorf("saving the event cursor after deleting processed events: %w", err)
	}
	r.Model.Committed(writes)
	return nil
}
