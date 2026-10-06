package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
)

func fakeGoogle(t *testing.T) (*fakesheets.Server, string) {
	t.Helper()
	fs := fakesheets.New()
	srv := httptest.NewServer(gcs.Route(gcs.New(), fs))
	t.Cleanup(srv.Close)
	old := testGoogleClient
	testGoogleClient = srv.Client()
	t.Cleanup(func() { testGoogleClient = old })
	return fs, srv.URL
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leadscore.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const hostedConfig = `version: 1
# the team's store
store:
  type: sheets
  lease_bucket: leadscore-lease
  base_url: %s
hosting:
  project: p
  run_account: leadscore-run
  receiver_account: leadscore-receiver
`

// setup sheet creates the spreadsheet, writes store.spreadsheet (keeping the
// file's comments), shares it with both accounts, and refuses to make a
// second one; --repair then fixes the existing one.
func TestSetupSheet(t *testing.T) {
	fs, url := fakeGoogle(t)
	path := writeConfig(t, strings.Replace(hostedConfig, "%s", url, 1))
	keepPersonClient(t)
	personClient = func(context.Context) (*http.Client, error) {
		t.Error("with base_url set, setup must not sign in as the person")
		return nil, errors.New("no")
	}
	code, stdout, stderr := run("setup", "sheet", "--config", path)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	ids := fs.IDs()
	if len(ids) != 1 {
		t.Fatalf("%d spreadsheets created", len(ids))
	}
	id := ids[0]
	if !strings.Contains(stdout, "https://docs.google.com/spreadsheets/d/"+id) || !strings.Contains(stdout, "leadscore-receiver@p.iam.gserviceaccount.com") {
		t.Errorf("stdout = %q", stdout)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# the team's store") {
		t.Errorf("comments lost:\n%s", data)
	}
	c, err := config.Load(config.Options{ConfigPath: path})
	if err != nil || c.Store.Spreadsheet != id {
		t.Fatalf("store.spreadsheet = %q, %v; want %s", c.Store.Spreadsheet, err, id)
	}
	if n := len(fs.Permissions(id)); n != 2 {
		t.Errorf("shared with %d accounts, want 2", n)
	}

	code, _, stderr = run("setup", "sheet", "--config", path)
	if code != exitFail || !strings.Contains(stderr, "already set") || len(fs.IDs()) != 1 {
		t.Errorf("a second setup: exit %d, %q, %d spreadsheets", code, stderr, len(fs.IDs()))
	}
	code, stdout, stderr = run("setup", "sheet", "--repair", "--config", path)
	if code != exitOK || !strings.Contains(stdout, "repaired") {
		t.Errorf("--repair: exit %d, %q %q", code, stdout, stderr)
	}
}

// A Workspace policy that blocks sharing: the spreadsheet is still recorded,
// and the message names Drive's error and the fix.
func TestSetupSheetShareBlocked(t *testing.T) {
	fs, url := fakeGoogle(t)
	path := writeConfig(t, strings.Replace(hostedConfig, "%s", url, 1))
	fs.BlockSharing("", "The domain policy prevents sharing with users outside the domain.")
	code, _, stderr := run("setup", "sheet", "--config", path)
	if code != exitFail || !strings.Contains(stderr, "domain policy") || !strings.Contains(stderr, "Workspace admin") ||
		!strings.Contains(stderr, "--repair") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	c, _ := config.Load(config.Options{ConfigPath: path})
	if c.Store.Spreadsheet == "" {
		t.Error("the created spreadsheet must be recorded so --repair can finish")
	}
}

// --view on a SQLite store makes the read-only view, shared with the key
// file's account, and writes store.view_spreadsheet.
func TestSetupSheetView(t *testing.T) {
	fs, url := fakeGoogle(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "key.json")
	os.WriteFile(key, []byte(`{"client_email":"one@p.iam.gserviceaccount.com"}`), 0o600)
	rubric, err := os.ReadFile("../../examples/rubric.yml") // its export lane: nurture
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "rubric.yml"), rubric, 0o600)
	path := filepath.Join(dir, "leadscore.yml")
	os.WriteFile(path, []byte("version: 1\nstore: { type: sqlite, path: "+filepath.Join(dir, "x.db")+
		", credentials: "+key+", base_url: "+url+" }\n"), 0o600)
	code, _, stderr := run("setup", "sheet", "--view", "--config", path)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	c, _ := config.Load(config.Options{ConfigPath: path})
	if c.Store.ViewSpreadsheet == "" {
		t.Fatal("view_spreadsheet not written")
	}
	var tabs []string
	for _, sh := range fs.Spreadsheet(c.Store.ViewSpreadsheet).Sheets {
		tabs = append(tabs, sh.Properties.Title)
	}
	if strings.Join(tabs, ",") != "Ranked,Health,Export nurture" {
		t.Errorf("view tabs = %v; want Ranked, Health and the rubric's export lane", tabs)
	}
	// --view on a Sheets store, and plain setup on SQLite, are refused.
	sheetsPath := writeConfig(t, strings.Replace(hostedConfig, "%s", url, 1))
	if code, _, stderr := run("setup", "sheet", "--view", "--config", sheetsPath); code != exitFail || !strings.Contains(stderr, "--view") {
		t.Errorf("--view on sheets: exit %d %q", code, stderr)
	}
	if code, _, stderr := run("setup", "sheet", "--config", path); code != exitFail || !strings.Contains(stderr, "store.type") {
		t.Errorf("setup on sqlite: exit %d %q", code, stderr)
	}
}

// Without base_url, setup signs in as the person through gcloud; a failure
// says how to log in.
func TestSetupSheetSignsInAsPerson(t *testing.T) {
	path := writeConfig(t, "version: 1\nstore: { type: sheets, lease_bucket: b }\nhosting: { project: p, run_account: r }\n")
	called := false
	keepPersonClient(t)
	personClient = func(context.Context) (*http.Client, error) {
		called = true
		return nil, errors.New("run `gcloud auth login --enable-gdrive-access`")
	}
	code, _, stderr := run("setup", "sheet", "--config", path)
	if !called || code != exitFail || !strings.Contains(stderr, "gcloud auth login") {
		t.Errorf("called %v, exit %d, %q", called, code, stderr)
	}
}

func keepPersonClient(t *testing.T) {
	old := personClient
	t.Cleanup(func() { personClient = old })
}

// A base_url in a real leadscore.yml (no test client) is refused: it would
// send the team's data to whatever address the file names.
func TestSetupSheetRefusesFileBaseURL(t *testing.T) {
	path := writeConfig(t, strings.Replace(hostedConfig, "%s", "http://127.0.0.1:9", 1))
	code, _, stderr := run("setup", "sheet", "--config", path)
	if code != exitFail || !strings.Contains(stderr, "base_url") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}
