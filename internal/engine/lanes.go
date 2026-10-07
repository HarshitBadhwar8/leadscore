package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// Lane kinds (contracts section 2).
const (
	kindCold    = "cold"
	kindNonCold = "non-cold"
	kindExport  = "export"
)

// Ledger states (contracts section 8).
const (
	statePending   = "pending"
	stateDone      = "done"
	stateFailed    = "failed"
	stateCancelled = "cancelled"
)

// Statuses the engine reads (RFC 6.3).
const (
	statusNew          = "new"
	statusContacted    = "contacted"
	statusDeal         = "deal"
	statusUnsubscribed = "unsubscribed"
	statusBlocked      = "blocked"
)

// Fixed values (contracts section 11).
const (
	maxAttempts  = 3  // counted attempts before a step is failed
	batchLeads   = 25 // leads per pushing batch
	lookupMargin = 10 // percent: extra new pushes looked up to replace leads a lookup removes
	// dealSearchLag is how long after a deal step's latest call (with no
	// deal id back) a lookup's "no deal" answer for its company is not
	// trusted: HubSpot's search can lag a fresh create. S0 confirms the lag
	// is seconds. The company stays held until a lookup at least 15 minutes
	// after the step's last call; a step retried every run keeps it held
	// until it settles.
	dealSearchLag = 15 * time.Minute
)

// The sinks and destinations the engine's own rules name (RFC 6.10, 6.12):
// an Apollo sequence lane (the Apollo-held rule) and a HubSpot deals lane
// (the deal rule, and its need for a company).
const (
	apolloSink     = "apollo"
	sequencePrefix = "sequence/"
	dealSink       = "hubspot"
	dealDest       = "deals"
	// dealStep is the step of a deals push whose vendor id is the deal
	// (contracts section 6: hubspot:deals has steps contact, deal).
	dealStep = "deal"
)

// Factories, variables so tests can point a sink or lookup type at a fake
// without registering it for every test in the binary.
var (
	sinkFactory   = api.SinkFactory
	lookupFactory = api.LookupFactory
	// dealLookup is the lookup type that also reads company deals: step 8
	// sends it one lead per company with a stored open deal or an export row.
	dealLookup = "hubspot"
)

// blocksCold reports a status that blocks cold lanes (RFC 6.3).
func blocksCold(status string) bool {
	switch status {
	case statusDeal, statusUnsubscribed, statusBlocked:
		return true
	}
	return strings.HasPrefix(status, "replied_")
}

func isSequence(l rules.Lane) bool {
	return l.Sink == apolloSink && strings.HasPrefix(l.Dest, sequencePrefix)
}

func isDealLane(l rules.Lane) bool { return l.Sink == dealSink && l.Dest == dealDest }

// holdsCold is the one-cold-push rule (RFC 6.10 rule 3, contracts section
// 8): a cold step holds a lead's one cold push once its call may have gone
// out, whatever the outcome: called_at is set (a timeout or a refusal
// included), intent_run is set (the pre-batch write ran, so a crash may have
// left the call in flight), or the step is done or failed. Only a cold step
// never called releases it. cold says whether the row counts as cold: its
// stored kind, or its lane's current kind (view.isCold), so changing a
// lane's kind can never release a hold. Its backups: a stored cold kind is
// never downgraded, the intent_run left by a crash becomes called_at at
// load, and the check just before each push reads the in-memory ledger.
func holdsCold(p model.Push, cold bool) bool {
	if !cold {
		return false
	}
	return !p.CalledAt.IsZero() || p.IntentRun != "" || p.State == stateDone || p.State == stateFailed
}

// view reads the model for lane decisions: the merge index (families and
// emails), the duplicates, the parsed Overrides, the rubric's lanes and the
// ledger by lead. Statuses, outcomes and ledger rows are read live from the
// model; the rest is a snapshot, so the run rebuilds the view (invalidate)
// after it re-reads Overrides or the model gains people.
type view struct {
	r      *Run
	m      *model.Model
	idx    *merge.Index
	dups   map[api.LeadID]bool
	ov     merge.Overrides
	lanes  map[string]rules.Lane
	refs   map[api.LeadID]int         // index into r.Input.Leads
	byLead map[api.LeadID][]model.Key // ledger rows under each lead id
	doms   map[string]*company        // per-domain deal summaries, built lazily
}

// view returns the run's view, building it on first use.
func (r *Run) view() *view {
	if r.lv == nil {
		r.lv = newView(r)
	}
	return r.lv
}

// invalidate drops the view, so the next use rebuilds it.
func (r *Run) invalidate() { r.lv = nil }

func newView(r *Run) *view {
	m := r.Model
	v := &view{
		r: r, m: m,
		idx:    merge.NewIndex(m, r.Config.Sources),
		dups:   merge.Duplicates(m),
		ov:     merge.ParseOverrides(m),
		lanes:  map[string]rules.Lane{},
		refs:   map[api.LeadID]int{},
		byLead: map[api.LeadID][]model.Key{},
		doms:   map[string]*company{},
	}
	for _, l := range r.Rubric.Lanes() {
		v.lanes[l.ID] = l
	}
	for i, ref := range r.Input.Leads {
		v.refs[ref.ID] = i
	}
	for k, p := range m.Pushes {
		v.byLead[p.LeadID] = append(v.byLead[p.LeadID], k)
	}
	return v
}

// isCold reports a row that counts as cold: its stored kind or its lane's
// current kind is cold.
func (v *view) isCold(p model.Push) bool {
	if p.LaneKind == kindCold {
		return true
	}
	l, ok := v.lanes[p.LaneID]
	return ok && l.Kind == kindCold
}

// holds reports a row holding its lead's one cold push.
func (v *view) holds(p model.Push) bool { return holdsCold(p, v.isCold(p)) }

// putPush writes a ledger row, keeping the view's index current.
func (v *view) putPush(p model.Push) {
	k := model.K(string(p.LeadID), p.LaneID, p.Step)
	if _, ok := v.m.Pushes[k]; !ok {
		v.byLead[p.LeadID] = append(v.byLead[p.LeadID], k)
		if v.dealStepRow(p) {
			v.doms = map[string]*company{} // a new deal step: the companies' summaries change
		}
	}
	v.m.Put(model.TablePushes, p)
}

// live follows merged_into to the live lead.
func (v *view) live(id api.LeadID) api.LeadID { return merge.Live(v.m, id) }

// family is a live lead and every lead merged into it.
func (v *view) family(id api.LeadID) []api.LeadID {
	if f := v.idx.Family(id); len(f) > 0 {
		return f
	}
	return []api.LeadID{id}
}

// rows returns the ledger rows under one lead id (not its family).
func (v *view) rows(id api.LeadID) []model.Push {
	out := make([]model.Push, 0, len(v.byLead[id]))
	for _, k := range v.byLead[id] {
		if p, ok := v.m.Pushes[k]; ok {
			out = append(out, p)
		}
	}
	return out
}

// familyRows returns the ledger rows of a live lead's whole family.
func (v *view) familyRows(id api.LeadID) []model.Push {
	var out []model.Push
	for _, f := range v.family(id) {
		out = append(out, v.rows(f)...)
	}
	return out
}

func (v *view) status(id api.LeadID) string { return v.m.Outcomes[model.Key(id)].Status }

func (v *view) domain(id api.LeadID) string {
	return v.m.People[model.Key(id)].Fields[model.CompanyDomainField].Value
}

// unsubscribed reports an opt-out anywhere in the lead's family (contracts
// section 7: the fold reads Outcomes across the lead and every lead merged
// into it). It is read from Outcomes directly, so an opt-out learned after
// the last fold still blocks.
func (v *view) unsubscribed(id api.LeadID) bool {
	for _, f := range v.family(id) {
		if !v.m.Outcomes[model.Key(f)].UnsubscribedAt.IsZero() {
			return true
		}
	}
	return false
}

// blocked reports a lead blocked on every lane, and why: unsubscribed,
// blocked by Overrides (conflicting rows, an unknown value, or a manual
// `blocked`), an unresolved duplicate (a namesake or a merge cycle), or a
// rubric conflict. id must be live.
func (v *view) blocked(id api.LeadID) (bool, string) {
	if _, ok := v.m.People[model.Key(id)]; !ok {
		return true, "no such lead"
	}
	if v.status(id) == statusUnsubscribed || v.unsubscribed(id) {
		return true, "unsubscribed"
	}
	if why, ok := v.ov.Blocked[id]; ok {
		return true, "blocked in Overrides: " + why
	}
	if v.status(id) == statusBlocked {
		return true, "blocked in Overrides"
	}
	if v.dups[id] {
		return true, "an unresolved duplicate (a namesake, or a merged_into cycle)"
	}
	if why, ok := v.r.Result.Blocked[id]; ok {
		return true, "a rubric conflict: " + why
	}
	return false, ""
}

// matchesWhen reports whether the lane's `when` held for the lead at scoring.
func (v *view) matchesWhen(id api.LeadID, lane string) bool {
	for _, l := range v.r.Result.Lanes[id] {
		if l == lane {
			return true
		}
	}
	return false
}

// apolloHeld reports apollo_held_at set on the lead or a lead merged into it
// (RFC 6.10, rule 4).
func (v *view) apolloHeld(id api.LeadID) bool {
	for _, f := range v.family(id) {
		if !v.m.People[model.Key(f)].ApolloHeldAt.IsZero() {
			return true
		}
	}
	return false
}

// laneCheck is every check that cancels a lead's pending steps in a lane
// (contracts section 8): the built-in checks, the lane's `when`, the
// Apollo-held rule on an Apollo sequence lane, and for a cold lane the
// cold-lane statuses, the deal rule and the one cold push. It returns why
// the lead fails, or "" when it passes. id must be live.
func (v *view) laneCheck(id api.LeadID, l rules.Lane) string {
	if b, why := v.blocked(id); b {
		return why
	}
	if l.Kind != kindExport {
		email := v.idx.PrimaryEmail(id)
		if email == "" || merge.ValidateEmailShape(email) != nil {
			return "no valid email"
		}
	}
	if !v.matchesWhen(id, l.ID) {
		return "the lane's when does not hold"
	}
	if isSequence(l) && v.apolloHeld(id) {
		return "Apollo already holds this lead"
	}
	if isDealLane(l) && v.domain(id) == "" {
		return "no company domain"
	}
	if l.Kind != kindCold {
		return ""
	}
	if s := v.status(id); blocksCold(s) {
		return "status " + s
	}
	if v.deal(id, l.ID) {
		return "the company has an open or won deal"
	}
	if other := v.coldHeldElsewhere(id, l.ID); other != "" {
		return "the lead's one cold push is held by lane " + other
	}
	return ""
}

// coldHeldElsewhere returns the lane of a row in the lead's family that holds
// its one cold push, other than the lead's own rows in lane; "" when none.
func (v *view) coldHeldElsewhere(id api.LeadID, lane string) string {
	for _, p := range v.familyRows(id) {
		if v.holds(p) && (p.LeadID != id || p.LaneID != lane) {
			return p.LaneID
		}
	}
	return ""
}

// company is what the deal rule reads about a set of leads (a company's
// leads, or one lead's merge family): whether one has a deal at an open or
// won stage, the latest lookup that found a deal lost, the deal ids at a
// stage other than open, and the ledger's deal steps.
type company struct {
	leads    []api.LeadID
	open     bool
	openID   string    // an open or won deal's id, when one has an id
	lostAt   time.Time // the latest deal_checked_at with stage lost
	checked  time.Time // the latest deal_checked_at at any stage
	closed   map[string]bool
	dealRows []model.Key
}

// summarize reads Outcomes and the ledger for a set of leads.
func (v *view) summarize(leads []api.LeadID) *company {
	c := &company{leads: leads, closed: map[string]bool{}}
	for _, l := range leads {
		o := v.m.Outcomes[model.Key(l)]
		switch o.DealStage {
		case "open", "won":
			c.open = true
			if c.openID == "" {
				c.openID = o.DealID
			}
		case "lost":
			if o.DealCheckedAt.After(c.lostAt) {
				c.lostAt = o.DealCheckedAt
			}
		}
		if o.DealCheckedAt.After(c.checked) {
			c.checked = o.DealCheckedAt
		}
		if o.DealID != "" && o.DealStage != "" && o.DealStage != "open" {
			c.closed[o.DealID] = true
		}
		for _, k := range v.byLead[l] {
			if p, ok := v.m.Pushes[k]; ok && v.dealStepRow(p) {
				c.dealRows = append(c.dealRows, k)
			}
		}
	}
	return c
}

// company returns a domain's summary, built once per view: its live leads
// and every lead merged into them. A new deal step row drops the cache
// (putPush); a lookup or re-read that changes Outcomes rebuilds the view.
func (v *view) company(domain string) *company {
	if c, ok := v.doms[domain]; ok {
		return c
	}
	seen := map[api.LeadID]bool{}
	var leads []api.LeadID
	for _, id := range v.m.PeopleAt(domain) {
		for _, f := range v.family(v.live(id)) {
			if !seen[f] {
				seen[f] = true
				leads = append(leads, f)
			}
		}
	}
	c := v.summarize(leads)
	v.doms[domain] = c
	return c
}

// companyLeads returns the live leads at a domain and every lead merged into
// them.
func (v *view) companyLeads(domain string) []api.LeadID { return v.company(domain).leads }

// dealFacts merges the lead's family and its company: what the deal rule and
// CompanyDealID read.
func (v *view) dealFacts(id api.LeadID) []*company {
	out := []*company{v.summarize(v.family(id))}
	if d := v.domain(id); d != "" {
		out = append(out, v.company(d))
	}
	return out
}

// deal is the deal rule (contracts section 7, rule 4), read from the
// in-memory model: the lead or any lead at its company has a deal at an open
// or won stage, or a hubspot:deals `deal` step at the company was called (the
// deal may exist even if the call timed out) and no lookup since has shown the
// company with no open or won deal. A step marked by this run's pre-batch
// write but not yet called is no deal yet (an intent_run left by another run
// became called_at at load). The lead's own push in exceptLane is left out,
// so a cold deals lane does not block itself.
func (v *view) deal(id api.LeadID, exceptLane string) bool {
	facts := v.dealFacts(id)
	var lostAt time.Time
	for _, c := range facts {
		if c.open {
			return true
		}
		if c.lostAt.After(lostAt) {
			lostAt = c.lostAt
		}
	}
	for _, c := range facts {
		for _, k := range c.dealRows {
			p := v.m.Pushes[k]
			if p.LeadID == id && p.LaneID == exceptLane {
				continue
			}
			if (p.IntentRun != "" && p.IntentRun != v.r.ID) || (!p.CalledAt.IsZero() && !lostAt.After(p.CalledAt)) {
				return true
			}
		}
	}
	return false
}

// dealStepRow reports a ledger row of a deals push's `deal` step, the only
// step that can create a deal: its own step and destination decide, whatever
// its lane says now, so editing or removing a deals lane never drops a
// company's deal hold.
func (v *view) dealStepRow(p model.Push) bool { return p.Step == dealStep && p.Dest == dealDest }

// dealWaits reports that the lead's deal step must wait (contracts section
// 8): another lead at its company has a deal step that was called and has no
// result yet (no deal id; pending, or cancelled after its call), and no
// lookup has read the company since that call. The deal may exist, so a
// second deal step would open a second deal; it waits for a later run, when
// the first has its deal id (passed in Related) or a lookup has the answer.
func (v *view) dealWaits(id api.LeadID) bool {
	d := v.domain(id)
	if d == "" {
		return false
	}
	own := map[api.LeadID]bool{}
	for _, f := range v.family(id) {
		own[f] = true
	}
	c := v.company(d)
	for _, k := range c.dealRows {
		p := v.m.Pushes[k]
		if own[p.LeadID] || p.VendorID != "" || p.CalledAt.IsZero() || (p.State != statePending && p.State != stateCancelled) {
			continue
		}
		if !c.checked.After(p.CalledAt) {
			return true
		}
	}
	return false
}

// recentDealCall reports a deal step at the company whose latest call was
// less than dealSearchLag ago and has no deal id: its deal may exist and not
// yet show in HubSpot's search, so a lookup's deal_lost for the company is
// not trusted (contracts section 8). called_at keeps the first call, so the
// latest is read from updated_at, which every call's result write sets
// (any later write only makes the hold longer: the safe direction).
func (v *view) recentDealCall(domain string, now time.Time) bool {
	for _, k := range v.company(domain).dealRows {
		p := v.m.Pushes[k]
		if p.VendorID != "" || p.CalledAt.IsZero() {
			continue
		}
		last := p.CalledAt
		if p.UpdatedAt.After(last) {
			last = p.UpdatedAt
		}
		if now.Sub(last) < dealSearchLag {
			return true
		}
	}
	return false
}

// companyDealID is LeadRef.CompanyDealID: the stored open or won deal at the
// lead's company, from Outcomes, else the newest done deal step there whose
// deal no lookup has shown won or lost.
func (v *view) companyDealID(id api.LeadID) string {
	facts := v.dealFacts(id)
	for _, c := range facts {
		if c.openID != "" {
			return c.openID
		}
	}
	var best model.Push
	for _, c := range facts {
		for _, k := range c.dealRows {
			p := v.m.Pushes[k]
			if p.State == stateDone && p.VendorID != "" && !c.closed[p.VendorID] && !facts[0].closed[p.VendorID] &&
				p.UpdatedAt.After(best.UpdatedAt) {
				best = p
			}
		}
	}
	return best.VendorID
}

// pushState is where a lead's own push in a lane stands.
type pushState int

const (
	pushCallable pushState = iota // no rows yet, or steps left to call (never-called cancelled rows return to pending)
	pushComplete                  // every step done
	pushStuck                     // a step failed, or was cancelled after its call may have gone out
)

// state classifies the lead's own rows in a lane, and whether the push
// already started (a retry, free of the budget).
func (v *view) state(id api.LeadID, lane string) (pushState, bool) {
	n, done := 0, 0
	started, stuck := false, false
	for _, p := range v.rows(id) {
		if p.LaneID != lane {
			continue
		}
		n++
		started = started || !p.FirstStartedAt.IsZero()
		switch {
		case p.State == stateDone:
			done++
		case p.State == stateFailed, p.State == stateCancelled && (!p.CalledAt.IsZero() || p.IntentRun != ""):
			stuck = true
		}
	}
	switch {
	case stuck:
		return pushStuck, started
	case n > 0 && done == n:
		return pushComplete, started
	}
	return pushCallable, started
}

// pushedUnderMerged reports a done step in the lane under a lead merged into
// id: a done row on either id counts as done for the survivor.
func (v *view) pushedUnderMerged(id api.LeadID, lane string) bool {
	for _, f := range v.family(id) {
		if f == id {
			continue
		}
		for _, p := range v.rows(f) {
			if p.LaneID == lane && p.State == stateDone {
				return true
			}
		}
	}
	return false
}

// item is one push the run may make: a lead in a lane.
type item struct {
	lead  api.LeadID
	lane  rules.Lane
	free  bool // already started: a retry, free of the budget
	score float64
}

// route decides a live lead's pushes this run, before limits, lookups and
// pushes_enabled: each matching non-cold lane it passes and has not been
// pushed to, and its one cold lane (RFC 6.10, contracts section 8). It also
// returns why each matching lane was skipped.
func (v *view) route(id api.LeadID) ([]item, map[string]string) {
	skipped := map[string]string{}
	verdict := v.r.Result.Verdicts[id]
	score := verdict.AccountScore + verdict.ContactScore
	var out []item
	var cold []rules.Lane
	for _, name := range v.r.Result.Lanes[id] {
		l, ok := v.lanes[name]
		if !ok {
			continue
		}
		switch l.Kind {
		case kindNonCold:
			if why := v.laneCheck(id, l); why != "" {
				skipped[l.ID] = why
				continue
			}
			st, started := v.state(id, l.ID)
			switch {
			case st == pushComplete || v.pushedUnderMerged(id, l.ID):
				skipped[l.ID] = "already pushed"
			case st == pushStuck:
				skipped[l.ID] = "a step failed or was refused"
			default:
				out = append(out, item{lead: id, lane: l, free: started, score: score})
			}
		case kindCold:
			cold = append(cold, l)
		case kindExport:
			// Not pushed here (the export hook lists it), but a lead the
			// built-in checks block is explained like any skipped lane.
			if why := v.laneCheck(id, l); why != "" {
				skipped[l.ID] = why
			}
		}
	}
	if it, ok := v.coldRoute(id, cold, score, skipped); ok {
		out = append(out, it)
	}
	return out, skipped
}

// coldRoute picks the lead's one cold lane among its matching cold lanes
// (highest priority first). A lead with an open cold row stays in that lane;
// a lead whose cold push is held anywhere gets no other lane; an Apollo-held
// lead falls through Apollo sequence lanes to its next matching cold lane.
func (v *view) coldRoute(id api.LeadID, matching []rules.Lane, score float64, skipped map[string]string) (item, bool) {
	// A cold lane with an own pending row: the lead stays there. When the
	// lane now fails a check, its pending steps are cancelled; if none of
	// them may have gone out, the cold push is still free and the lead
	// falls through to its matching cold lanes.
	for _, l := range v.openColdLanes(id) {
		if why := v.laneCheck(id, l); why != "" {
			skipped[l.ID] = why
			if v.holdsIn(id, l.ID) {
				return item{}, false
			}
			continue
		}
		st, started := v.state(id, l.ID)
		if st == pushStuck || st == pushComplete {
			return item{}, false
		}
		return item{lead: id, lane: l, free: started, score: score}, true
	}
	for _, l := range matching {
		if _, done := skipped[l.ID]; done {
			continue
		}
		if why := v.laneCheck(id, l); why != "" {
			skipped[l.ID] = why
			continue
		}
		st, started := v.state(id, l.ID)
		switch st {
		case pushComplete:
			skipped[l.ID] = "already pushed"
			return item{}, false
		case pushStuck:
			skipped[l.ID] = "a step failed or was refused"
			return item{}, false
		}
		return item{lead: id, lane: l, free: started, score: score}, true
	}
	return item{}, false
}

// holdsIn reports an own row of the lead in the lane that holds its cold push.
func (v *view) holdsIn(id api.LeadID, lane string) bool {
	for _, p := range v.rows(id) {
		if p.LaneID == lane && v.holds(p) {
			return true
		}
	}
	return false
}

// openColdLanes are the cold lanes (still in the rubric) where the lead has
// an own pending row, the one holding its cold push first, then by priority.
func (v *view) openColdLanes(id api.LeadID) []rules.Lane {
	holds := map[string]bool{}
	var out []rules.Lane
	seen := map[string]bool{}
	for _, p := range v.rows(id) {
		l, ok := v.lanes[p.LaneID]
		if !ok || l.Kind != kindCold || p.State != statePending {
			continue
		}
		holds[l.ID] = holds[l.ID] || v.holds(p)
		if !seen[l.ID] {
			seen[l.ID] = true
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if holds[out[i].ID] != holds[out[j].ID] {
			return holds[out[i].ID]
		}
		return out[i].Priority > out[j].Priority
	})
	return out
}

// routeAll routes every scored lead: its pushes, its planned lane (the
// highest-priority lane it is pushed to or listed on) and why lanes it
// matched were skipped.
func (v *view) routeAll() ([]item, map[api.LeadID]string, map[api.LeadID][]string) {
	var items []item
	planned := map[api.LeadID]string{}
	reasons := map[api.LeadID][]string{}
	for _, ref := range v.r.Input.Leads {
		id := ref.ID
		its, skipped := v.route(id)
		items = append(items, its...)
		pushed := map[string]bool{}
		for _, it := range its {
			pushed[it.lane.ID] = true
		}
		explained := map[string]bool{}
		for _, name := range v.r.Result.Lanes[id] {
			l, ok := v.lanes[name]
			if !ok {
				continue
			}
			if planned[id] == "" && (pushed[name] || (l.Kind == kindExport && v.laneCheck(id, l) == "")) {
				planned[id] = name
			}
			if why, ok := skipped[name]; ok {
				explained[name] = true
				reasons[id] = append(reasons[id], fmt.Sprintf("lane %s skipped: %s", name, why))
			}
		}
		// A lane the lead no longer matches but has an open row in.
		for _, name := range sortedNames(skipped) {
			if !explained[name] {
				reasons[id] = append(reasons[id], fmt.Sprintf("lane %s skipped: %s", name, skipped[name]))
			}
		}
	}
	return items, planned, reasons
}

// order sorts pushes as the budget takes them (contracts section 8):
// non-cold first, then cold, each by lane priority (highest first), then
// score (highest first), then lead id. Within a company this also runs
// non-cold steps before cold ones.
func order(items []item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if (a.lane.Kind == kindCold) != (b.lane.Kind == kindCold) {
			return b.lane.Kind == kindCold
		}
		if a.lane.Priority != b.lane.Priority {
			return a.lane.Priority > b.lane.Priority
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.lead != b.lead {
			return a.lead < b.lead
		}
		return a.lane.ID < b.lane.ID
	})
}

// newPushes is how many new pushes the limits allow this run: the run limit,
// and what is left of the day's (a push counts on the day of its first
// first_started_at, in the rubric's timezone).
func (v *view) newPushes() int {
	lim := v.r.Rubric.Limits()
	loc := lim.Location
	if loc == nil {
		loc = time.UTC
	}
	day := v.r.Now().In(loc).Format(time.DateOnly)
	first := map[[2]string]time.Time{}
	for _, p := range v.m.Pushes {
		if p.FirstStartedAt.IsZero() || p.LaneKind == kindExport {
			continue
		}
		k := [2]string{string(p.LeadID), p.LaneID}
		if t, ok := first[k]; !ok || p.FirstStartedAt.Before(t) {
			first[k] = p.FirstStartedAt
		}
	}
	used := 0
	for _, t := range first {
		if t.In(loc).Format(time.DateOnly) == day {
			used++
		}
	}
	return max(0, min(lim.MaxPushesPerRun, lim.MaxPushesPerDay-used))
}

// budget keeps every retry and as many new pushes as allowed, in order.
func budget(items []item, allowed int) []item {
	var out []item
	n := 0
	for _, it := range items {
		if !it.free {
			if n >= allowed {
				continue
			}
			n++
		}
		out = append(out, it)
	}
	return out
}

// withMargin is the provisional count step 8 looks up: the allowed new
// pushes plus a 10% margin, rounded up.
func withMargin(n int) int { return n + int(math.Ceil(float64(n)*lookupMargin/100)) }

// Blocked reports a lead blocked on every lane, and why (contracts section
// 12.6): unsubscribed, blocked by Overrides (conflicting status rows, an
// unknown value, or a manual `blocked`), an unresolved duplicate, or a
// rubric conflict. A merged lead is judged as the lead it was merged into.
// S13 uses it for an export row's do_not_contact.
func Blocked(r *Run, id api.LeadID) (bool, string) {
	v := r.view()
	return v.blocked(v.live(id))
}

// MatchesLane reports whether the lead (or, for a merged lead, the lead it
// was merged into) may be routed to the lane: the lane's `when` held at
// scoring and the lead passes every check that would cancel its steps there
// (the built-in checks; the Apollo-held rule on an Apollo sequence lane; and
// on a cold lane the cold-lane statuses, the deal rule and the one cold
// push). Limits, lookups and pushes_enabled do not count. S13 uses it.
func MatchesLane(r *Run, id api.LeadID, laneID string) bool {
	v := r.view()
	l, ok := v.lanes[laneID]
	if !ok {
		return false
	}
	return v.laneCheck(v.live(id), l) == ""
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
