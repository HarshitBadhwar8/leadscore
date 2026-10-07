package api

import (
	"reflect"
	"testing"
)

func TestSquashHeader(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Work Email", "workemail"},
		{"work_email", "workemail"},
		{"WORK-EMAIL", "workemail"},
		{"  E-mail  ", "email"},
		{"Favourite Editor", "favouriteeditor"},
		{"Number of Employees (2024)", "numberofemployees2024"},
		{"Ünïcode Fïeld", "ncodefeld"},
		{"", ""},
		{"___", ""},
	}
	for _, tt := range tests {
		if got := SquashHeader(tt.in); got != tt.want {
			t.Errorf("SquashHeader(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The table must be exactly as documented: a missing spelling silently
// loses a whole column of data, and an extra one can steal a column a rubric
// meant to read under its own name.
func TestBuiltinAliasesMatchContract(t *testing.T) {
	contract := map[string][]string{
		"email":                 {"email", "emailaddress", "workemail"},
		"full_name":             {"fullname", "name"},
		"company.name":          {"company", "companyname", "account"},
		"company.domain":        {"companydomain", "domain", "website"},
		"company.employees":     {"employees", "headcount", "companysize", "numberofemployees"},
		"company.funding_stage": {"fundingstage", "funding", "stage"},
		"company.region":        {"country", "region"},
		"title":                 {"title", "jobtitle", "role"},
		"linkedin_url":          {"linkedinurl", "linkedin", "linkedinprofile"},
		"segment":               {"segment", "segmentlabel"},
		"trigger_note":          {"triggersignalnote", "triggernote", "triggerevent"},
		"warm_path":             {"bestpathin", "bestpath", "pathin"},
		"contact_id":            {"contactid", "apollocontactid", "personid"},
		"at":                    {"visitedat", "visitdate", "lastvisited", "lastvisitat"},
	}
	want := map[string]string{}
	for field, spellings := range contract {
		for _, s := range spellings {
			if _, dup := want[s]; dup {
				t.Fatalf("contract lists spelling %q twice", s)
			}
			want[s] = field
		}
	}
	if got := BuiltinAliases(); !reflect.DeepEqual(got, want) {
		t.Errorf("BuiltinAliases() = %v, want %v", got, want)
	}
}

func TestBuiltinAliasSpellingsAreSquashed(t *testing.T) {
	for spelling := range BuiltinAliases() {
		if SquashHeader(spelling) != spelling {
			t.Errorf("spelling %q is not in squashed form; it could never match", spelling)
		}
	}
}

func TestBuiltinAliasesReturnsACopy(t *testing.T) {
	a := BuiltinAliases()
	a["email"] = "changed"
	if BuiltinAliases()["email"] != "email" {
		t.Fatal("mutating the returned map changed the built-in table")
	}
}
