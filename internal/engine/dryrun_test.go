package engine

import (
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
	enriched, pushed := false, false
	hooks := DefaultHooks()
	hooks.Enrich = func(*Run) error { enriched = true; return nil }
	hooks.Push = func(*Run) error { pushed = true; return nil }
	hooks.PrePush = func(*Run, []api.LeadID) error { pushed = true; return nil }
	res, out, err := in.run(hooks, dry)
	if err != nil || res.Skipped {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if enriched || pushed {
		t.Error("a dry run calls no enrichment, lookup or push")
	}
	for _, want := range []string{
		"dry run: no lease taken, nothing written",
		"tier 2 -> 1",
		"tier 1 -> 2",
		"totals: 5 lead(s) scored: 3 new, 2 changed, 0 unchanged; planned lanes: list 5",
		"3 input row(s) merged, 0 left",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\nnew ") != 3 || strings.Contains(out, "@") {
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
