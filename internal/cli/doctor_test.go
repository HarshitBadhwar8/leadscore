// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

const doctorRubric = `version: 1
score:
  contact:
    - { when: { field: title, contains: head }, points: 5 }
lanes:
  - { id: list, kind: export, priority: 1, when: { field: title, present: true }, push: "export:list" }
`

// doctorInstall writes leadscore.yml (SQLite at store.db, a CSV source, then
// extra), the rubric and leads.csv into a new folder, and returns the
// config's path.
func doctorInstall(t *testing.T, extra, rubric string) string {
	t.Helper()
	return doctorInstallStore(t, "", extra, rubric)
}

// doctorInstallStore is doctorInstall with more keys in the store block.
func doctorInstallStore(t *testing.T, storeKeys, extra, rubric string) string {
	t.Helper()
	if storeKeys != "" {
		storeKeys = ", " + storeKeys
	}
	dir := t.TempDir()
	files := map[string]string{
		"leadscore.yml": "version: 1\nstore: { type: sqlite, path: store.db" + storeKeys + " }\nexport: { dir: out }\n" +
			"sources:\n  - { id: leads, type: csv, path: leads.csv }\n" + extra,
		"rubric.yml": rubric,
		"leads.csv":  "Email,Name,Title\nana@acme.example,Ana A,Head of Ops\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "leadscore.yml")
}

// doctorEnv clears the key variables a test machine may have set, and sets
// the receiver secret unless a test unsets it.
func doctorEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"APOLLO_API_KEY", "HUBSPOT_TOKEN", "LEADSCORE_RECEIVER_SECRET_PREVIOUS", "K_SERVICE", "CLOUD_RUN_JOB"} {
		t.Setenv(v, "")
	}
	t.Setenv("LEADSCORE_RECEIVER_SECRET", "test-secret")
}

// seed changes the install's SQLite store through the model and codec, as a
// run would.
func seed(t *testing.T, cfg string, f func(m *model.Model)) {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(filepath.Dir(cfg), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	m, err := codec.Load(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	f(m)
	if err := s.Commit(context.Background(), codec.Encode(m)); err != nil {
		t.Fatal(err)
	}
}

func tables(t *testing.T, cfg string) map[string][]api.Row {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(filepath.Dir(cfg), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	out := map[string][]api.Row{}
	for _, d := range model.Tables {
		if d.Pattern {
			continue
		}
		rows, err := s.ReadTable(context.Background(), d.Name)
		if err != nil {
			t.Fatal(err)
		}
		out[d.Name] = rows
	}
	return out
}

// A CSV-only install after its first run is green: every check passes or
// warns, doctor exits 0, and prints one line per DoctorOrder check, in its
// order.
func TestDoctorCSVOnlyIsGreen(t *testing.T) {
	doctorEnv(t)
	cfg := doctorInstall(t, "", doctorRubric)
	if code, out, errb := cli("run", "--config", cfg); code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errb)
	}
	code, out, errb := cli("doctor", "--config", cfg)
	if code != exitOK || strings.Contains(out, "FAIL") {
		t.Fatalf("doctor: %d\n%s%s", code, out, errb)
	}
	for _, want := range []string{"ok    rubric\n", "ok    store\n", "warn  pushes-enabled: pushes-enabled:off:",
		"      fix: nothing to do while no sinks block is set up", "warn  receivers: receivers:no_public_url:", "doctor: 0 failed, 2 warnings,"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	// Doctor's order: each check's first line comes after the one before.
	last := -1
	for _, name := range DoctorOrder {
		i := strings.Index(out, "  "+name+"\n")
		if j := strings.Index(out, "  "+name+": "); i < 0 || (j >= 0 && j < i) {
			i = j
		}
		if i < 0 {
			t.Errorf("no line for %s:\n%s", name, out)
			continue
		}
		if i < last {
			t.Errorf("%s is out of DoctorOrder's order:\n%s", name, out)
		}
		last = i
	}
}

// doctor never writes the store and never takes the lease: with no SQLite
// file it creates none (and says the store checks were skipped); after a run,
// every table reads the same before and after it.
func TestDoctorIsReadOnly(t *testing.T) {
	doctorEnv(t)
	cfg := doctorInstall(t, "", doctorRubric)
	db := filepath.Join(filepath.Dir(cfg), "store.db")
	code, out, _ := cli("doctor", "--config", cfg)
	if code != exitOK || !strings.Contains(out, "warn  store: store:not_created:") || !strings.Contains(out, "skip  overrides (no SQLite file yet at ") {
		t.Errorf("doctor before any run: %d\n%s", code, out)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("doctor created the SQLite file: %v", err)
	}

	if code, out, errb := cli("run", "--config", cfg); code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errb)
	}
	before := tables(t, cfg)
	if code, out, _ := cli("doctor", "--config", cfg); code != exitOK {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if after := tables(t, cfg); !reflect.DeepEqual(before, after) {
		t.Error("doctor changed the store")
	}
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if owner, _, err := s.LeaseInfo(context.Background()); err != nil || owner != "" {
		t.Errorf("doctor left the lease held by %q (%v)", owner, err)
	}
}

// A failing check exits 1 and prints its fix; a configuration that does not
// load is one failing line.
func TestDoctorExitCodes(t *testing.T) {
	doctorEnv(t)
	cfg := doctorInstall(t, "", "version: 1\nlanes: nope\n")
	code, out, _ := cli("doctor", "--config", cfg)
	if code != exitFail || !strings.Contains(out, "FAIL  rubric: rubric_invalid:compile:") ||
		!strings.Contains(out, "      fix: fix the file (leadscore rules check <file> lists every error)") {
		t.Errorf("broken rubric: %d\n%s", code, out)
	}
	bad := doctorInstall(t, "no_such_key: 1\n", doctorRubric)
	code, out, _ = cli("doctor", "--config", bad)
	if code != exitFail || !strings.Contains(out, "FAIL  config: ") || !strings.Contains(out, "no_such_key") {
		t.Errorf("broken config: %d\n%s", code, out)
	}
	if strings.Contains(out, "create leadscore.yml") {
		t.Errorf("a broken config is corrected, not created:\n%s", out)
	}
	// No config file: doctor says to create one, not to correct it.
	missing := filepath.Join(t.TempDir(), "leadscore.yml")
	code, out, _ = cli("doctor", "--config", missing)
	if code != exitFail || !strings.Contains(out, "fix: create leadscore.yml (start from examples/leadscore.csv-only.yml) or pass --config") ||
		strings.Contains(out, "correct leadscore.yml") {
		t.Errorf("missing config: %d\n%s", code, out)
	}
	t.Chdir(t.TempDir())
	code, out, _ = cli("doctor")
	if code != exitFail || !strings.Contains(out, "fix: create leadscore.yml (start from examples/leadscore.csv-only.yml) or pass --config") {
		t.Errorf("no config anywhere: %d\n%s", code, out)
	}
}

// On a hosted install doctor reads the keys its adapters need from Secret
// Manager first, so the adapter checks run with them.
func TestDoctorLoadsHostedKeys(t *testing.T) {
	doctorEnv(t)
	f := fakeGCP(t)
	f.CreateSecret("p", hosting.KeySecrets["APOLLO_API_KEY"])
	f.AddVersion("p", hosting.KeySecrets["APOLLO_API_KEY"], []byte("apollo-key-from-secret-manager\n"))
	// base_url without a test client makes the apollo-key check refuse the
	// block before any call, which proves it ran with a key.
	cfg := doctorInstall(t, "enrich: { type: apollo, base_url: \"http://127.0.0.1:1\" }\nhosting: { project: p }\n", doctorRubric)
	_, out, _ := cli("doctor", "--config", cfg)
	if got := os.Getenv("APOLLO_API_KEY"); got != "apollo-key-from-secret-manager" {
		t.Errorf("APOLLO_API_KEY = %q after doctor", got)
	}
	if strings.Contains(out, "secret_missing:APOLLO_API_KEY") || !strings.Contains(out, "FAIL  apollo-key: apollo-key:config:") {
		t.Errorf("doctor output:\n%s", out)
	}
	if strings.Contains(out, "apollo-key-from-secret-manager") {
		t.Error("doctor printed the key")
	}
}

// Every DoctorOrder check is registered, doctor-only ones are not in-run, and
// DoctorOrder names each once.
func TestDoctorOrderMatchesRegistry(t *testing.T) {
	registered := map[string]check.Check{}
	for _, c := range check.All() {
		registered[c.Name()] = c
	}
	seen := map[string]bool{}
	for _, n := range DoctorOrder {
		if seen[n] {
			t.Errorf("%s twice in DoctorOrder", n)
		}
		seen[n] = true
		c, ok := registered[n]
		if !ok {
			t.Errorf("%s is not registered", n)
			continue
		}
		doctorOnly := map[string]bool{"rubric-version": true, "receivers": true, "lease": true, "pushes-enabled": true,
			"hosting": true, "receiver-secret": true}
		if c.InRun() == doctorOnly[n] {
			t.Errorf("%s: InRun %v", n, c.InRun())
		}
	}
}

// healthyReceiver is a /healthz that answers 200.
func healthyReceiver(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Every DoctorOrder name has a doctorRows test (the root test checks the
// docs/reference.md table; this one checks the order list doctor prints from).
func TestEveryDoctorCheckHasARowTest(t *testing.T) {
	for _, name := range DoctorOrder {
		if _, ok := doctorRows[name]; !ok {
			t.Errorf("doctorRows has no test for %s", name)
		}
	}
	if len(doctorRows) != len(DoctorOrder) {
		t.Errorf("doctorRows has %d entries, DoctorOrder %d", len(doctorRows), len(DoctorOrder))
	}
}

// fileBytes reads every file of the store: the database and its -wal and
// -journal companions, and the -shm index's mode, with their modes.
func fileBytes(t *testing.T, db string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Stat(db + suffix)
		if os.IsNotExist(err) {
			continue
		}
		data, err := os.ReadFile(db + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if suffix == "-shm" {
			// The WAL index is shared memory every reader updates; it holds
			// no data, so only its mode counts.
			data = nil
		}
		out[suffix] = info.Mode().String() + ":" + string(data)
	}
	return out
}

// copyStore copies the store's files into the folder of another install's
// config, as a backup taken while a process held them would be.
func copyStore(t *testing.T, from, toCfg string) string {
	t.Helper()
	to := filepath.Join(filepath.Dir(toCfg), "store.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(from + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to+suffix, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return to
}

// doctor opens SQLite read-only: a leftover WAL is not folded into the file,
// a hot rollback journal is not rolled back, and no file changes its mode.
func TestDoctorLeavesSQLiteFilesAlone(t *testing.T) {
	doctorEnv(t)
	src := doctorInstall(t, "", doctorRubric)
	if code, out, errb := cli("run", "--config", src); code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errb)
	}
	srcDB := filepath.Join(filepath.Dir(src), "store.db")

	t.Run("leftover WAL", func(t *testing.T) {
		db, err := sql.Open("sqlite", srcDB+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TABLE crash_left_this (x text); INSERT INTO crash_left_this VALUES ('in the wal')`); err != nil {
			t.Fatal(err)
		}
		cfg := doctorInstall(t, "", doctorRubric)
		dst := copyStore(t, srcDB, cfg) // the process "crashed": its WAL is left
		if _, ok := fileBytes(t, dst)["-wal"]; !ok {
			t.Fatal("setup: no WAL left")
		}
		before := fileBytes(t, dst)
		code, out, _ := cli("doctor", "--config", cfg)
		if code != exitOK || !strings.Contains(out, "ok    store\n") {
			t.Errorf("doctor on a store with a WAL: %d\n%s", code, out)
		}
		after := fileBytes(t, dst)
		for k := range before {
			if before[k] != after[k] {
				t.Errorf("doctor changed store.db%s (%d -> %d bytes)", k, len(before[k]), len(after[k]))
			}
		}
	})

	t.Run("hot rollback journal", func(t *testing.T) {
		db, err := sql.Open("sqlite", srcDB+"?_pragma=journal_mode(DELETE)&_pragma=cache_size(1)")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`CREATE TABLE half_done (x text)`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			if _, err := tx.Exec(`INSERT INTO half_done VALUES (?)`, strings.Repeat("x", 2000)); err != nil {
				t.Fatal(err)
			}
		}
		cfg := doctorInstall(t, "", doctorRubric)
		dst := copyStore(t, srcDB, cfg) // a crash mid-transaction leaves a hot journal
		if _, ok := fileBytes(t, dst)["-journal"]; !ok {
			t.Fatal("setup: no rollback journal left")
		}
		before := fileBytes(t, dst)
		cli("doctor", "--config", cfg)
		if after := fileBytes(t, dst); !reflect.DeepEqual(before, after) {
			t.Error("doctor changed the SQLite files (rolled the journal back, or changed a mode)")
		}
	})
}

// Without a store that opened, lease is skipped, not passed.
func TestDoctorSkipsLeaseWithoutAStore(t *testing.T) {
	doctorEnv(t)
	cfg := doctorInstall(t, "", doctorRubric)
	_, out, _ := cli("doctor", "--config", cfg)
	if !strings.Contains(out, "skip  lease (no SQLite file yet at ") || strings.Contains(out, "ok    lease") {
		t.Errorf("doctor output:\n%s", out)
	}
}

// doctor has no input headers, so a rubric field no stored row carries is a
// warning there (the run decides), and says why.
func TestDoctorUnknownRubricFieldIsAWarning(t *testing.T) {
	doctorEnv(t)
	cfg := doctorInstall(t, "", doctorRubric)
	if code, out, errb := cli("run", "--config", cfg); code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errb)
	}
	// dockdoors is a squashed header no lead row has.
	rubric := strings.Replace(doctorRubric, "lanes:", "    - { when: { field: dockdoors, present: true }, points: 1 }\nlanes:", 1)
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "rubric.yml"), []byte(rubric), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := cli("doctor", "--config", cfg)
	if code != exitOK || !strings.Contains(out, "warn  rubric: rubric_unknown_field:dockdoors:") ||
		!strings.Contains(out, "doctor reads no input headers") {
		t.Errorf("doctor: %d\n%s", code, out)
	}
}
