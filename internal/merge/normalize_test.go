package merge

import (
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// in builds an input row from header, value pairs, headers in the order given.
func in(source string, pairs ...string) api.InputRow {
	r := api.InputRow{SourceID: source, Columns: map[string]string{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		r.Headers = append(r.Headers, pairs[i])
		r.Columns[pairs[i]] = pairs[i+1]
	}
	return r
}

func TestNormalizeResolvesHeaders(t *testing.T) {
	n := Normalize(in("leads",
		"Work Email", "  Ada@ACME.EXAMPLE ",
		"LinkedIn Profile", "https://www.LinkedIn.com/in/Ada/",
		"Website", "https://www.Acme.example:443/about?x=1",
		"Job Title", " CTO ",
		"Favourite Editor", "Vim",
		"???", "dropped",
		"Empty", "",
	), nil)
	if n.Reject != "" {
		t.Fatal(n.Reject)
	}
	want := map[string]string{
		"email":           "ada@acme.example",
		"linkedin_url":    "linkedin.com/in/ada",
		"company.domain":  "acme.example",
		"title":           "CTO",
		"favouriteeditor": "Vim",
	}
	if len(n.Fields) != len(want) {
		t.Errorf("fields = %v, want %v", n.Fields, want)
	}
	for k, v := range want {
		if n.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, n.Fields[k], v)
		}
	}
	if n.RowID != "ada@acme.example" || n.SourceID != "leads" {
		t.Errorf("row id = %s/%s, want leads/ada@acme.example", n.SourceID, n.RowID)
	}
}

// Proof: two headers resolving to one field; the first in file order owns it,
// even when its cell is empty, so a column order change is the only thing that
// changes which column counts.
func TestTwoHeadersOneFieldFirstWins(t *testing.T) {
	n := Normalize(in("s", "Email", "first@acme.example", "Work Email", "second@acme.example"), nil)
	if n.Fields["email"] != "first@acme.example" {
		t.Errorf("email = %q, want the first header's", n.Fields["email"])
	}
	n = Normalize(in("s", "Title", "", "Job title", "CTO", "Email", "a@acme.example"), nil)
	if _, ok := n.Fields["title"]; ok {
		t.Errorf("title = %q: the first header owns the field even when empty", n.Fields["title"])
	}
}

func TestRubricAliasesWinOverBuiltins(t *testing.T) {
	r := in("s", "Email", "a@acme.example", "Stage", "Series B", "Headcount", "40")
	n := Normalize(r, map[string]string{"stage": "deal_stage"})
	if n.Fields["deal_stage"] != "Series B" || n.Fields["company.funding_stage"] != "" {
		t.Errorf("fields = %v: the rubric's alias must win", n.Fields)
	}
	if n.Fields["company.employees"] != "40" {
		t.Errorf("built-in aliases still apply: %v", n.Fields)
	}
}

func TestRowIDs(t *testing.T) {
	tests := []struct {
		name string
		row  api.InputRow
		want string
	}{
		{"email first", in("s", "email", "A@acme.example", "linkedin", "linkedin.com/in/a"), "a@acme.example"},
		{"linkedin next", in("s", "linkedin", "https://linkedin.com/in/A/"), "linkedin.com/in/a"},
		{"domain and name", in("s", "domain", "acme.example", "name", "  Priya   R "), "acme.example|priya r"},
		{"receiver contact id first", in("receiver", "contact_id", "c-1", "email", "a@acme.example"), "c-1"},
		{"receiver without contact id", in("receiver", "email", "a@acme.example"), "a@acme.example"},
		{"contact id means nothing for other sources", in("s", "contact_id", "c-1", "email", "a@acme.example"), "a@acme.example"},
	}
	for _, tt := range tests {
		n := Normalize(tt.row, nil)
		if n.Reject != "" || n.RowID != tt.want {
			t.Errorf("%s: row id %q, %v; want %q", tt.name, n.RowID, n.Reject, tt.want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for name, r := range map[string]api.InputRow{
		"placeholder email":   in("s", "email", "{{contact.email}}", "linkedin", "linkedin.com/in/a"),
		"display-name form":   in("s", "email", "Ada <ada@acme.example>"),
		"no key":              in("s", "name", "Ada", "title", "CTO"),
		"name without domain": in("s", "name", "Ada"),
	} {
		n := Normalize(r, nil)
		if n.Reject == "" {
			t.Errorf("%s: want a reject", name)
		}
		if n.RowID == "" || n.RowHash == "" {
			t.Errorf("%s: a rejected row still needs a row id and hash to be recorded once", name)
		}
		if strings.Contains(n.Reject, "ada") || strings.Contains(n.Reject, "acme") {
			t.Errorf("%s: reject %q quotes a cell", name, n.Reject)
		}
	}
}

// Proof: the hash covers every raw cell and the alias table, so an edited row
// or an alias change re-applies the row; header order does not matter.
func TestRowHash(t *testing.T) {
	a := Normalize(in("s", "email", "a@acme.example", "title", "CTO"), nil)
	b := Normalize(in("s", "title", "CTO", "email", "a@acme.example"), nil)
	c := Normalize(in("s", "email", "a@acme.example", "title", "CEO"), nil)
	d := Normalize(in("s", "email", "a@acme.example", "title", "CTO"), map[string]string{"title": "job"})
	if a.RowHash != b.RowHash {
		t.Error("header order changed the hash")
	}
	if a.RowHash == c.RowHash {
		t.Error("an edited cell did not change the hash")
	}
	if a.RowHash == d.RowHash {
		t.Error("an alias change did not change the hash")
	}
}

func TestNormalizeEventKeys(t *testing.T) {
	e := NormalizeEventKeys(api.Event{
		Email: " Ada@ACME.EXAMPLE ", LinkedInURL: "HTTPS://www.linkedin.com/in/Ada/", Domain: "http://WWW.Acme.EXAMPLE:8080/pricing",
	})
	if e.Email != "ada@acme.example" || e.LinkedInURL != "linkedin.com/in/ada" || e.Domain != "acme.example" {
		t.Errorf("normalized = %q %q %q", e.Email, e.LinkedInURL, e.Domain)
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"acme.example":                     "acme.example",
		" ACME.EXAMPLE. ":                  "acme.example",
		"www.acme.example":                 "acme.example",
		"https://www.acme.example/":        "acme.example",
		"http://acme.example:8080/a/b?c#d": "acme.example",
		"eng.acme.example":                 "eng.acme.example",
		"https://user@acme.example/path":   "acme.example",
		"":                                 "",
	} {
		if got := NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePerson(t *testing.T) {
	for in, want := range map[string]string{
		" Ada@Acme.example ":                     "ada@acme.example",
		"https://www.LinkedIn.com/in/Ada/":       "linkedin.com/in/ada",
		" 0190f4c2-aaaa-7000-8000-000000000001 ": "0190f4c2-aaaa-7000-8000-000000000001",
		"*":                                      "*",
	} {
		if got := NormalizePerson(in); got != want {
			t.Errorf("NormalizePerson(%q) = %q, want %q", in, got, want)
		}
	}
}
