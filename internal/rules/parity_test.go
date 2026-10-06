package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// The parity directory (RFC 8.3) holds a team's private rubric and the
// verdicts its previous scoring system gave, kept outside the repo:
//
//	rubric.yml          the rubric
//	parity.json         {"rows": [{id, inputs, account_pass, lead_pass}]}
//	conformance/*.yml   [{name, input, expect}]
//	divergences.yml     [{name, what, effect, rows, fields}] (optional)
//
// Inputs are keyed by rubric field names. account_pass and lead_pass, and a
// case's expect, map verdict fields to values: account_score and
// contact_score are the score halves; any other key is a derived name.
// Values compare as JSON, and null means no value.

// A private rubric kept outside the repo, in LEADSCORE_PARITY_DIR, must
// compile.
func TestPrivateRubricCompiles(t *testing.T) {
	dir := parityDir(t)
	r := parityRubric(t, dir)
	for _, w := range r.Warnings() {
		t.Logf("warning: %v", w)
	}
	t.Logf("compiled, version %s", r.Version())
}

// TestParityWithCore runs the private rubric over the exported verdicts and the
// conformance cases and compares every field. A mismatch passes only when
// divergences.yml lists its row and field, and every listed row must still
// mismatch on one of its fields, so the list cannot go stale.
func TestParityWithCore(t *testing.T) {
	dir := parityDir(t)
	r := parityRubric(t, dir)
	divs := parityDivergences(t, dir)

	// Expected mismatches: row -> field -> divergence name.
	allowed := map[string]map[string]string{}
	for _, d := range divs {
		for _, row := range d.Rows {
			if allowed[row] == nil {
				allowed[row] = map[string]string{}
			}
			for _, f := range d.Fields {
				allowed[row][f] = d.Name
			}
		}
	}
	hit := map[string]bool{}   // rows that mismatched where a divergence allows it
	known := map[string]bool{} // every row id and case name
	compare := func(id string, want map[string]any, got api.Verdict) {
		known[id] = true
		keys := make([]string, 0, len(want))
		for k := range want {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var g any
			switch k {
			case "account_score":
				g = got.AccountScore
			case "contact_score":
				g = got.ContactScore
			default:
				v, ok := got.Values[k]
				if !ok {
					t.Errorf("%s: the rubric has no derived name %q", id, k)
					continue
				}
				g = v
			}
			gs, ws := jsonValue(t, g), jsonValue(t, want[k])
			if gs == ws {
				continue
			}
			if name, ok := allowed[id][k]; ok {
				hit[id] = true
				t.Logf("%s: %s = %s, expected %s (divergence %s)", id, k, gs, ws, name)
				continue
			}
			t.Errorf("%s: %s = %s, want %s", id, k, gs, ws)
		}
	}

	// parity.json: one evaluation over every row, so leads sharing a company
	// domain are one company, oldest first in file order.
	rows := parityRows(t, dir)
	in, err := buildInput(rowsInputs(rows))
	if err != nil {
		t.Fatalf("parity.json: %v", err)
	}
	res := r.Evaluate(in)
	for _, w := range res.Warnings {
		t.Logf("parity.json warning: %s", w)
	}
	for _, row := range rows {
		if known[row.ID] {
			t.Fatalf("parity.json: row id %s appears twice", row.ID)
		}
		want := map[string]any{}
		for k, v := range row.AccountPass {
			want[k] = v
		}
		for k, v := range row.LeadPass {
			want[k] = v
		}
		compare(row.ID, want, res.Verdicts[api.LeadID(row.ID)])
	}

	// Conformance cases: each one lead at its own company.
	files, err := filepath.Glob(filepath.Join(dir, "conformance", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var cases int
	for _, f := range files {
		for _, c := range conformanceCases(t, f) {
			cases++
			if known[c.Name] {
				t.Fatalf("%s: case name %q is used twice", filepath.Base(f), c.Name)
			}
			if _, ok := c.Input["company.domain"]; !ok {
				c.Input["company.domain"] = fmt.Sprintf("case-%d.invalid", cases)
			}
			in, err := buildInput([]leadInput{{id: c.Name, fields: c.Input}})
			if err != nil {
				t.Fatalf("%s: %s: %v", filepath.Base(f), c.Name, err)
			}
			res := r.Evaluate(in)
			for _, w := range res.Warnings {
				t.Logf("%s warning: %s", c.Name, w)
			}
			compare(c.Name, c.Expect, res.Verdicts[api.LeadID(c.Name)])
		}
	}

	var names []string
	for _, d := range divs {
		names = append(names, d.Name)
		for _, row := range d.Rows {
			switch {
			case !known[row]:
				t.Errorf("divergence %s names %q, which is neither a parity row nor a case", d.Name, row)
			case !hit[row]:
				t.Errorf("divergence %s: %s now matches on %v; remove it from divergences.yml", d.Name, row, d.Fields)
			}
		}
	}
	t.Logf("compared %d parity rows and %d conformance cases; divergences: %s",
		len(rows), cases, strings.Join(names, ", "))
}

func parityDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("LEADSCORE_PARITY_DIR")
	if dir == "" {
		t.Skip("LEADSCORE_PARITY_DIR is not set")
	}
	return dir
}

func parityRubric(t *testing.T, dir string) *Rubric {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(dir, "rubric.yml"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compile(src)
	if err != nil {
		t.Fatalf("rubric.yml:\n%v", err)
	}
	return r
}

type parityRow struct {
	ID          string         `json:"id"`
	Inputs      map[string]any `json:"inputs"`
	AccountPass map[string]any `json:"account_pass"`
	LeadPass    map[string]any `json:"lead_pass"`
}

func parityRows(t *testing.T, dir string) []parityRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Rows []parityRow `json:"rows"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil {
		t.Fatalf("parity.json: %v", err)
	}
	if len(file.Rows) == 0 {
		t.Fatal("parity.json has no rows; the comparison would pass vacuously")
	}
	return file.Rows
}

type conformanceCase struct {
	Name   string         `yaml:"name"`
	Input  map[string]any `yaml:"input"`
	Expect map[string]any `yaml:"expect"`
}

func conformanceCases(t *testing.T, path string) []conformanceCase {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []conformanceCase
	if err := yaml.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	for i := range cases {
		if cases[i].Name == "" || len(cases[i].Expect) == 0 {
			t.Fatalf("%s: case %d needs a name and an expect", filepath.Base(path), i+1)
		}
		if cases[i].Input == nil {
			cases[i].Input = map[string]any{}
		}
	}
	return cases
}

type divergence struct {
	Name   string   `yaml:"name"`
	What   string   `yaml:"what"`
	Effect string   `yaml:"effect"`
	Rows   []string `yaml:"rows"`
	Fields []string `yaml:"fields"`
}

func parityDivergences(t *testing.T, dir string) []divergence {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "divergences.yml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var divs []divergence
	if err := yaml.Unmarshal(raw, &divs); err != nil {
		t.Fatalf("divergences.yml: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range divs {
		if d.Name == "" || strings.TrimSpace(d.What) == "" || strings.TrimSpace(d.Effect) == "" {
			t.Fatalf("divergences.yml: every entry needs a name, what and effect (%+v)", d)
		}
		if seen[d.Name] {
			t.Fatalf("divergences.yml: %s is listed twice", d.Name)
		}
		seen[d.Name] = true
		if len(d.Rows) > 0 && len(d.Fields) == 0 {
			t.Fatalf("divergences.yml: %s lists rows but no fields", d.Name)
		}
	}
	return divs
}

// leadInput is one lead's inputs, keyed by rubric field name.
type leadInput struct {
	id     string
	fields map[string]any
}

func rowsInputs(rows []parityRow) []leadInput {
	out := make([]leadInput, len(rows))
	for i, r := range rows {
		out[i] = leadInput{id: r.ID, fields: r.Inputs}
	}
	return out
}

// buildInput turns leads' inputs into an Input as merge would leave it: leads
// first seen in the order given, company facts by domain (they must agree
// across a company's leads), and company.leads_seen as given or, when no lead
// gives it, the number of leads at the domain.
func buildInput(leads []leadInput) (Input, error) {
	in := Input{Companies: map[string]api.CompanyFacts{}, LeadsSeen: map[string]int{}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	companyVals := map[string]map[string]string{} // domain -> key -> value, to check they agree
	counts := map[string]int{}
	for i, li := range leads {
		l := api.LeadRef{ID: api.LeadID(li.id), FirstSeenAt: start.Add(time.Duration(i) * time.Minute), Fields: map[string]string{}}
		if d, ok := li.fields["company.domain"]; ok && d != nil {
			l.Domain = fmt.Sprint(d)
		}
		facts := api.CompanyFacts{Domain: l.Domain}
		cv := map[string]string{}
		keys := make([]string, 0, len(li.fields))
		for k := range li.fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := li.fields[k]
			if v == nil {
				continue // null is no value
			}
			s := fmt.Sprint(v)
			if strings.HasPrefix(k, "company.") {
				cv[k] = s
			}
			var err error
			switch k {
			case "company.domain":
			case "company.name":
				facts.Name = s
			case "company.region":
				facts.Region = s
			case "company.funding_stage":
				facts.FundingStage = s
			case "company.employees":
				var n int
				n, err = strconv.Atoi(s)
				facts.Employees = &n
			case "company.leads_seen":
				var n int
				n, err = strconv.Atoi(s)
				in.LeadsSeen[l.Domain] = n
			case "sources_seen":
				l.SourcesSeen, err = strconv.Atoi(s)
			case "receiver_only":
				l.ReceiverOnly, err = strconv.ParseBool(s)
			case "status":
				l.Status = s
			case "email":
				l.Emails = []string{s}
			case "linkedin_url":
				l.LinkedInURLs = []string{s}
			case "full_name":
				l.FullName = s
			case "title":
				l.Title = s
			default:
				if strings.HasPrefix(k, "company.") {
					if facts.Extra == nil {
						facts.Extra = map[string]string{}
					}
					facts.Extra[k] = s
				} else {
					l.Fields[k] = s
				}
			}
			if err != nil {
				return Input{}, fmt.Errorf("%s: %s: %v", li.id, k, err)
			}
		}
		if l.Domain != "" {
			counts[l.Domain]++
			if prev, ok := companyVals[l.Domain]; ok {
				if !sameStrings(prev, cv) {
					return Input{}, fmt.Errorf("%s: company facts differ from an earlier lead at %s", li.id, l.Domain)
				}
			} else {
				companyVals[l.Domain] = cv
				in.Companies[l.Domain] = facts
			}
		}
		in.Leads = append(in.Leads, l)
	}
	for d, n := range counts {
		if _, ok := in.LeadsSeen[d]; !ok {
			in.LeadsSeen[d] = n
		}
	}
	return in, nil
}

func sameStrings(a, b map[string]string) bool {
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

// jsonValue renders v as normalized JSON, so 40, 40.0 and a json.Number "40"
// compare equal and a nil value is null.
func jsonValue(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(x)
	return string(b)
}
