// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package api

// builtinAliases maps a squashed header spelling to the field it names:
// common header spellings of the built-in fields, plus three company facts.
// It names built-in fields only; a team's own fields get their spellings from
// the rubric's `aliases`.
var builtinAliases = map[string]string{
	"email":        "email",
	"emailaddress": "email",
	"workemail":    "email",

	"fullname": "full_name",
	"name":     "full_name",

	"company":     "company.name",
	"companyname": "company.name",
	"account":     "company.name",

	"companydomain": "company.domain",
	"domain":        "company.domain",
	"website":       "company.domain",

	"employees":         "company.employees",
	"headcount":         "company.employees",
	"companysize":       "company.employees",
	"numberofemployees": "company.employees",

	"fundingstage": "company.funding_stage",
	"funding":      "company.funding_stage",
	"stage":        "company.funding_stage",

	"country": "company.region",
	"region":  "company.region",

	"title":    "title",
	"jobtitle": "title",
	"role":     "title",

	"linkedinurl":     "linkedin_url",
	"linkedin":        "linkedin_url",
	"linkedinprofile": "linkedin_url",

	"segment":      "segment",
	"segmentlabel": "segment",

	"triggersignalnote": "trigger_note",
	"triggernote":       "trigger_note",
	"triggerevent":      "trigger_note",

	"bestpathin": "warm_path",
	"bestpath":   "warm_path",
	"pathin":     "warm_path",

	"contactid":       "contact_id",
	"apollocontactid": "contact_id",
	"personid":        "contact_id",

	// Event rows only: when the event happened.
	"visitedat":   "at",
	"visitdate":   "at",
	"lastvisited": "at",
	"lastvisitat": "at",
}

// BuiltinAliases returns a copy of the built-in alias table: squashed header
// spelling to field name. A copy, so no caller can change what another sees.
func BuiltinAliases() map[string]string {
	out := make(map[string]string, len(builtinAliases))
	for k, v := range builtinAliases {
		out[k] = v
	}
	return out
}

// SquashHeader reduces a header to lowercase a-z and 0-9, dropping everything
// else, so "Work Email", "work_email" and "WORK-EMAIL" all become "workemail".
// Non-ASCII letters are dropped too: the alias table is ASCII.
func SquashHeader(h string) string {
	b := make([]byte, 0, len(h))
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		}
	}
	return string(b)
}
