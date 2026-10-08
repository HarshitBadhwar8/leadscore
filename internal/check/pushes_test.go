// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func TestPushesCheck(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	m := model.New()
	put := func(lead, lane, state string, started time.Time) {
		m.Put(model.TablePushes, model.Push{LeadID: api.LeadID("lead-x" + lead), LaneID: lane, Step: "push", LaneKind: "cold",
			State: state, Attempts: 3, FirstStartedAt: started, UpdatedAt: started})
	}
	put("1", "a", "failed", now.Add(-time.Hour))
	put("2", "a", "pending", now.Add(-25*time.Hour))
	put("3", "a", "pending", now.Add(-48*time.Hour))
	put("4", "a", "pending", now.Add(-time.Hour)) // recent
	put("5", "a", "done", now.Add(-100*time.Hour))
	put("6", "a", "failed", now.Add(-time.Hour)) // a merged lead: left out
	m.Put(model.TablePeople, model.Person{LeadID: "lead-x6", MergedInto: "lead-x1"})

	got := pushesCheck{now: func() time.Time { return now }}.Run(context.Background(), Env{Model: m})
	if len(got) != 2 {
		t.Fatalf("problems %+v", got)
	}
	if got[0].Key != "push_failed:lead-x1:a:push" || !strings.Contains(got[0].Fix, "leadscore retry --lane a lead-x1") || got[0].Warning {
		t.Errorf("failed: %+v", got[0])
	}
	if got[1].Key != "push_pending" || !got[1].Warning || !strings.Contains(got[1].Message, "2 step(s)") ||
		!strings.Contains(got[1].Message, model.FormatTime(now.Add(-48*time.Hour))) {
		t.Errorf("pending: %+v", got[1])
	}
	if p := (pushesCheck{now: time.Now}).Run(context.Background(), Env{}); p != nil {
		t.Errorf("no model: %v", p)
	}
}

func TestStoreCheckLedgerShrank(t *testing.T) {
	m := model.New()
	m.Put(model.TablePushes, model.Push{LeadID: "a", LaneID: "l", Step: "s", State: "done"})
	for _, tt := range []struct {
		saved string
		want  bool
	}{{"", false}, {"1", false}, {"2", true}} {
		st := stateStore{}
		if tt.saved != "" {
			st["ledger_rows"] = tt.saved
		}
		got := storeCheck{}.Run(context.Background(), Env{Model: m, Store: st})
		has := false
		for _, p := range got {
			has = has || p.Key == "ledger_shrank"
		}
		if has != tt.want {
			t.Errorf("ledger_rows %q: problems %+v", tt.saved, got)
		}
	}
}
