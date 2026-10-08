// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// A dry run takes no lease, writes nothing, spends no credits, reads every row
// (no chunking), and prints one line per changed lead plus totals.
func TestDryRun(t *testing.T) {
	in := basicInstall(t)
	if _, _, err := in.run(DefaultHooks()); err != nil {
		t.Fatal(err)
	}
	people, ranked, health := len(in.rows(model.TablePeople)), in.rows(model.TableRanked), in.health()

	// Another run holds the lease; the dry run does not need it. Clerks become
	// tier 1, and three new leads arrive, more than one chunk.
	in.setLease("live-run", time.Now().Add(time.Hour))
	in.config(leadsCSV + "ingest_chunk_rows: 1\n")
	in.write("rubric.yml", strings.Replace(testRubric, "contains: head }\n      then: 1", "contains: clerk }\n      then: 1", 1))
	in.write("leads.csv", csvText("Email,Name,Title",
		"ana@acme.example,Ana A,Head of Ops", "bo@acme.example,Bo B,Clerk",
		"cy@acme.example,Cy C,Clerk", "di@acme.example,Di D,Clerk", "ed@acme.example,Ed E,Clerk"))
	enriched, prepushed, pushed := false, false, false
	hooks := DefaultHooks()
	hooks.Enrich = func(*Run) error { enriched = true; return nil }
	hooks.Push = func(*Run) error { pushed = true; return nil }
	// PrePush runs (it skips its lookups on a dry run) and its result is
	// what the report plans: here it holds Ed back from every lane.
	hooks.PrePush = func(r *Run, _ []api.LeadID) error {
		prepushed = r.DryRun
		for _, l := range r.Input.Leads {
			if l.Emails[0] == "ed@acme.example" {
				delete(r.Result.Lanes, l.ID)
			}
		}
		return nil
	}
	res, out, err := in.run(hooks, dry)
	if err != nil || res.Skipped {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if enriched || pushed || !prepushed {
		t.Errorf("a dry run calls PrePush but no enrichment or push: enriched %v prepushed %v pushed %v", enriched, prepushed, pushed)
	}
	for _, want := range []string{
		"dry run: no lease taken, nothing written",
		"tier 2 -> 1",
		"tier 1 -> 2",
		"totals: 5 lead(s) scored: 3 new, 2 changed, 0 unchanged; planned lanes: list 4, none 1",
		"dry run (healthy): 5 lead(s) would be scored from 3 input row(s), 0 left for later runs; nothing was saved or pushed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains("\n"+out, "\nrun ") || strings.Contains(out, "merged") {
		t.Errorf("a dry run's summary names no run id and nothing merged:\n%s", out)
	}
	if strings.Count(out, "lane=none") != 1 || strings.Count(out, "\nnew ") != 3 || strings.Contains(out, "@") {
		t.Errorf("one line per new lead, by id only:\n%s", out)
	}
	if len(in.rows(model.TablePeople)) != people || len(in.rows(model.TableRanked)) != len(ranked) || in.health()["result:run_id"] != health["result:run_id"] {
		t.Error("a dry run must write nothing")
	}
	if in.state("lease_owner") != "live-run" {
		t.Error("a dry run must not touch the lease")
	}
	t.Logf("dry-run output:\n%s", out)
}

// A dry run on a fresh install scores against an empty store and does not
// create the SQLite file.
func TestDryRunOnAFreshInstallCreatesNoStore(t *testing.T) {
	in := basicInstall(t)
	res, out, err := in.run(DefaultHooks(), dry)
	if err != nil || !strings.Contains(out, "no store yet") || !strings.Contains(out, "2 new") {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if _, err := os.Stat(filepath.Join(in.dir, "leadscore.db")); !os.IsNotExist(err) {
		t.Errorf("the dry run created the store: %v", err)
	}
}
