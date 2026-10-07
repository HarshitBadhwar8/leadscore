package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

var stores = []string{"sqlite", "sheets"}

func at(t time.Time) string { return t.Format(time.RFC3339) }

// The main scenario's visits (expected.md, "Clock and events"): e1 to e4
// before run 1, e5 between the runs.
var (
	visitsBeforeRun1 = [][4]string{
		{"visit_demo", at(t1.Add(-48 * time.Hour)), anna, "kranlogistik.example"}, // e1: demo_visit fires
		{"visit_demo", at(t1.Add(-240 * time.Hour)), lea, "vanderhaven.example"},  // e2: first demo visit over 7d ago
		{"visit_pricing", at(t1.Add(-480 * time.Hour)), "", "routeclair.example"}, // e3: outside the 14d window
		{"visit_pricing", at(t1.Add(-48 * time.Hour)), "", "routeclair.example"},  // e4
	}
	visitBetweenRuns = [4]string{"visit_pricing", at(t1.Add(30 * time.Minute)), "", "routeclair.example"} // e5
)

// The example ICP's full output, vendor variant: Apollo and HubSpot sinks,
// replies through the receiver, enrichment. Ranked after each run, the
// pushes, Apollo's and HubSpot's records, the nurture export (a tab on
// Sheets), and the detector firings.
func TestExampleICPWithVendors(t *testing.T) {
	for _, kind := range stores {
		t.Run(kind, func(t *testing.T) {
			in := newInstall(t, kind, vendorV)
			in.addVisits(t0, visitsBeforeRun1...)
			in.receive(t0, replyBody("replied_positive", tom)) // e6

			res := in.run(t1)
			if res.Pushed != 6 {
				t.Errorf("run 1 pushed %d, want 6 (Tom, Anna, Jonas, Pia, Lea, Ines)", res.Pushed)
			}
			in.checkRanked("run 1", rankedV1)
			in.checkPushes(pushesV)

			in.addVisits(t1.Add(30*time.Minute), visitBetweenRuns)
			res = in.run(t2)
			if res.Pushed != 0 {
				t.Errorf("run 2 pushed %d, want 0", res.Pushed)
			}
			in.checkRanked("run 2", rankedV2)
			in.checkPushes(pushesV) // nothing new, nothing changed state
			in.checkExport("run 2", exportV)
			in.checkApollo()
			in.checkHubSpot()
			in.checkDetectorLog()
			in.checkPricingWindow()
			if in.kind == "sheets" {
				in.checkExportTab()
			}
		})
	}
}

// The example ICP's full output, CSV-only: the export-lane variant of the
// rubric, visits from an events source, no vendors.
func TestExampleICPCSVOnly(t *testing.T) {
	for _, kind := range stores {
		t.Run(kind, func(t *testing.T) {
			in := newInstall(t, kind, csvOnlyC)
			in.addVisits(t0, visitsBeforeRun1...)
			if res := in.run(t1); res.Pushed != 0 {
				t.Errorf("run 1 pushed %d", res.Pushed)
			}
			in.checkRanked("run 1", rankedC)
			in.addVisits(t1.Add(30*time.Minute), visitBetweenRuns)
			in.run(t2)
			in.checkRanked("run 2", rankedC)
			in.checkExport("run 2", exportC)
			in.checkPricingWindow()
			if n := len(in.rows(model.TablePushes)); n != 0 {
				t.Errorf("%d ledger rows in a CSV-only install", n)
			}
			if in.kind == "sheets" {
				in.checkExportTab()
			}
		})
	}
}

// Opt-outs, vendor variant: by an Apollo `unsubscribed` webhook (Ines), by
// Overrides (Lea), and before the person's row exists (Nora: the event in
// run 1, her CSV row in run 2, no push in run 2).
func TestOptOutsWithVendors(t *testing.T) {
	for _, kind := range stores {
		t.Run(kind, func(t *testing.T) {
			in := newInstall(t, kind, vendorV)
			in.receive(t0, replyBody("unsubscribed", ines), replyBody("unsubscribed", nora))
			in.setStatus(lea, "unsubscribed")

			if res := in.run(t1); res.Pushed != 3 {
				t.Errorf("run 1 pushed %d, want 3 (Anna, Jonas, Pia)", res.Pushed)
			}
			in.addLead(noraRow)
			if res := in.run(t2); res.Pushed != 0 {
				t.Errorf("run 2 pushed %d, want 0 (Nora opted out before her row existed)", res.Pushed)
			}

			got := map[string]string{}
			for _, r := range in.pushes() {
				if r["state"] != "done" {
					t.Errorf("ledger row %v not done", r)
				}
				got[r["email"]] = r["lane_id"]
			}
			if !mapsEqual(got, optOutPushesV) {
				t.Errorf("pushed %v, want %v", got, optOutPushesV)
			}
			for _, e := range []string{lea, ines, nora} {
				if _, ok := in.apollo.ContactByEmail(e); ok {
					t.Errorf("Apollo holds a contact for %s, who opted out", e)
				}
			}
			ranked := map[string]map[string]string{}
			for _, r := range in.rows(model.TableRanked) {
				ranked[r["email"]] = r
			}
			for e, origin := range optOutOrigins {
				o := in.outcome(e)
				if o["status"] != "unsubscribed" || o["unsubscribed_origin"] != origin {
					t.Errorf("%s outcome %v, want unsubscribed by %s", e, o, origin)
				}
				if r := ranked[e]; r["status"] != "unsubscribed" || r["lane"] != "" {
					t.Errorf("%s Ranked status %q lane %q, want unsubscribed and no lane", e, r["status"], r["lane"])
				}
			}
			if r := ranked[nora]; r["full_name"] != "Nora Klein" {
				t.Errorf("Nora's row did not merge into her receiver lead: %v", r)
			}
			in.checkExport("run 2", optOutExportV)
		})
	}
}

// Opt-outs, CSV-only: Overrides for a known person (Lea) and for one whose
// row arrives only in run 2 (Nora) keep both off the export list.
func TestOptOutsCSVOnly(t *testing.T) {
	for _, kind := range stores {
		t.Run(kind, func(t *testing.T) {
			in := newInstall(t, kind, csvOnlyC)
			in.setStatus(lea, "unsubscribed")
			in.setStatus(nora, "unsubscribed")
			res := in.run(t1)
			if !hasPrefix(res.Problems, "override_unmatched:") {
				t.Errorf("run 1 problems %v, want override_unmatched for Nora's waiting row", res.Problems)
			}
			in.addLead(noraRow)
			res = in.run(t2)
			if hasPrefix(res.Problems, "override_unmatched:") {
				t.Errorf("run 2 problems %v: Nora's row should have matched", res.Problems)
			}
			for _, e := range []string{lea, nora} {
				if o := in.outcome(e); o["status"] != "unsubscribed" || o["unsubscribed_origin"] != "manual" {
					t.Errorf("%s outcome %v, want unsubscribed by manual", e, o)
				}
			}
			in.checkExport("run 2", optOutExportC)
		})
	}
}

// Polling: replies read from Apollo, not the receiver. A polled
// willing_to_meet puts Tom on the deals lane, a polled unsubscribe blocks
// Ines, and a receiver replied_positive for Sam is ignored.
func TestPolling(t *testing.T) {
	in := newInstall(t, "sqlite", variant{vendor: true, polling: true})
	in.apollo.AddReply(fakeapollo.Reply{MessageID: "m-tom", Email: tom, Label: "willing_to_meet",
		SentAt: t1.Add(-48 * time.Hour), RepliedAt: t1.Add(-24 * time.Hour)})
	in.apollo.AddReply(fakeapollo.Reply{MessageID: "m-ines", Email: ines, Label: "unsubscribe",
		SentAt: t1.Add(-72 * time.Hour), RepliedAt: t1.Add(-24 * time.Hour)})
	in.receive(t0, replyBody("replied_positive", sam))

	in.run(t1)
	got := map[string]string{}
	for _, r := range in.pushes() {
		if r["state"] != "done" {
			t.Errorf("ledger row %v not done", r)
		}
		got[r["email"]] = r["lane_id"]
	}
	if !mapsEqual(got, pollingPushes) {
		t.Errorf("pushed %v, want %v", got, pollingPushes)
	}
	for e, status := range pollingStatuses {
		if o := in.outcome(e); o["status"] != status {
			t.Errorf("%s outcome %v, want %s", e, o, status)
		}
	}
	if o := in.outcome(sam); o["reply_status"] != "" {
		t.Errorf("Sam's receiver reply counted in polling mode: %v", o)
	}
	if n := len(in.hub.IDs("deals")); n != 1 {
		t.Errorf("%d HubSpot deals, want Tom's one", n)
	}
}

// The CSV-only rubric is the example with only its export lane: everything
// before the lanes comment is the example's text exactly.
func TestCSVOnlyRubricMatchesTheExample(t *testing.T) {
	ex, err := os.ReadFile(filepath.Join("..", "..", "examples", "rubric.yml"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := os.ReadFile(filepath.Join("testdata", "rubric_csv_only.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const cut = "# Where leads go."
	i := strings.Index(string(ex), cut)
	if i < 0 || !strings.HasPrefix(string(v), string(ex)[:i]) {
		t.Error("testdata/rubric_csv_only.yml no longer starts with examples/rubric.yml up to its lanes; copy the example's new text in")
	}
}

// checkPushes compares the ledger with the hand-derived rows: every row
// done, called, with a vendor id.
func (in *install) checkPushes(want []pushed) {
	in.t.Helper()
	rows := in.pushes()
	if len(rows) != len(want) {
		in.t.Errorf("%d ledger rows, want %d: %v", len(rows), len(want), rows)
	}
	got := map[pushed]bool{}
	for _, r := range rows {
		got[pushed{r["email"], r["lane_id"], r["step"], r["lane_kind"], r["dest"]}] = true
		if r["state"] != "done" || r["vendor_id"] == "" || r["called_at"] == "" {
			in.t.Errorf("ledger row %v: want done, called, with a vendor id", r)
		}
	}
	for _, w := range want {
		if !got[w] {
			in.t.Errorf("no ledger row %+v", w)
		}
	}
}

// checkApollo: one contact per cold push, each enrolled in its lane's
// sequence, and nobody else.
func (in *install) checkApollo() {
	in.t.Helper()
	if n := in.apollo.Count("contact"); n != len(sequencesV) {
		in.t.Errorf("Apollo created %d contacts, want %d", n, len(sequencesV))
	}
	if n := in.apollo.Count("enroll"); n != len(sequencesV) {
		in.t.Errorf("Apollo made %d enrollments, want %d", n, len(sequencesV))
	}
	ids := map[string]string{"seq-demo": "Demo visitors", "seq-fleet": "Fleet ops intro"}
	for e, seq := range sequencesV {
		c, ok := in.apollo.ContactByEmail(e)
		if !ok {
			in.t.Errorf("no Apollo contact for %s", e)
			continue
		}
		var seqs []string
		for id := range c.Sequences {
			seqs = append(seqs, ids[id])
		}
		if len(seqs) != 1 || seqs[0] != seq {
			in.t.Errorf("%s is in sequences %v, want [%s]", e, seqs, seq)
		}
	}
	for _, e := range []string{tom, marie, sam} {
		if _, ok := in.apollo.ContactByEmail(e); ok {
			in.t.Errorf("Apollo holds a contact for %s", e)
		}
	}
}

// checkHubSpot: Tom's contact, created with the run-1 verdict, and one deal
// at Ironbridge associated with it.
func (in *install) checkHubSpot() {
	in.t.Helper()
	contacts, deals := in.hub.IDs("contacts"), in.hub.IDs("deals")
	if len(contacts) != 1 || len(deals) != 1 {
		in.t.Fatalf("HubSpot holds contacts %v and deals %v, want one of each", contacts, deals)
	}
	if e := in.hub.Prop("contacts", contacts[0], "email"); e != tom {
		in.t.Errorf("the HubSpot contact is %q, want Tom", e)
	}
	for k, v := range tomContactV {
		if got := in.hub.Prop("contacts", contacts[0], k); got != v {
			in.t.Errorf("Tom's contact %s = %q, want %q", k, got, v)
		}
	}
	d := deals[0]
	if n, dom := in.hub.Prop("deals", d, "dealname"), in.hub.Prop("deals", d, "leadscore_company_domain"); n != "ironbridge.example" || dom != "ironbridge.example" {
		in.t.Errorf("the deal is %q with domain %q, want ironbridge.example", n, dom)
	}
	if st := in.hub.Prop("deals", d, "dealstage"); st != fakehub.StageOpen {
		in.t.Errorf("the deal's stage %q, want %s", st, fakehub.StageOpen)
	}
	if got := in.hub.Associated("deals", d, "contacts"); len(got) != 1 || got[0] != contacts[0] {
		in.t.Errorf("the deal's contacts %v, want Tom's", got)
	}
}

// checkDetectorLog: pricing_visits fired for Route Clair in run 2, moving
// Marie from tier 3 C to 2 B; no other lead's tier or priority moved, and
// run 1 logged none (no earlier Ranked).
func (in *install) checkDetectorLog() {
	in.t.Helper()
	id := in.leadIDs()[marie]
	kinds := map[string]int{}
	for _, r := range in.rows(model.TableLog) {
		if r["kind"] != "tier_change" && r["kind"] != "priority_change" {
			continue
		}
		if r["lead_id"] != id {
			in.t.Errorf("%s for a lead other than Marie: %v", r["kind"], r)
		}
		kinds[r["kind"]]++
		want := map[string]string{"tier_change": "-> 2", "priority_change": "-> B"}[r["kind"]]
		if !strings.HasSuffix(r["message"], want) {
			in.t.Errorf("%s message %q, want it to end %q", r["kind"], r["message"], want)
		}
	}
	if kinds["tier_change"] != 1 || kinds["priority_change"] != 1 {
		in.t.Errorf("change lines %v, want one tier and one priority change", kinds)
	}
}

// checkPricingWindow: the three pricing visits of Route Clair are kept as
// company window events (the detector's input), and two demo visits.
func (in *install) checkPricingWindow() {
	in.t.Helper()
	n := map[string]int{}
	for _, r := range in.rows(model.TableWindowEvents) {
		n[r["kind"]+"|"+r["domain"]]++
	}
	if n["visit_pricing|routeclair.example"] != 3 {
		in.t.Errorf("window events %v, want three visit_pricing at routeclair.example", n)
	}
	if n["visit_demo|kranlogistik.example"] != 1 || n["visit_demo|vanderhaven.example"] != 1 {
		in.t.Errorf("window events %v, want one demo visit each for Anna and Lea", n)
	}
}

// checkExportTab: on Sheets the export is a tab of the team's spreadsheet.
func (in *install) checkExportTab() {
	in.t.Helper()
	for _, sh := range in.fs.Spreadsheet(in.sheetID).Sheets {
		if sh.Properties.Title == model.ExportTable("nurture") {
			return
		}
	}
	in.t.Error("no Export nurture tab in the spreadsheet")
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func hasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
