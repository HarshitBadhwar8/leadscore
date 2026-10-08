// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

func (w *world) limits(l string) { w.write("rubric.yml", laneRubric+"limits: "+l+"\n") }

// The run limit counts new pushes only, taken in budget order (lane priority,
// then score); a retry of an earlier push is free.
func TestRunLimitAndFreeRetries(t *testing.T) {
	w := newWorld(t,
		"bo@beta.example,Bo B,Clerk,beta.example",
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"cy@cyan.example,Cy C,Clerk,cyan.example")
	w.limits("{ max_pushes_per_run: 1 }")
	w.apollo.Fail("enroll", sinktest.Transient)
	w.mustRun()
	if got := calls(w.apollo); !slices.Equal(got, []string{"ana@acme.example seq-a contact", "ana@acme.example seq-a enroll"}) {
		t.Fatalf("the highest lane goes first: %v", got)
	}
	if len(w.fake.Calls()) != 0 {
		t.Fatal("the run limit was passed")
	}
	// Run 2: Ana's retry is free; one new push (Bo, the lower id at equal score).
	res, _ := w.mustRun()
	if res.Pushed != 2 || len(w.fake.Calls()) != 1 {
		t.Fatalf("pushed %d, fake %v", res.Pushed, calls(w.fake))
	}
}

// Non-cold pushes claim the budget before cold ones.
func TestNonColdClaimsTheBudgetFirst(t *testing.T) {
	w := newWorld(t,
		"ana@acme.example,Ana A,Head of Ops,acme.example",
		"pat@pine.example,Pat P,Clerk,pine.example")
	w.limits("{ max_pushes_per_run: 1 }")
	w.pushesOff()
	w.reply("pat@pine.example", "replied_positive")
	w.mustRun()
	if len(w.apollo.Calls()) != 0 || len(w.hubspot.Calls()) != 2 {
		t.Errorf("apollo %v, hubspot %v", calls(w.apollo), calls(w.hubspot))
	}
}

// The day limit counts pushes by the day of their first first_started_at,
// in the rubric's timezone.
func TestDayLimit(t *testing.T) {
	var leads []string
	for i := 0; i < 3; i++ {
		leads = append(leads, fmt.Sprintf("p%d@p%d.example,P%d,Clerk,p%d.example", i, i, i, i))
	}
	w := newWorld(t, leads...)
	w.limits("{ max_pushes_per_day: 2, timezone: Asia/Kolkata }")
	day := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) // 17:30 in Kolkata
	w.clock = func() time.Time { return day }
	w.mustRun()
	if n := len(w.fake.Calls()); n != 2 {
		t.Fatalf("day 1 pushed %d, want 2", n)
	}
	day = day.Add(5 * time.Hour) // 22:30 in Kolkata: the same day
	w.mustRun()
	if n := len(w.fake.Calls()); n != 2 {
		t.Fatalf("the same day pushed again: %d", n)
	}
	day = day.Add(2 * time.Hour) // 00:30 the next day in Kolkata (still 2026-10-07 UTC)
	w.mustRun()
	if n := len(w.fake.Calls()); n != 3 {
		t.Errorf("the next day in the rubric's timezone pushed %d in all, want 3", n)
	}
}
