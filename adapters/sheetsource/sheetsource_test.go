package sheetsource_test

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/adapters/sheetsource"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
)

func setup(t *testing.T) (*fakesheets.Server, func(cfg api.Config) api.Config) {
	t.Helper()
	fs := fakesheets.New()
	srv := httptest.NewServer(gcs.Route(gcs.New(), fs))
	t.Cleanup(srv.Close)
	return fs, func(cfg api.Config) api.Config {
		cfg["base_url"], cfg["_http_client"] = srv.URL, srv.Client()
		return cfg
	}
}

func TestFetchRows(t *testing.T) {
	fs, with := setup(t)
	id := fs.NewSpreadsheet("team")
	fs.Put(id, "Leads", [][]any{
		{"Work Email", "Full Name", "Employees", "Email"},
		{"a@x.example", "Ann", 120, "second@x.example"},
		{},
		{"  ", nil},
		{"b@y.example", nil, true, nil, "past the header"},
	})
	fs.Put(id, "Conference", [][]any{{"email", "title"}, {"c@z.example", "CTO"}})
	src, err := sheetsource.New(with(api.Config{"id": "leads", "spreadsheet": id, "tabs": []any{"Leads", "Conference"}}))
	if err != nil {
		t.Fatal(err)
	}
	rows, events, next, err := src.Fetch(t.Context(), "anything")
	if err != nil || len(events) != 0 || next != "" {
		t.Fatalf("Fetch = %d rows, %d events, %q, %v", len(rows), len(events), next, err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3 (blank rows skipped)", len(rows))
	}
	r := rows[0]
	if r.SourceID != "leads" || !slices.Equal(r.Headers, []string{"Work Email", "Full Name", "Employees", "Email"}) {
		t.Errorf("row 0 = %+v", r)
	}
	// Formatted values, as a person sees them; the first of two same-named
	// headers would win, and both are kept as written here.
	if r.Columns["Employees"] != "120" || r.Columns["Email"] != "second@x.example" || r.Columns["Full Name"] != "Ann" {
		t.Errorf("row 0 columns = %v", r.Columns)
	}
	if b := rows[1].Columns; b["Employees"] != "TRUE" || b["Full Name"] != "" || len(b) != 4 {
		t.Errorf("row 1 columns = %v", b)
	}
	if c := rows[2]; !slices.Equal(c.Headers, []string{"email", "title"}) || c.Columns["title"] != "CTO" {
		t.Errorf("row from the second tab = %+v", c)
	}
	reads := strings.Join(fs.ValueReads(), " ")
	if !strings.Contains(reads, "FORMATTED_VALUE 'Leads'") {
		t.Errorf("tabs must be read formatted: %s", reads)
	}
}

// An events tab follows the CSV event rules (contracts section 5.2),
// rejected rows named by their sheet row.
func TestFetchEvents(t *testing.T) {
	fs, with := setup(t)
	id := fs.NewSpreadsheet("team")
	fs.Put(id, "Events import", [][]any{
		{"Event", "At", "Email", "Page"},
		{"visit_pricing", "2026-10-01", "a@x.example", "/pricing"},
		{"sent", "2026-10-01", "b@x.example"},
	})
	src, err := sheetsource.New(with(api.Config{"id": "offline", "spreadsheet": id, "tabs": []any{"Events import"}, "events": true}))
	if err != nil {
		t.Fatal(err)
	}
	rows, events, _, err := src.Fetch(t.Context(), "")
	if err != nil || len(rows) != 0 || len(events) != 2 {
		t.Fatalf("Fetch = %d rows, %d events, %v", len(rows), len(events), err)
	}
	if e := events[0]; e.Kind != "visit_pricing" || e.Email != "a@x.example" || e.Attrs["page"] != "/pricing" || e.Origin != "offline" {
		t.Errorf("event 0 = %+v", e)
	}
	if e := events[1]; e.Kind != "" || !strings.HasPrefix(e.Attrs["reject"], "line 3:") {
		t.Errorf("event 1 = %+v; want rejected, naming sheet row 3", e)
	}
}

func TestFetchFailures(t *testing.T) {
	fs, with := setup(t)
	id := fs.NewSpreadsheet("team")
	fs.Put(id, "Blank", [][]any{{"", " "}, {"a@x.example"}})
	for tab, want := range map[string]string{"Missing": "missing", "Blank": "header row"} {
		src, err := sheetsource.New(with(api.Config{"id": "s", "spreadsheet": id, "tabs": []any{tab}}))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := src.Fetch(t.Context(), ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("tab %s: %v, want %q", tab, err, want)
		}
	}
	for _, cfg := range []api.Config{
		{"spreadsheet": id, "tabs": []any{"Leads"}},
		{"id": "s", "spreadsheet": id},
		{"id": "s", "tabs": []any{"Leads"}},
		{"id": "s", "spreadsheet": id, "tabs": []any{"Leads"}, "events": "yes"},
	} {
		if _, err := sheetsource.New(with(cfg)); err == nil {
			t.Errorf("New(%v) accepted a bad entry", cfg)
		}
	}
	if f, ok := api.SourceFactory("sheetsource"); !ok || f == nil {
		t.Error("sheetsource is not registered")
	}
}
