// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

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
		"rollups":     `{"open_deals":3}`,
		"first_seen":  `{"visit":"2026-09-15T00:00:00Z"}`,
		"enriched_at": "2026-10-01T00:00:00Z"},
	{"domain": "+cmd.example", "facts": `{"region":{"value":"In\u001b[31mdia","origin":"input","at":"2026-09-01T00:00:00Z"}}`,
		"not_found_at": "2026-10-02T00:00:00Z"},
}

// sqliteFactsInstall is a SQLite install whose store holds rows in Company
// facts (none when rows is nil). It returns leadscore.yml and the store path.
// With leftoverWAL, the store is copied while its writer is still open, as a
// crash would leave it: the rows are only in the -wal file.
func sqliteFactsInstall(t *testing.T, rows []api.Row, leftoverWAL bool) (cfg, db string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "store.db")
	s, err := sqlite.Open(src)
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
	dir := t.TempDir()
	db = filepath.Join(dir, "store.db")
	if leftoverWAL {
		for _, suffix := range []string{"", "-wal"} {
			data, err := os.ReadFile(src + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(db+suffix, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.Close()
	} else {
		_ = s.Close()
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(db, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	for i, col := range recs[0] {
		for j, want := range []api.Row{factsRows[1], factsRows[0]} {
			if col != "domain" && recs[j+1][i] != want[col] {
				t.Errorf("CSV %s of %s = %q, want the stored %q", col, want["domain"], recs[j+1][i], want[col])
			}
		}
	}
}

// facts prints the stored rows and writes them as CSV with the table's own
// columns, and never changes the SQLite files: rows left only in a WAL are
// read, but the WAL is not folded into the database.
func TestFactsSQLite(t *testing.T) {
	cfg, db := sqliteFactsInstall(t, factsRows, true)
	read := func() (main, wal []byte) {
		t.Helper()
		main, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		wal, err = os.ReadFile(db + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return main, wal
	}
	main0, wal0 := read()
	if bytes.Contains(main0, []byte("zeta.example")) || !bytes.Contains(wal0, []byte("zeta.example")) {
		t.Fatal("setup: the rows must be only in the WAL")
	}
	checkFacts(t, cfg)
	main1, wal1 := read()
	if !bytes.Equal(main0, main1) || !bytes.Equal(wal0, wal1) {
		t.Error("facts changed the SQLite file or its WAL")
	}
	if _, err := os.Stat(db + "-journal"); err == nil {
		t.Error("facts left a rollback journal")
	}
}

func TestFactsEmpty(t *testing.T) {
	cfg, _ := sqliteFactsInstall(t, nil, false)
	if code, out, _ := cli("facts", "--config", cfg); code != 0 || !strings.Contains(out, "Company facts is empty") {
		t.Errorf("facts on an empty store: %d %q", code, out)
	}
	if code, out, _ := cli("facts", "--csv", "--config", cfg); code != 0 || strings.TrimSpace(out) != factsHeader {
		t.Errorf("facts --csv on an empty store is the header only: %d %q", code, out)
	}
	// No SQLite file yet: facts says so and does not create one.
	db := filepath.Join(filepath.Dir(cfg), "store.db")
	_ = os.Remove(db)
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
	writes, creates := fs.Calls("batchUpdate"), fs.Calls("create")
	checkFacts(t, path)
	if n, m := fs.Calls("batchUpdate"), fs.Calls("create"); n != writes || m != creates {
		t.Errorf("facts wrote to Google: %d batchUpdate and %d create calls", n-writes, m-creates)
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
