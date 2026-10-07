package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

const factsHeader = "domain,facts,previous,rollups,first_seen,enriched_at,not_found_at,enrich_failed_at"

// Two stored companies: one enriched, with a changed fact; one Apollo did not
// know, whose domain a spreadsheet would read as a formula.
var factsRows = []api.Row{
	{"domain": "zeta.example", "facts": `{"employees":{"value":"120","origin":"enrichment","at":"2026-10-01T00:00:00Z"},` +
		`"funding_stage":{"value":"Series B","origin":"input","at":"2026-09-01T00:00:00Z"}}`,
		"previous":    `{"employees":{"value":"80","origin":"enrichment","at":"2026-09-01T00:00:00Z"}}`,
		"enriched_at": "2026-10-01T00:00:00Z"},
	{"domain": "+cmd.example", "facts": `{"region":{"value":"In\u001b[31mdia","origin":"input","at":"2026-09-01T00:00:00Z"}}`,
		"not_found_at": "2026-10-02T00:00:00Z"},
}

// sqliteFactsInstall is a SQLite install whose store holds rows in Company
// facts (none when rows is nil). It returns leadscore.yml and the store path.
func sqliteFactsInstall(t *testing.T, rows []api.Row) (cfg, db string) {
	t.Helper()
	dir := t.TempDir()
	db = filepath.Join(dir, "store.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	w := []api.TableWrite{{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"},
		Rows: []api.Row{{"kind": "result", "key": "last_result", "value": "healthy"}}}}
	if rows != nil {
		w = append(w, api.TableWrite{Table: model.TableCompanyFacts, Op: api.OpUpsert, Key: []string{"domain"}, Rows: rows})
	}
	if err := s.Commit(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfg = filepath.Join(dir, "leadscore.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\nstore: { type: sqlite, path: store.db }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, db
}

// checkFacts checks both outputs of `facts` over factsRows.
func checkFacts(t *testing.T, cfg string) {
	t.Helper()
	code, out, errOut := cli("facts", "--config", cfg)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 || !strings.HasPrefix(lines[0], "domain") || !strings.HasPrefix(lines[1], "+cmd.example") {
		t.Fatalf("facts (sorted by domain): %d %q %q", code, out, errOut)
	}
	if want := "employees=120 (enrichment; was 80 from enrichment), funding_stage=Series B (input)"; !strings.HasSuffix(lines[2], want) ||
		!strings.Contains(lines[2], "2026-10-01T00:00:00Z") {
		t.Errorf("zeta's line lacks %q: %q", want, lines[2])
	}
	if strings.ContainsRune(out, '\x1b') || !strings.Contains(lines[1], "2026-10-02T00:00:00Z") {
		t.Errorf("+cmd's line: %q", lines[1])
	}

	code, out, _ = cli("facts", "--csv", "--config", cfg)
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if code != 0 || err != nil || len(recs) != 3 || strings.Join(recs[0], ",") != factsHeader {
		t.Fatalf("facts --csv: %d %v %q", code, err, out)
	}
	if recs[1][0] != "'+cmd.example" {
		t.Errorf("a formula-like domain must be quoted: %q", recs[1][0])
	}
	if recs[2][0] != "zeta.example" || recs[2][1] != factsRows[0]["facts"] || recs[2][2] != factsRows[0]["previous"] ||
		recs[2][5] != "2026-10-01T00:00:00Z" || recs[1][6] != "2026-10-02T00:00:00Z" {
		t.Errorf("CSV must carry the stored text: %q", recs)
	}
}

// facts prints the stored rows and writes them as CSV with the table's own
// columns, and never changes the SQLite file.
func TestFactsSQLite(t *testing.T) {
	cfg, db := sqliteFactsInstall(t, factsRows)
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	checkFacts(t, cfg)
	after, _ := os.ReadFile(db)
	if !bytes.Equal(before, after) {
		t.Error("facts changed the SQLite file")
	}
	// A read-only reader of a WAL database may leave an empty -wal beside
	// it (SQLite's own index files), but never data.
	for _, suffix := range []string{"-wal", "-journal"} {
		if data, err := os.ReadFile(db + suffix); err == nil && len(data) > 0 {
			t.Errorf("facts wrote %d bytes to %s", len(data), suffix)
		}
	}
}

func TestFactsEmpty(t *testing.T) {
	cfg, _ := sqliteFactsInstall(t, nil)
	if code, out, _ := cli("facts", "--config", cfg); code != 0 || !strings.Contains(out, "Company facts is empty") {
		t.Errorf("facts on an empty store: %d %q", code, out)
	}
	if code, out, _ := cli("facts", "--csv", "--config", cfg); code != 0 || strings.TrimSpace(out) != factsHeader {
		t.Errorf("facts --csv on an empty store is the header only: %d %q", code, out)
	}
	// No SQLite file yet: facts says so and does not create one.
	db := filepath.Join(filepath.Dir(cfg), "store.db")
	os.Remove(db)
	if code, _, errOut := cli("facts", "--config", cfg); code != 1 || !strings.Contains(errOut, "no SQLite file") {
		t.Errorf("facts with no file: %d %q", code, errOut)
	}
	if fileExists(db) {
		t.Error("facts created the SQLite file")
	}
}

// On Sheets, facts reads the hidden tab and writes nothing.
func TestFactsSheets(t *testing.T) {
	fs, url := fakeGoogle(t)
	path := writeConfig(t, strings.Replace(hostedConfig, "%s", url, 1))
	if code, _, stderr := run("setup", "sheet", "--config", path); code != exitOK {
		t.Fatalf("setup sheet: %s", stderr)
	}
	c, err := config.Load(config.Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	header := strings.Split(factsHeader, ",")
	tab := [][]any{toAny(header)}
	for _, r := range factsRows {
		row := make([]string, len(header))
		for i, col := range header {
			row[i] = r[col]
		}
		tab = append(tab, toAny(row))
	}
	if err := fs.Put(c.Store.Spreadsheet, model.TableCompanyFacts, tab); err != nil {
		t.Fatal(err)
	}
	writes := fs.Calls("batchUpdate")
	checkFacts(t, path)
	if n := fs.Calls("batchUpdate"); n != writes {
		t.Errorf("facts wrote to the spreadsheet (%d batchUpdate calls)", n-writes)
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
