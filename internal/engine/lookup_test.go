// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func clerks(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("p%02d@p%02d.example,P%02d,Clerk,p%02d.example", i, i, i, i))
	}
	return out
}

// An opt-out the pre-push lookup finds blocks the push, is folded as
// unsubscribed (origin lookup), and the lead is scored again: Ranked shows
// no lane.
func TestLookupOptOutBlocksThePush(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.lookup("apollo").OptOut("ANA@acme.example")
	w.mustRun()
	if len(w.apollo.Calls()) != 0 {
		t.Fatalf("an opted-out lead was pushed: %v", calls(w.apollo))
	}
	if got := calls(w.fake); !slices.Equal(got, []string{"bo@beta.example seq-b push"}) {
		t.Errorf("fake %v", got)
	}
	o := w.outcome("ana@acme.example")
	if o["status"] != statusUnsubscribed || o["unsubscribed_origin"] != "lookup" || o["unsubscribed_at"] == "" {
		t.Errorf("outcome %v", o)
	}
	if r := w.ranked("ana@acme.example"); r["lane"] != "" || r["status"] != statusUnsubscribed {
		t.Errorf("Ranked after the re-score: %v", r)
	}
}

// A lead whose lookup failed waits: it is not pushed, its rows are not
// cancelled, and the next run pushes it.
func TestFailedLookupWaits(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"bo@beta.example,Bo B,Clerk,beta.example")
	w.pushesOff()
	hs := w.lookup("hubspot")
	hs.FailLookup(w.id("ana@acme.example"), errors.New("timeout"))
	res, _ := w.mustRun()
	if len(w.apollo.Calls()) != 0 || len(w.fake.Calls()) != 1 {
		t.Fatalf("apollo %v fake %v", calls(w.apollo), calls(w.fake))
	}
	if !hasKey(res.Problems, "lookup_failed:hubspot") {
		t.Errorf("problems %v", res.Problems)
	}
	hs.FailLookup(w.id("ana@acme.example"), nil)
	w.lookups["hubspot"] = nil
	delete(w.lookups, "hubspot")
	w.mustRun()
	if got := pushedTo(w, w.apollo, "enroll"); !slices.Equal(got, []string{"ana@acme.example"}) {
		t.Errorf("next run %v", got)
	}
}

// A lookup that fails as a whole blocks every lead it was for, and makes the
// run unhealthy.
func TestWholeLookupFailureBlocksAll(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example", "bo@beta.example,Bo B,Clerk,beta.example")
	w.lookup("hubspot").FailAllLookups(errors.New("503 from bob@x.example"))
	res, _ := w.mustRun()
	if len(w.apollo.Calls())+len(w.fake.Calls()) != 0 {
		t.Fatal("pushed with a failed lookup")
	}
	if !hasKey(res.Problems, "lookup_failed:hubspot") || res.Healthy {
		t.Errorf("problems %v healthy %v", res.Problems, res.Healthy)
	}
	for _, r := range w.rows(model.TableHealth) {
		if strings.Contains(r["value"], "bob@x.example") {
			t.Errorf("an email reached Health: %q", r["value"])
		}
	}
}

// Step 8 looks up the allowed new pushes plus a 10% margin, rounded up; only
// looked-up leads push, so a lead removed past the margin is not replaced.
func TestLookupMargin(t *testing.T) {
	w := newWorld(t, clerks(25)...)
	w.limits("{ max_pushes_per_run: 10 }")
	lk := w.lookup("hubspot")
	w.mustRun()
	if n := len(lk.LookedIDs()); n != 11 {
		t.Fatalf("looked up %d leads, want 11 (10 plus 10%%)", n)
	}
	if n := len(w.fake.Calls()); n != 10 {
		t.Errorf("pushed %d", n)
	}

	w2 := newWorld(t, clerks(4)...)
	w2.limits("{ max_pushes_per_run: 2 }")
	lk2 := w2.lookup("hubspot")
	w2.pushesOff()
	lk2.OptOut("p00@p00.example")
	lk2.OptOut("p01@p01.example")
	w2.mustRun()
	if got := pushedTo(w2, w2.fake, "push"); !slices.Equal(got, []string{"p02@p02.example"}) {
		t.Errorf("pushed %v: only the one looked-up lead left (p03 was past the margin)", got)
	}
}

// With pushes off no candidate is looked up, but one lead per company with an
// export row still goes to the deal lookup; a dry run looks up nothing.
func TestLookupsWithPushesOffAndDryRun(t *testing.T) {
	w := newWorld(t, "ana@acme.example,Ana A,Head of Ops,acme.example", "ben@acme.example,Ben B,Clerk,acme.example")
	w.config("pushes_enabled: true", "pushes_enabled: false")
	lk := w.lookup("hubspot")
	w.edit(func(m *model.Model) {
		m.Put(model.ExportTable("list"), model.ExportRow{LeadID: "x", Email: "x@acme.example"})
	})
	w.mustRun()
	// The export row names no live lead's domain, so nothing is looked up yet.
	if n := len(lk.Looked()); n != 0 {
		t.Fatalf("looked up %v", lk.Looked())
	}
	ana := w.id("ana@acme.example")
	w.edit(func(m *model.Model) {
		m.Put(model.ExportTable("list"), model.ExportRow{LeadID: ana, Email: "ana@acme.example"})
	})
	w.mustRun()
	looked := lk.LookedIDs()
	if len(looked) != 1 || looked[0] != min(ana, w.id("ben@acme.example")) {
		t.Errorf("looked up %v, want one lead at acme", looked)
	}
	before := len(lk.Looked())
	w.config("pushes_enabled: false", "pushes_enabled: true")
	w.mustRun(dry)
	if len(lk.Looked()) != before {
		t.Error("a dry run called a lookup")
	}
}
