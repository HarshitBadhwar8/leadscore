package rules

import (
	"encoding/csv"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// exampleInput reads examples/leads.csv into an Input the way merge will (S6):
// headers resolved with the rubric's aliases, then the built-in table, then the
// squashed header; one lead per row, first seen in row order; company facts
// from the built-in company columns, first non-empty value wins. It is a
// stand-in so the example can be hand-checked before merge exists.
func exampleInput(t *testing.T, r *Rubric) (Input, map[string]api.LeadID) {
	t.Helper()
	f, err := os.Open("../../examples/leads.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	builtin, own := api.BuiltinAliases(), r.Aliases()
	resolve := func(h string) string {
		sq := api.SquashHeader(h)
		if v, ok := own[sq]; ok {
			return v
		}
		if v, ok := builtin[sq]; ok {
			return v
		}
		return sq
	}
	in := Input{Companies: map[string]api.CompanyFacts{}, LeadsSeen: map[string]int{}}
	byEmail := map[string]api.LeadID{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, row := range rows[1:] {
		fields := map[string]string{}
		for j, h := range rows[0] {
			if name := resolve(h); fields[name] == "" {
				fields[name] = row[j]
			}
		}
		domain := fields["company.domain"]
		id := api.LeadID("lead-" + strconv.Itoa(i+1))
		in.Leads = append(in.Leads, api.LeadRef{
			ID: id, Emails: []string{fields["email"]}, FullName: fields["full_name"], Title: fields["title"],
			Domain: domain, Status: "new", Fields: fields, FirstSeenAt: start.Add(time.Duration(i) * time.Hour),
			SourcesSeen: 1,
		})
		byEmail[fields["email"]] = id
		in.LeadsSeen[domain]++
		c := in.Companies[domain]
		c.Domain = domain
		fill := func(dst *string, v string) {
			if *dst == "" {
				*dst = v
			}
		}
		fill(&c.Name, fields["company.name"])
		fill(&c.Region, fields["company.region"])
		fill(&c.FundingStage, fields["company.funding_stage"])
		if n, err := strconv.Atoi(fields["company.employees"]); err == nil && c.Employees == nil {
			c.Employees = &n
		}
		in.Companies[domain] = c
	}
	return in, byEmail
}

// The example ICP's results, worked out by hand from examples/rubric.yml and
// examples/leads.csv:
//
//   - Kran Logistik: Spreadsheets is a legacy tool, so fit yes; 420 people, in
//     the 50-5000 band; largest fleet 60 >= 20, so tier 1; Germany is a home
//     country, so priority A. Account: tier 1 40 + home 10 + three leads seen
//     10 + two "operations" titles 5 = 65.
//   - Anna (Head of Operations): +15 for "head"; contact 15, score 80.
//   - Jonas (Warehouse Manager, has a warm path): +10; contact 10.
//   - Van der Haven: Excel, fit yes; fleet 12 < 20, so tier 2; priority B.
//     Account 25 + home 10 = 35. Lea (Director): +15.
//   - Ironbridge: 30 people, tier 4; priority C; account 0. Tom (CFO): 0.
//   - Route Clair: no headcount, so tier has no value and needs enrichment;
//     Retail is not a target industry and no legacy tool, so no fit signal;
//     priority C. Marie (VP Supply Chain): +15.
//   - Solmar: Manhattan WMS is not legacy, but Food distribution is a target
//     industry, so fit yes; fleet 25, tier 1; Spain is not home, so priority B.
//     Account 40. Ines visited the demo page this week (demo_visit fired), so
//     hot: +20, and the hot-visitors lane outranks fleet-ops.
func TestExampleICP(t *testing.T) {
	src, err := os.ReadFile("../../examples/rubric.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	in, ids := exampleInput(t, r)
	in.Detectors.Leads = map[api.LeadID]map[string]bool{ids["ines.ruiz@solmar.example"]: {"demo_visit": true}}

	verdicts, blocked, warnings := r.Evaluate(in)
	if len(blocked) != 0 || len(warnings) != 0 {
		t.Fatalf("blocked %v, warnings %v", blocked, warnings)
	}
	lanes := r.MatchLanes(in)
	type want struct {
		fit, tier, priority any
		hot                 bool
		account, contact    float64
		lanes               []string
	}
	for email, w := range map[string]want{
		"anna.weber@kranlogistik.example":   {"yes", 1.0, "A", false, 65, 15, []string{"fleet-ops", "nurture"}},
		"jonas.brandt@kranlogistik.example": {"yes", 1.0, "A", false, 65, 10, []string{"fleet-ops", "nurture"}},
		"pia.schulz@kranlogistik.example":   {"yes", 1.0, "A", false, 65, 0, []string{"fleet-ops", "nurture"}},
		"lea.devries@vanderhaven.example":   {"yes", 2.0, "B", false, 35, 15, []string{"fleet-ops", "nurture"}},
		"tom.hale@ironbridge.example":       {"yes", 4.0, "C", false, 0, 0, []string{}},
		"marie.laurent@routeclair.example":  {nil, nil, "C", false, 0, 15, []string{}},
		"sam.ortiz@bigbox.example":          {"yes", 4.0, "C", false, 0, 0, []string{}},
		"ines.ruiz@solmar.example":          {"yes", 1.0, "B", true, 40, 20, []string{"hot-visitors", "fleet-ops", "nurture"}},
	} {
		id := ids[email]
		v := verdicts[id]
		got := want{v.Values["fit_signal"], v.Values["tier"], v.Values["priority"], v.Values["hot"] == true, v.AccountScore, v.ContactScore, lanes[id]}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %+v, want %+v\n%s", email, got, w, r.Explain(v))
		}
	}
	explained := r.Explain(verdicts[ids["anna.weber@kranlogistik.example"]])
	for _, line := range []string{
		"tier: 1",
		"score: 80 (account 65, contact 15)",
		"tier = 1 (rule 3: fit_signal = yes and company.largest_fleet >= 20)",
		"+10 account: company.leads_seen is 3 (band 3)",
		"+15 contact: title contains head or title contains director or title contains vp",
	} {
		if !strings.Contains(explained, line) {
			t.Errorf("Explain is missing %q:\n%s", line, explained)
		}
	}
}

// Every input field the example rubric reads is a column of the sample CSV,
// or one merge produces.
func TestExampleCSVCoversTheRubric(t *testing.T) {
	src, err := os.ReadFile("../../examples/rubric.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := exampleInput(t, r)
	have := map[string]bool{"sources_seen": true, "receiver_only": true, "company.leads_seen": true, "company.domain": true}
	for k := range in.Leads[0].Fields {
		have[k] = true
	}
	for _, f := range r.Fields() {
		if !have[f] {
			t.Errorf("the rubric reads %s, but no column of examples/leads.csv resolves to it", f)
		}
	}
}
