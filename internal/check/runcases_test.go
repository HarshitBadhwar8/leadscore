// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func TestStoreCheckRefusesLocalFilesOnCloudRun(t *testing.T) {
	csvSrc := config.Source{ID: "leads", Type: "csv", Block: api.Config{"path": "/in/leads.csv"}}
	sheetSrc := config.Source{ID: "tab", Type: "sheetsource", Block: api.Config{"tabs": []any{"Leads"}}}
	for _, tt := range []struct {
		name string
		cfg  *config.Config
		env  map[string]string
		want string // a phrase of the message; empty for no problem
	}{
		{"sqlite on a laptop", &config.Config{Store: config.Store{Type: "sqlite"}}, nil, ""},
		{"sqlite in a Cloud Run job", &config.Config{Store: config.Store{Type: "sqlite"}}, map[string]string{"CLOUD_RUN_JOB": "j"}, "a SQLite store"},
		{"a CSV path in a Cloud Run service", &config.Config{Store: config.Store{Type: "sheets"}, Sources: []config.Source{csvSrc}},
			map[string]string{"K_SERVICE": "s"}, "the CSV path of source leads"},
		{"sheets and tabs on Cloud Run", &config.Config{Store: config.Store{Type: "sheets"}, Sources: []config.Source{sheetSrc}},
			map[string]string{"CLOUD_RUN_JOB": "j"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := storeCheck{getenv: func(k string) string { return tt.env[k] },
				inContainer: func() bool { return true }, mountType: func(string) (string, bool) { return "ext4", true }}
			got := c.Run(context.Background(), Env{Config: tt.cfg, Store: stateStore{}})
			if tt.want == "" {
				if len(got) != 0 {
					t.Errorf("got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Key != "store:cloud_run_files" || !strings.Contains(got[0].Message, tt.want) || got[0].Fix == "" || got[0].Warning {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestUnknownFields(t *testing.T) {
	r, err := rules.Compile([]byte(`version: 1
fields:
  budget: { type: number }
  hq: { level: company }
derive:
  x:
    - when:
        all:
          - { field: budget, gt: 1 }
          - { field: title, present: true }
          - { field: sources_seen, gt: 1 }
          - { field: company.leads_seen, gt: 1 }
          - { field: company.hq, present: true }
          - { field: toolused, present: true }
          - { field: company.tier, present: true }
          - { field: company.funding_stage, present: true }
          - { field: pipeline, present: true }
      then: 1
lanes: []
`))
	if err != nil {
		t.Fatal(err)
	}
	keys := func(ps []Problem) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Key)
		}
		return out
	}
	m := model.New()
	got := keys(UnknownFields(r, m, nil))
	want := []string{"rubric_unknown_field:company.tier", "rubric_unknown_field:pipeline", "rubric_unknown_field:toolused"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("empty model: %v, want %v", got, want)
	}
	// A loaded People field, a Companies tab header and a fetched header (even
	// one whose cells are all empty) are all loaded columns.
	m.Put(model.TablePeople, model.Person{LeadID: "l1", Fields: map[string]model.Field{"toolused": {Value: "x"}}})
	if err := m.Load(model.TableCompanies, []api.Row{{"domain": "a.example", "Tier": "1"}}); err != nil {
		t.Fatal(err)
	}
	if got := keys(UnknownFields(r, m, []string{"Pipeline "})); len(got) != 0 {
		t.Errorf("all loaded: %v", got)
	}

}

// The rubric check applies the field part when it has a model, counting the
// run's fetched headers as loaded.
func TestRubricCheckFieldPart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leadscore.yml"), []byte("version: 1\nstore: { type: sqlite }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rubricText := "version: 1\nderive:\n  x:\n    - { when: { field: mystery, present: true }, then: 1 }\nlanes: []\n"
	if err := os.WriteFile(filepath.Join(dir, "rubric.yml"), []byte(rubricText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{ConfigPath: filepath.Join(dir, "leadscore.yml")})
	if err != nil {
		t.Fatal(err)
	}
	if got := (rubric{}).Run(context.Background(), Env{Config: cfg}); len(got) != 0 {
		t.Errorf("no model (doctor could not load the store): %v", got)
	}
	got := (rubric{}).Run(context.Background(), Env{Config: cfg, Model: model.New()})
	if len(got) != 1 || got[0].Key != "rubric_unknown_field:mystery" || got[0].Warning {
		t.Errorf("got %+v", got)
	}
	if got := (rubric{}).Run(context.Background(), Env{Config: cfg, Model: model.New(), Columns: []string{"Mystery"}}); len(got) != 0 {
		t.Errorf("a fetched header is loaded: %v", got)
	}
}
