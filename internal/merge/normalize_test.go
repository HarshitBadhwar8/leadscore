package merge

import (
	"errors"
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
	n, err := Normalize(in("leads",
		"Work Email", "  Ada@ACME.io ",
		"LinkedIn Profile", "https://www.LinkedIn.com/in/Ada/",
		"Website", "https://www.Acme.io:443/about?x=1",
		"Job Title", " CTO ",
		"Primary AI coding tool", "Cursor",
		"???", "dropped",
		"Empty", "",
	), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"email":               "ada@acme.io",
		"linkedin_url":        "linkedin.com/in/ada",
		"company.domain":      "acme.io",
		"title":               "CTO",
		"primaryaicodingtool": "Cursor",
	}
	if len(n.Fields) != len(want) {
		t.Errorf("fields = %v, want %v", n.Fields, want)
	}
	for k, v := range want {
		if n.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, n.Fields[k], v)
		}
	}
	if n.RowID != "ada@acme.io" || n.SourceID != "leads" {
		t.Errorf("row id = %s/%s, want leads/ada@acme.io", n.SourceID, n.RowID)
	}
}

// Proof: two headers resolving to one field; the first in file order owns it,
// even when its cell is empty, so a column order change is the only thing that
// changes which column counts.
func TestTwoHeadersOneFieldFirstWins(t *testing.T) {
	n, _ := Normalize(in("s", "Email", "first@acme.io", "Work Email", "second@acme.io"), nil)
	if n.Fields["email"] != "first@acme.io" {
		t.Errorf("email = %q, want the first header's", n.Fields["email"])
	}
	n, _ = Normalize(in("s", "Title", "", "Job title", "CTO", "Email", "a@acme.io"), nil)
	if _, ok := n.Fields["title"]; ok {
		t.Errorf("title = %q: the first header owns the field even when empty", n.Fields["title"])
	}
}

func TestRubricAliasesWinOverBuiltins(t *testing.T) {
	r := in("s", "Email", "a@acme.io", "Stage", "Series B", "Headcount", "40")
	n, _ := Normalize(r, map[string]string{"stage": "deal_stage"})
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
		{"email first", in("s", "email", "A@acme.io", "linkedin", "linkedin.com/in/a"), "a@acme.io"},
		{"linkedin next", in("s", "linkedin", "https://linkedin.com/in/A/"), "linkedin.com/in/a"},
		{"domain and name", in("s", "domain", "acme.io", "name", "  Priya   R "), "acme.io|priya r"},
		{"receiver contact id first", in("receiver", "contact_id", "c-1", "email", "a@acme.io"), "c-1"},
		{"receiver without contact id", in("receiver", "email", "a@acme.io"), "a@acme.io"},
		{"contact id means nothing for other sources", in("s", "contact_id", "c-1", "email", "a@acme.io"), "a@acme.io"},
	}
	for _, tt := range tests {
		n, err := Normalize(tt.row, nil)
		if err != nil || n.RowID != tt.want {
			t.Errorf("%s: row id %q, %v; want %q", tt.name, n.RowID, err, tt.want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for name, r := range map[string]api.InputRow{
		"placeholder email":   in("s", "email", "{{contact.email}}", "linkedin", "linkedin.com/in/a"),
		"display-name form":   in("s", "email", "Ada <ada@acme.io>"),
		"no key":              in("s", "name", "Ada", "title", "CTO"),
		"name without domain": in("s", "name", "Ada"),
	} {
		n, err := Normalize(r, nil)
		if !errors.Is(err, ErrRejected) || n.Reject == "" {
			t.Errorf("%s: err %v, reject %q; want a reject", name, err, n.Reject)
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
	a, _ := Normalize(in("s", "email", "a@acme.io", "title", "CTO"), nil)
	b, _ := Normalize(in("s", "title", "CTO", "email", "a@acme.io"), nil)
	c, _ := Normalize(in("s", "email", "a@acme.io", "title", "CEO"), nil)
	d, _ := Normalize(in("s", "email", "a@acme.io", "title", "CTO"), map[string]string{"title": "job"})
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
		Email: " Ada@ACME.io ", LinkedInURL: "HTTPS://www.linkedin.com/in/Ada/", Domain: "http://WWW.Acme.IO:8080/pricing",
	})
	if e.Email != "ada@acme.io" || e.LinkedInURL != "linkedin.com/in/ada" || e.Domain != "acme.io" {
		t.Errorf("normalized = %q %q %q", e.Email, e.LinkedInURL, e.Domain)
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"acme.io":                     "acme.io",
		" ACME.IO. ":                  "acme.io",
		"www.acme.io":                 "acme.io",
		"https://www.acme.io/":        "acme.io",
		"http://acme.io:8080/a/b?c#d": "acme.io",
		"eng.acme.io":                 "eng.acme.io",
		"https://user@acme.io/path":   "acme.io",
		"":                            "",
	} {
		if got := NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePerson(t *testing.T) {
	for in, want := range map[string]string{
		" Ada@Acme.IO ":                          "ada@acme.io",
		"https://www.LinkedIn.com/in/Ada/":       "linkedin.com/in/ada",
		" 0190f4c2-aaaa-7000-8000-000000000001 ": "0190f4c2-aaaa-7000-8000-000000000001",
		"*":                                      "*",
	} {
		if got := NormalizePerson(in); got != want {
			t.Errorf("NormalizePerson(%q) = %q, want %q", in, got, want)
		}
	}
}
