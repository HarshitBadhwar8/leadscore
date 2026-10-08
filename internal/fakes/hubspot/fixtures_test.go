package hubspot

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every fixture's request, replayed against a fake seeded to match it,
// gets the fixture's status and an answer with the fixture's shape: every
// key the fixture has, with a value of the same JSON type. So the fake
// cannot drift from the fixtures, and when a real capture replaces a
// fixture the fake's differences show up here.
func TestFakeMatchesFixtures(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	contact := func(s *Server, email string, props map[string]string) { s.AddContact(email, props) }
	seeds := map[string]func(*Server){
		"contacts_search/found_by_lead_id": func(s *Server) {
			contact(s, "ana@example.com", map[string]string{"leadscore_lead_id": "01900000-0000-7000-8000-000000000001"})
		},
		"contacts_search/paged": func(s *Server) {
			contact(s, "ana@example.com", map[string]string{"leadscore_lead_id": "01900000-0000-7000-8000-000000000001", "hs_email_optout": "false"})
			contact(s, "ben@example.com", map[string]string{"leadscore_lead_id": "01900000-0000-7000-8000-000000000002", "hs_email_optout": "false"})
		},
		"contacts_create/conflict":                 func(s *Server) { contact(s, "ana@example.com", nil) },
		"contacts_batch_read/by_email_one_missing": func(s *Server) { contact(s, "ana@example.com", map[string]string{"hs_email_optout": "true"}) },
		"deals_search/by_company_domain": func(s *Server) {
			s.next = 200
			s.AddDeal(StageOpen, map[string]string{"leadscore_company_domain": "example.com"})
		},
		"deals_create/with_association": func(s *Server) { contact(s, "ana@example.com", nil) },
		"deals_batch_read/one_deleted": func(s *Server) {
			s.next = 200
			s.AddDeal(StageLost, nil)
		},
		"associations_batch_read/contacts_deals": func(s *Server) {
			contact(s, "ana@example.com", nil)
			contact(s, "ben@example.com", nil)
			s.next = 200
			d := s.AddDeal(StageOpen, nil)
			s.Associate("contacts", "101", "deals", d)
		},
		"associations_put/deal_contact": func(s *Server) {
			contact(s, "ana@example.com", nil)
			s.next = 200
			s.AddDeal(StageOpen, nil)
		},
		"companies_search/by_domain": func(s *Server) { s.next = 300; s.AddCompany("example.com") },
		"contacts_get/merged_id": func(s *Server) {
			old := s.AddContact("ana.old@example.com", nil)
			winner := s.AddContact("ana@example.com", map[string]string{"hs_email_optout": "true"})
			s.Merge(old, winner)
		},
		"associations_batch_read/contacts_companies": func(s *Server) {
			c := s.AddContact("ana@example.com", nil)
			s.next = 300
			s.Associate("contacts", c, "companies", s.AddCompany("example.com"))
		},
		"associations_batch_read/deals_companies": func(s *Server) {
			s.next = 200
			d := s.AddDeal(StageOpen, nil)
			s.next = 300
			s.Associate("deals", d, "companies", s.AddCompany("example.com"))
		},
		"companies_batch_read/by_id": func(s *Server) { s.next = 300; s.AddCompany("example.com") },
		"associations_batch_read/companies_deals": func(s *Server) {
			s.next = 200
			d := s.AddDeal(StageOpen, nil)
			s.next = 300
			s.Associate("companies", s.AddCompany("example.com"), "deals", d)
		},
	}
	bare := map[string]bool{"properties_create/created": true, "property_groups_create/created": true}
	skip := map[string]string{
		"errors/rate_limited": "served only through a fault, verbatim",
		"errors/server_error": "served only through a fault, verbatim",
	}
	for key, fx := range fixtures {
		t.Run(key, func(t *testing.T) {
			if why, ok := skip[key]; ok {
				t.Skip(why)
			}
			if !fx.Provisional {
				t.Errorf("%s: not marked provisional, but no real call was recorded for it", key)
			}
			s := New()
			if bare[key] {
				s = NewBare()
				if key == "properties_create/created" {
					s.groups["contacts"] = true
				}
			}
			if seed := seeds[key]; seed != nil {
				seed(s)
			}
			srv := httptest.NewServer(s)
			defer srv.Close()
			var body io.Reader
			if len(fx.RequestBody) > 0 && string(fx.RequestBody) != "null" {
				body = bytes.NewReader(fx.RequestBody)
			}
			url := srv.URL + fx.Path
			if fx.Query != "" {
				url += "?" + fx.Query
			}
			req, _ := http.NewRequest(fx.Method, url, body)
			token := Token
			if key == "errors/unauthorized" {
				token = "wrong"
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != fx.Status {
				t.Fatalf("status %d, fixture %d: %s", resp.StatusCode, fx.Status, got)
			}
			var want, have any
			if err := json.Unmarshal(fx.ResponseBody, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &have); err != nil {
				t.Fatalf("the fake's answer is not JSON: %s", got)
			}
			if path := sameShape(want, have, "$"); path != "" {
				t.Errorf("the fake's answer differs from the fixture at %s:\nfixture %s\nfake    %s", path, fx.ResponseBody, got)
			}
		})
	}
}

// sameShape reports the first path where have lacks a key of want, or holds
// a value of another JSON type ("" when the shapes match). A null in want
// matches anything; arrays compare their first elements.
func sameShape(want, have any, path string) string {
	switch w := want.(type) {
	case nil:
		return ""
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return path
		}
		for k, v := range w {
			hv, ok := h[k]
			if !ok {
				return path + "." + k
			}
			if p := sameShape(v, hv, path+"."+k); p != "" {
				return p
			}
		}
	case []any:
		h, ok := have.([]any)
		if !ok || len(w) > 0 && len(h) == 0 {
			return path
		}
		if len(w) > 0 {
			return sameShape(w[0], h[0], path+"[0]")
		}
	default:
		switch want.(type) {
		case string:
			if _, ok := have.(string); !ok {
				return path
			}
		case float64:
			if _, ok := have.(float64); !ok {
				return path
			}
		case bool:
			if _, ok := have.(bool); !ok {
				return path
			}
		}
	}
	return ""
}
