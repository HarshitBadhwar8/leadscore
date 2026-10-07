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

// An opt-out stored on a lead that a `same_as` merge then absorbs blocks the
// survivor: the fold reads Outcomes across the whole merge family.
func TestOptOutOnMergedAwayLeadBlocksSurvivor(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"ana.private@home.example,Ana Private,Head of Ops,home.example")
	w.pushesOff()
	alt := w.id("ana.private@home.example")
	w.edit(func(m *model.Model) {
		o := m.Outcomes[model.Key(alt)]
		o.LeadID, o.UnsubscribedAt, o.UnsubscribedOrigin = alt, time.Now().UTC(), events.UnsubEvent
		m.Put(model.TableOutcomes, o)
	})
	w.override("ana@acme.example", "same_as", "ana.private@home.example", "")
	w.mustRun()
	survivor := w.id("ana@acme.example")
	var merged api.Row
	for _, r := range w.rows(model.TablePeople) {
		if r["lead_id"] == string(alt) {
			merged = r
		}
	}
	if merged["merged_into"] != string(survivor) {
		t.Fatalf("the merge did not happen: %v", merged)
	}
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed {
		t.Errorf("survivor status %q, want unsubscribed", s)
	}
	if n := len(w.apollo.Calls()) + len(w.fake.Calls()); n != 0 {
		t.Errorf("the survivor of an opted-out lead was pushed: %v %v", calls(w.apollo), calls(w.fake))
	}
}

// In a hand-edited merged_into cycle an opt-out on any member counts, and
// every member is blocked as a duplicate anyway.
func TestOptOutInMergeCycle(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bea@beta.example,Bea B,Head of Ops,beta.example")
	w.pushesOff()
	a, b := w.id("ana@acme.example"), w.id("bea@beta.example")
	w.edit(func(m *model.Model) {
		pa, pb := m.People[model.Key(a)], m.People[model.Key(b)]
		pa.MergedInto, pb.MergedInto = b, a
		m.Put(model.TablePeople, pa)
		m.Put(model.TablePeople, pb)
		o := m.Outcomes[model.Key(b)]
		o.LeadID, o.UnsubscribedAt, o.UnsubscribedOrigin = b, time.Now().UTC(), events.UnsubLookup
		m.Put(model.TableOutcomes, o)
	})
	w.mustRun()
	live := min(a, b)
	var st string
	for _, r := range w.rows(model.TableOutcomes) {
		if r["lead_id"] == string(live) {
			st = r["status"]
		}
	}
	if st != statusUnsubscribed {
		t.Errorf("the cycle's live lead is %q, want unsubscribed (the opt-out is on a member)", st)
	}
	if len(w.apollo.Calls())+len(w.fake.Calls()) != 0 {
		t.Error("a merge-cycle lead was pushed")
	}
}

// An automated opt-out arriving after a manual one wins the origin, so a
// later `resubscribe` cannot undo it; a manual-only opt-out is undone once.
func TestResubscribeUndoesOnlyManualOptOuts(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Head of Ops,beta.example")
	w.pushesOff()
	w.edit(func(m *model.Model) {
		merge.SetStatus(m, "ana@acme.example", "unsubscribed", time.Now())
		merge.SetStatus(m, "bo@beta.example", "unsubscribed", time.Now())
	})
	w.config("pushes_enabled: true", "pushes_enabled: false")
	w.mustRun()
	for _, e := range []string{"ana@acme.example", "bo@beta.example"} {
		if o := w.outcome(e); o["unsubscribed_origin"] != unsubManual || o["status"] != statusUnsubscribed {
			t.Fatalf("%s after the manual row: %v", e, o)
		}
	}
	// Ana's opt-out is then confirmed by the Apollo lookup.
	w.event("ana@acme.example", api.Event{Kind: "optout", Email: "ana@acme.example", Origin: events.OriginApolloLookup})
	if o := w.outcome("ana@acme.example"); o["unsubscribed_origin"] != events.UnsubLookup {
		t.Fatalf("the automated opt-out takes the origin: %v", o)
	}
	w.edit(func(m *model.Model) {
		merge.SetStatus(m, "ana@acme.example", "resubscribe", time.Now())
		merge.SetStatus(m, "bo@beta.example", "resubscribe", time.Now())
	})
	w.config("pushes_enabled: false", "pushes_enabled: true")
	w.mustRun()
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed {
		t.Errorf("Ana's automated opt-out survived resubscribe? status %q", s)
	}
	if o := w.outcome("bo@beta.example"); o["status"] != statusNew || o["unsubscribed_at"] != "" {
		t.Errorf("Bo's manual opt-out is undone: %v", o)
	}
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"bo@beta.example"}) {
		t.Errorf("pushed %v, want only Bo", got)
	}
	if n := len(w.rows(model.TableAppliedOverrides)); n != 2 {
		t.Errorf("Applied overrides has %d rows, want the two resubscribe rows", n)
	}
	logs := 0
	for _, r := range w.rows(model.TableLog) {
		if r["kind"] == logResubscribe {
			logs++
			if strings.Contains(r["message"], "@") {
				t.Errorf("a log line carries an email: %q", r["message"])
			}
		}
	}
	if logs != 2 {
		t.Errorf("%d resubscribe log lines, want 2", logs)
	}
	// It applies once: a later automated-free run leaves things alone.
	w.mustRun()
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed {
		t.Errorf("Ana %q", s)
	}
}

// A `resubscribe` row waiting while a manual `unsubscribed` row still names
// the lead (a hand edit; the CLI never leaves both): the unsubscribed row
// wins, and the resubscribe is used up.
func TestUnsubscribedRowBeatsWaitingResubscribe(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.pushesOff()
	w.override("ana@acme.example", "status", "unsubscribed", "")
	w.mustRun()
	since := w.outcome("ana@acme.example")["unsubscribed_at"]
	w.override("ana@acme.example", "status", "resubscribe", "2026-10-07T00:00:00.000Z")
	w.mustRun()
	if o := w.outcome("ana@acme.example"); o["status"] != statusUnsubscribed || o["unsubscribed_at"] != since || since == "" {
		t.Fatalf("outcome %v; the opt-out keeps its date %q", o, since)
	}
	if len(w.rows(model.TableAppliedOverrides)) != 1 {
		t.Error("the resubscribe row applies once")
	}
	// Removing the unsubscribed row later does not release her: only a new
	// resubscribe clears the manual opt-out.
	w.edit(func(m *model.Model) {
		m.Delete(model.TableOverrides, []string{"ana@acme.example", "status", "unsubscribed", ""})
	})
	w.mustRun()
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed || len(w.apollo.Calls()) != 0 {
		t.Errorf("status %q, calls %v", s, calls(w.apollo))
	}
}

// Overrides persons are normalized like Identities: a capitalized email with
// spaces matches.
func TestCapitalizedOverridesEmailMatches(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.pushesOff()
	w.override("  Ana@ACME.Example ", "Status", "Unsubscribed", "")
	w.mustRun()
	if s := w.outcome("ana@acme.example")["status"]; s != statusUnsubscribed {
		t.Errorf("status %q, want unsubscribed", s)
	}
	if len(w.apollo.Calls())+len(w.fake.Calls()) != 0 {
		t.Error("a lead opted out in Overrides was pushed")
	}
}

// Removing a manual status row (other than unsubscribed) releases the lead.
func TestRemovingManualStatusReleases(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
	w.pushesOff()
	w.override("ana@acme.example", "status", "replied_negative", "")
	w.mustRun()
	if s := w.outcome("ana@acme.example")["status"]; s != "replied_negative" || len(w.apollo.Calls()) != 0 {
		t.Fatalf("status %q", s)
	}
	w.edit(func(m *model.Model) { merge.SetStatus(m, "ana@acme.example", "none", time.Now()) })
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ana@acme.example"}) {
		t.Errorf("released lead pushed: %v", got)
	}
}

// The status precedence on one lead: each rule beats the ones below it.
func TestStatusPrecedence(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		o    model.Outcome
		row  string // a manual status row
		want string
	}{
		{"nothing", model.Outcome{}, "", statusNew},
		{"contacted", model.Outcome{ContactedAt: now}, "", statusContacted},
		{"reply beats contacted", model.Outcome{ContactedAt: now, ReplyStatus: "replied_neutral", ReplyAt: now}, "", "replied_neutral"},
		{"deal beats reply", model.Outcome{ReplyStatus: "replied_positive", ReplyAt: now, DealID: "D", DealStage: "open"}, "", statusDeal},
		{"lost deal releases", model.Outcome{ReplyStatus: "replied_positive", ReplyAt: now, DealID: "D", DealStage: "lost"}, "", "replied_positive"},
		{"manual beats deal", model.Outcome{DealID: "D", DealStage: "won"}, "replied_negative", "replied_negative"},
		{"unsubscribed beats all", model.Outcome{UnsubscribedAt: now, UnsubscribedOrigin: "event", DealStage: "open", DealID: "D"}, "replied_positive", statusUnsubscribed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example")
			w.config("pushes_enabled: true", "pushes_enabled: false")
			w.mustRun()
			id := w.id("ana@acme.example")
			w.edit(func(m *model.Model) {
				o := c.o
				o.LeadID = id
				m.Put(model.TableOutcomes, o)
			})
			if c.row != "" {
				w.override("ana@acme.example", "status", c.row, "")
			}
			w.mustRun()
			if s := w.outcome("ana@acme.example")["status"]; s != c.want {
				t.Errorf("status %q, want %q", s, c.want)
			}
		})
	}
}
