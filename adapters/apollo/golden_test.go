package apollo_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// goldenDir holds one stored body per file (testdata/events/README.md). They
// are provisional: built from earlier working code's test bodies until real
// captured calls replace them.
const goldenDir = "../../testdata/events"

var goldenReceived = time.Date(2026, 8, 23, 7, 0, 0, 0, time.UTC)

type golden struct {
	events []api.Event
	rows   []api.InputRow
	reject string // a substring of the error, when the body is refused
}

func row(cols ...string) api.InputRow {
	r := api.InputRow{SourceID: "receiver", Columns: map[string]string{}}
	for i := 0; i+1 < len(cols); i += 2 {
		r.Headers = append(r.Headers, cols[i])
		r.Columns[cols[i]] = cols[i+1]
	}
	return r
}

var goldens = map[string]golden{
	"apollo_reply_sent.json": {
		events: []api.Event{{Kind: "sent", Email: "dana.reyes@example.com", LinkedInURL: "https://linkedin.example/in/dana-reyes-example/",
			Domain: "example.com", At: goldenReceived, ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"stage": "Approaching", "full_name": "Dana Reyes", "title": "VP Engineering"}}},
		rows: []api.InputRow{row("email", "dana.reyes@example.com",
			"linkedin_url", "https://linkedin.example/in/dana-reyes-example/", "full_name", "Dana Reyes",
			"title", "VP Engineering", "company.domain", "example.com")},
	},
	"apollo_reply_replied.json": {
		events: []api.Event{{Kind: "replied", Email: "dana.reyes@example.com", Domain: "example.com", At: goldenReceived,
			ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"stage": "Replied", "conversation_link": "https://app.apollo.io/#/conv/9"}}},
		rows: []api.InputRow{row("email", "dana.reyes@example.com", "company.domain", "example.com")},
	},
	"apollo_reply_replied_positive.json": {
		events: []api.Event{{Kind: "replied_positive", Email: "dana.reyes@example.com", At: goldenReceived,
			ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"stage": "Interested", "conversation_link": "https://app.apollo.io/#/conv/9"}}},
		rows: []api.InputRow{row("email", "dana.reyes@example.com")},
	},
	"apollo_reply_unsubscribed.json": {
		events: []api.Event{{Kind: "unsubscribed", Email: "sam.ortiz@example.org", At: goldenReceived,
			ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"stage": "Do Not Contact", "full_name": "Sam Ortiz"}}},
		rows: []api.InputRow{row("email", "sam.ortiz@example.org", "full_name", "Sam Ortiz")},
	},
	"apollo_reply_opened.json":         {reject: "not one we act on"}, // ignored, and logged as such
	"apollo_reply_reserved_stage.json": {reject: "reserved"},
	"apollo_visit_identified.json": {
		events: []api.Event{{Kind: "visit_pricing", Email: "lee.park@example.net", LinkedInURL: "https://linkedin.example/in/lee-park-example/",
			Domain: "example.net", At: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"contact_id": "ct-2002", "full_name": "Lee Park", "title": "Head of Platform", "company": "Example Net", "visited_at": "2026-08-20T10:00:00.000Z"}}},
		rows: []api.InputRow{row("contact_id", "ct-2002", "email", "lee.park@example.net",
			"linkedin_url", "https://linkedin.example/in/lee-park-example/", "full_name", "Lee Park",
			"title", "Head of Platform", "company.name", "Example Net", "company.domain", "example.net")},
	},
	"apollo_visit_company_only.json": {
		events: []api.Event{{Kind: "visit_pricing", Domain: "example.org", At: time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC),
			ReceivedAt: goldenReceived, Origin: "receiver", Attrs: map[string]string{"company": "Example Org", "visited_at": "2026-08-21T09:30:00.000Z"}}},
	},
	"apollo_visit_no_visited_at.json": {
		events: []api.Event{{Kind: "visit_docs", Email: "lee.park@example.net", At: goldenReceived, ReceivedAt: goldenReceived,
			Origin: "receiver", Attrs: map[string]string{"full_name": "Lee", "no_visited_at": "yes"}}},
		rows: []api.InputRow{row("email", "lee.park@example.net", "full_name", "Lee")},
	},
	"apollo_visit_linkedin_only.json": {
		events: []api.Event{{Kind: "visit_pricing", LinkedInURL: "https://linkedin.example/in/ada-example/", Domain: "example.com",
			At: time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC), ReceivedAt: goldenReceived, Origin: "receiver",
			Attrs: map[string]string{"full_name": "Ada", "visited_at": "2026-08-22T08:00:00.000Z"}}},
		rows: []api.InputRow{row("linkedin_url", "https://linkedin.example/in/ada-example/", "full_name", "Ada", "company.domain", "example.com")},
	},
	"apollo_visit_no_identity.json": {reject: "neither a contact nor a company"},
}

// Every golden body parses to exactly the expected events and receiver rows,
// and every body in the folder has an expectation.
func TestGoldenBodies(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden bodies: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			want, ok := goldens[name]
			if !ok {
				t.Fatal("no expectation for this body; add one to goldens")
			}
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var probe map[string]any
			if json.Unmarshal(body, &probe) != nil || probe["provisional"] != true {
				t.Error(`a body not from a real capture must carry "provisional": true`)
			}
			kind := apollo.KindReply
			if strings.HasPrefix(name, "apollo_visit_") {
				kind = apollo.KindVisit
			}
			evs, rows, err := apollo.ParseRaw(api.RawEvent{Seq: "1", Kind: kind, ReceivedAt: goldenReceived, Body: body})
			if want.reject != "" {
				if err == nil || !strings.Contains(err.Error(), want.reject) {
					t.Fatalf("err = %v, want a reject naming %q", err, want.reject)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != len(want.events) || len(rows) != len(want.rows) {
				t.Fatalf("%d events and %d rows, want %d and %d", len(evs), len(rows), len(want.events), len(want.rows))
			}
			if !reflect.DeepEqual(evs, want.events) {
				t.Errorf("events\n got %+v\nwant %+v", evs, want.events)
			}
			if !reflect.DeepEqual(rows, want.rows) {
				t.Errorf("rows\n got %+v\nwant %+v", rows, want.rows)
			}
		})
	}
	for name := range goldens {
		if _, err := os.Stat(filepath.Join(goldenDir, name)); err != nil {
			t.Errorf("expectation for %s, but no such body", name)
		}
	}
}

// The variable catalogue seen on 2026-08-19 lists no contact id (another
// token is not ruled out), so neither the reply templates nor the reply
// bodies carry contact_id: replies are matched by email.
func TestReplyBodiesAndTemplatesCarryNoContactID(t *testing.T) {
	bodies, _ := filepath.Glob(filepath.Join(goldenDir, "apollo_reply_*.json"))
	templates, _ := filepath.Glob(filepath.Join("../../setup/apollo", "email_*.json"))
	if len(bodies) == 0 || len(templates) == 0 {
		t.Fatalf("found %d reply bodies and %d reply templates", len(bodies), len(templates))
	}
	for _, f := range append(bodies, templates...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if _, ok := m["contact_id"]; ok {
			t.Errorf("%s carries contact_id, which the template leaves out", filepath.Base(f))
		}
		if _, ok := m["contact_email"]; !ok {
			t.Errorf("%s has no contact_email, the reply body's only key", filepath.Base(f))
		}
	}
}
