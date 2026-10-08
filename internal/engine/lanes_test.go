package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// reply records a positive reply for a lead, as an earlier Intake would.
func (w *world) reply(email, status string) {
	w.t.Helper()
	id := w.id(email)
	w.edit(func(m *model.Model) {
		o := m.Outcomes[model.Key(id)]
		o.LeadID, o.ReplyStatus, o.ReplyAt = id, status, time.Now().UTC()
		m.Put(model.TableOutcomes, o)
	})
}

// event applies an event to the store's model, as an earlier Intake would.
func (w *world) event(email string, e api.Event) {
	w.t.Helper()
	id := w.id(email)
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	w.edit(func(m *model.Model) { events.Apply(m, id, merge.NormalizeEventKeys(e), nil) })
}

// pushesOff runs once with pushes off, so the leads exist with no pushes.
func (w *world) pushesOff() {
	w.t.Helper()
	w.config("pushes_enabled: true", "pushes_enabled: false")
	w.mustRun()
	w.config("pushes_enabled: false", "pushes_enabled: true")
}

// A deal step called earlier in the run blocks a colleague's cold push at
// the same company, even in the same batch and even when the deal call timed
// out (the ledger rules); the next run's fold marks the colleague `deal`
// from the ledger; a lookup that finds the deal closed-lost releases the
// company, and the colleague's never-called cold steps return to pending.
func TestDealEarlierInRunBlocksColleaguesColdPush(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Clerk,acme.example",
		"ben@acme.example,Ben B,Head of IT,acme.example")
	w.pushesOff()
	w.reply("ana@acme.example", "replied_positive")
	w.hubspot.FailAfter("deal", 1) // the deal may exist: the call timed out after HubSpot acted
	res, out := w.mustRun()
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("Ben was cold-pushed while his company's deal step was in flight: %v\n%s", calls(w.apollo), out)
	}
	if got := calls(w.hubspot); !slices.Equal(got, []string{"ana@acme.example warm contact", "ana@acme.example warm deal"}) {
		t.Fatalf("hubspot calls %v", got)
	}
	if r := w.push("ben@acme.example", "seq-a", "contact"); r["state"] != stateCancelled || r["called_at"] != "" ||
		!strings.Contains(r["last_error"], "open or won deal") {
		t.Errorf("Ben's cold step: %v", r)
	}
	if res.Pushed != 0 {
		t.Errorf("pushed %d", res.Pushed)
	}

	// Run 2: the deal step is pending and called, so the company is `deal`.
	w.hubspot.Before(nil)
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusDeal {
		t.Errorf("Ben's status %q, want deal (rule 4 from the ledger)", s)
	}
	if len(w.apollo.Calls()) != 0 {
		t.Fatal("Ben was cold-pushed while his company holds a deal")
	}

	// Run 3, past the search-lag window: the HubSpot lookup finds the
	// company's deal closed-lost: the company is released, and Ben (now
	// looked up) may be pushed.
	w.clock = func() time.Time { return time.Now().Add(dealSearchLag + time.Minute) }
	w.lookup("hubspot").SetDeal("acme.example", w.push("ana@acme.example", "warm", "deal")["vendor_id"], "lost")
	w.mustRun()
	if s := w.outcome("ben@acme.example")["status"]; s != statusNew {
		t.Errorf("after closed-lost Ben's status is %q, want new", s)
	}
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ben@acme.example"}) {
		t.Errorf("after the release Ben is pushed once: %v", got)
	}
}

// A company with an open or won deal (from Outcomes) blocks cold lanes for
// every lead there, but not non-cold lanes.
func TestCompanyDealBlocksColdNotNonCold(t *testing.T) {
	for _, stage := range []string{"open", "won"} {
		t.Run(stage, func(t *testing.T) {
			w := newWorld(t,
				"ana@acme.example,Ana A,Head of Ops,acme.example",
				"ben@acme.example,Ben B,Head of IT,acme.example")
			w.pushesOff()
			w.edit(func(m *model.Model) {
				events.Apply(m, "", api.Event{Kind: "deal_" + stage, Domain: "acme.example", At: time.Now().UTC(),
					Attrs: map[string]string{"deal_id": "D1"}}, nil)
			})
			// The warm lane also takes deal-status leads here, to show a deal
			// does not block non-cold lanes.
			w.rubric("{ field: status, eq: replied_positive }", "{ field: status, in: [replied_positive, deal] }")
			w.mustRun()
			if len(w.apollo.Calls()) != 0 || len(w.fake.Calls()) != 0 {
				t.Fatalf("cold pushes at a company with a %s deal: %v %v", stage, calls(w.apollo), calls(w.fake))
			}
			if got := pushedTo(w, w.hubspot, "contact"); !slices.Equal(got, []string{"ana@acme.example", "ben@acme.example"}) {
				t.Errorf("the non-cold lane still runs: %v", got)
			}
			if s := w.outcome("ana@acme.example")["status"]; s != statusDeal {
				t.Errorf("Ana's status %q", s)
			}
		})
	}
}

// Every status that blocks cold lanes keeps a lead out of them; unsubscribed
// and blocked also keep it out of non-cold lanes; replied_* does not.
func TestStatusesBlockLanes(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(w *world)
		cold, warm  bool // pushed to a cold lane, to the warm lane
		wantStatus  string
		positiveRow bool // the lead also has a positive reply (so warm matches)
	}{
		{name: "new", setup: func(*world) {}, cold: true, wantStatus: statusNew},
		{name: "replied_negative", setup: func(w *world) { w.reply("ana@acme.example", "replied_negative") }, wantStatus: "replied_negative"},
		{name: "replied_neutral", setup: func(w *world) { w.reply("ana@acme.example", "replied_neutral") }, wantStatus: "replied_neutral"},
		{name: "replied_unlabelled", setup: func(w *world) { w.reply("ana@acme.example", "replied_unlabelled") }, wantStatus: "replied_unlabelled"},
		{name: "replied_positive", setup: func(w *world) { w.reply("ana@acme.example", "replied_positive") }, warm: true, wantStatus: "replied_positive"},
		{name: "unsubscribed event", setup: func(w *world) {
			w.reply("ana@acme.example", "replied_positive")
			w.event("ana@acme.example", api.Event{Kind: "unsubscribed", Email: "ana@acme.example", Origin: events.OriginReceiver})
		}, wantStatus: statusUnsubscribed},
		{name: "manual blocked", setup: func(w *world) {
			w.reply("ana@acme.example", "replied_positive")
			w.override("ana@acme.example", "status", "blocked", "")
		}, wantStatus: statusBlocked},
		{name: "conflicting rows", setup: func(w *world) {
			w.reply("ana@acme.example", "replied_positive")
			w.override("ana@acme.example", "status", "replied_negative", "")
			w.override("ana@acme.example", "status", "replied_neutral", "")
		}, wantStatus: statusBlocked},
		{name: "unknown value", setup: func(w *world) {
			w.reply("ana@acme.example", "replied_positive")
			w.override("ana@acme.example", "status", "unsubscibed", "") // a typo blocks every lane
		}, wantStatus: statusBlocked},
		{name: "manual replied_positive", setup: func(w *world) { w.override("ana@acme.example", "status", "replied_positive", "") },
			warm: true, wantStatus: "replied_positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
			w.pushesOff()
			c.setup(w)
			w.mustRun()
			if s := w.outcome("ana@acme.example")["status"]; s != c.wantStatus {
				t.Errorf("status %q, want %q", s, c.wantStatus)
			}
			if got := len(w.apollo.Calls()) > 0; got != c.cold {
				t.Errorf("cold push %v, want %v: %v", got, c.cold, calls(w.apollo))
			}
			if got := len(w.hubspot.Calls()) > 0; got != c.warm {
				t.Errorf("non-cold push %v, want %v: %v", got, c.warm, calls(w.hubspot))
			}
		})
	}
}

// The built-in checks on every lane: an unresolved namesake, a rubric
// conflict and a lead with no valid email (LinkedIn only) are never pushed;
// a LinkedIn-only lead may still be listed on an export lane.
func TestBuiltInChecks(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"ana2@acme.example,Ana A,Head of Ops,acme.example", // a namesake of Ana at the same company
		"cy@cyan.example,Cy C,Head of Sales,cyan.example")
	w.write("li.csv", csvText("LinkedIn,Name,Title,Domain", "https://www.linkedin.com/in/dee,Dee D,Head of HR,dee.example"))
	w.config("sources:\n", "sources:\n  - { id: li, type: csv, path: li.csv }\n")
	w.mustRun()
	if got := pushedTo(w, w.apollo, "contact"); !slices.Equal(got, []string{"cy@cyan.example"}) {
		t.Fatalf("only Cy passes the built-in checks: %v", calls(w.apollo))
	}
	for _, e := range []string{"ana@acme.example", "ana2@acme.example"} {
		if r := w.ranked(e); r["lane"] != "" || !strings.Contains(r["reasons"], "unresolved duplicate") {
			t.Errorf("namesake %s: %v", e, r)
		}
	}
	var dee api.Row
	for _, r := range w.rows(model.TableRanked) {
		if r["linkedin_url"] == "linkedin.com/in/dee" {
			dee = r
		}
	}
	if dee["lane"] != "list" || !strings.Contains(dee["reasons"], "lane seq-a skipped: no valid email") {
		t.Errorf("a LinkedIn-only lead is listed but never pushed: %v", dee)
	}
}

// A rubric `conflicts` flag blocks every lane.
func TestRubricConflictBlocksEveryLane(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.write("rubric.yml", strings.Replace(laneRubric, "lanes:", "conflicts:\n  - { field: title }\nlanes:", 1))
	w.write("more.csv", csvText("Email,Title", "ana@acme.example,Clerk"))
	w.config("sources:\n", "sources:\n  - { id: more, type: csv, path: more.csv }\n")
	w.mustRun()
	if len(w.apollo.Calls())+len(w.fake.Calls()) != 0 {
		t.Fatalf("a lead with a rubric conflict was pushed: %v %v", calls(w.apollo), calls(w.fake))
	}
}

// A lead Apollo already holds is never enrolled in an Apollo sequence: it
// falls through to its next matching cold lane.
func TestApolloHeldFallsThrough(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.pushesOff()
	w.edit(func(m *model.Model) {
		p := m.People[model.Key(w.id("ana@acme.example"))]
		p.ApolloHeldAt = time.Now().UTC()
		m.Put(model.TablePeople, p)
	})
	w.mustRun()
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("an Apollo-held lead was enrolled: %v", calls(w.apollo))
	}
	if got := calls(w.fake); !slices.Equal(got, []string{"ana@acme.example seq-b push"}) {
		t.Errorf("it falls through to seq-b: %v", got)
	}
	if r := w.ranked("ana@acme.example"); r["lane"] != "seq-b" || !strings.Contains(r["reasons"], "lane seq-a skipped: Apollo already holds this lead") {
		t.Errorf("Ranked %v", r)
	}
}

// A HubSpot deals lane needs a company domain.
func TestDealsLaneNeedsACompany(t *testing.T) {
	w := newWorld(t, "ana@gmail.example,Ana A,Clerk,")
	w.pushesOff()
	w.reply("ana@gmail.example", "replied_positive")
	// gmail.example derives a domain from the email; take it away.
	w.edit(func(m *model.Model) {
		p := m.People[model.Key(w.id("ana@gmail.example"))]
		delete(p.Fields, model.CompanyDomainField)
		m.Put(model.TablePeople, p)
	})
	w.mustRun()
	if len(w.hubspot.Calls()) != 0 {
		t.Fatalf("a lead with no company entered a deals lane: %v", calls(w.hubspot))
	}
	if r := w.ranked("ana@gmail.example"); !strings.Contains(r["reasons"], "lane warm skipped: no company domain") {
		t.Errorf("reasons %q", r["reasons"])
	}
}

// Pushes start disabled: nothing is pushed and no row is written, but
// Ranked shows each lead's planned lane.
func TestPushesDisabledPushNothing(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.config("pushes_enabled: true", "pushes_enabled: false")
	res, _ := w.mustRun()
	if len(w.apollo.Calls()) != 0 || len(w.rows(model.TablePushes)) != 0 || res.Pushed != 0 {
		t.Fatal("pushes_enabled: false pushed")
	}
	if l := w.ranked("ana@acme.example")["lane"]; l != "seq-a" {
		t.Errorf("planned lane %q", l)
	}
}

// A non-cold lane pushes a lead once; an export lane never counts as the
// cold push.
func TestNonColdOnceAndExportNeverCold(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.rubric(`push: "hubspot:deals"`, `push: "hubspot:contacts"`) // a deals lane would make the company a deal
	w.pushesOff()
	w.override("ana@acme.example", "status", "replied_positive", "")
	w.mustRun()
	w.mustRun()
	if n := len(w.hubspot.Calls()); n != 1 {
		t.Fatalf("the warm lane ran %d times, want once: %v", n, calls(w.hubspot))
	}
	// Remove the override: Ana is new again; her listing on the export lane
	// never held a cold push, so she gets one.
	w.edit(func(m *model.Model) { _, _ = merge.SetStatus(m, "ana@acme.example", "none", time.Now()) })
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ana@acme.example"}) {
		t.Errorf("cold pushes %v", got)
	}
}

// Blocked and MatchesLane, as the export hook calls them.
func TestBlockedAndMatchesLane(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.pushesOff()
	w.event("bo@beta.example", api.Event{Kind: "unsubscribed", Email: "bo@beta.example", Origin: events.OriginReceiver})
	var got []string
	hooks := DefaultHooks()
	hooks.Export = func(r *Run) error {
		for _, e := range []string{"ana@acme.example", "bo@beta.example"} {
			id := w.id(e)
			b, why := Blocked(r, id)
			got = append(got, e+" blocked="+boolText(b)+" "+why+
				" seq-a="+boolText(MatchesLane(r, id, "seq-a"))+
				" seq-b="+boolText(MatchesLane(r, id, "seq-b"))+
				" list="+boolText(MatchesLane(r, id, "list"))+
				" nope="+boolText(MatchesLane(r, id, "nope")))
		}
		return nil
	}
	if _, out, err := w.install.run(hooks); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Ana was cold-pushed to seq-a in this run: seq-a stays eligible (her own
	// lane) and seq-b is not (her one cold push is held). Bo opted out.
	want := []string{
		"ana@acme.example blocked=false  seq-a=true seq-b=false list=true nope=false",
		"bo@beta.example blocked=true unsubscribed seq-a=false seq-b=false list=false nope=false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// A lead blocked on every lane is explained on an export lane too: with only
// an export lane, an unsubscribed lead's reasons still say why it is not
// listed (a review found the export lane's skip missing).
func TestBlockedExportLaneIsExplained(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
	w.write("rubric.yml", `version: 1
lanes:
  - { id: list, kind: export, when: { field: receiver_only, eq: false }, push: "export:list" }
`)
	w.override("ana@acme.example", "status", "unsubscribed", "")
	w.mustRun()
	if r := w.ranked("ana@acme.example"); r["lane"] != "" || !strings.Contains(r["reasons"], "lane list skipped: ") || !strings.Contains(r["reasons"], "unsubscribed") {
		t.Errorf("Ana's Ranked row %v, want no lane and the export lane's skip explained", r)
	}
	if r := w.ranked("bo@beta.example"); r["lane"] != "list" || strings.Contains(r["reasons"], "skipped") {
		t.Errorf("Bo's Ranked row %v, want listed with nothing skipped", r)
	}
}
