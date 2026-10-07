package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

const doctorRubric = `version: 1
score:
  contact:
    - { when: { field: title, present: true }, points: 1 }
lanes:
  - { id: list, kind: export, priority: 1, when: { field: title, present: true }, push: "export:list" }
`

// loadIn parses leadscore.yml text in a folder holding rubric.
func loadIn(t *testing.T, yml, rubric string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rubric.yml"), []byte(rubric), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse([]byte(yml), dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func keysOf(ps []Problem) string {
	var out []string
	for _, p := range ps {
		k := p.Key
		if p.Warning {
			k += "(warning)"
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func doctorOnly(t *testing.T, name string) Check {
	t.Helper()
	for _, c := range All() {
		if c.Name() == name {
			if c.InRun() {
				t.Errorf("%s runs only from doctor", name)
			}
			return c
		}
	}
	t.Fatalf("%s is not registered", name)
	return nil
}

func TestRubricVersionCheck(t *testing.T) {
	ck := doctorOnly(t, "rubric-version")
	local, err := rules.Compile([]byte(doctorRubric))
	if err != nil {
		t.Fatal(err)
	}
	withVersion := func(v string) *model.Model {
		m := model.New()
		m.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "rubric_version", Value: v})
		return m
	}
	docker := loadIn(t, "version: 1\nstore: { type: sqlite }\n", doctorRubric)
	hosted := loadIn(t, "version: 1\nstore: { type: sqlite }\nhosting: { project: p }\n", doctorRubric)
	cases := []struct {
		name string
		cfg  *config.Config
		m    *model.Model
		want string
	}{
		{"no store loaded", docker, nil, ""},
		{"no run yet", docker, model.New(), ""},
		{"the same rubric", docker, withVersion(local.Version()), ""},
		{"a changed rubric", docker, withVersion("old-version"), "rubric-version:differs(warning)"},
		{"a changed rubric, hosted", hosted, withVersion("old-version"), "rubric-version:differs(warning)"},
	}
	for _, tc := range cases {
		ps := ck.Run(context.Background(), Env{Config: tc.cfg, Model: tc.m})
		if got := keysOf(ps); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if tc.cfg == hosted && len(ps) == 1 && !strings.Contains(ps[0].Fix, "leadscore config push") {
			t.Errorf("%s: fix %q", tc.name, ps[0].Fix)
		}
	}
}

func TestReceiversCheck(t *testing.T) {
	ck := doctorOnly(t, "receivers")
	status := http.StatusOK
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	cfg := func(extra string) *config.Config {
		return loadIn(t, "version: 1\nstore: { type: sqlite }\n"+extra, doctorRubric)
	}
	cases := []struct {
		name   string
		cfg    *config.Config
		status int
		want   string
	}{
		{"polling, no visit workflows", cfg("replies: polling\n"), 200, ""},
		{"no public_url", cfg("replies: receiver\n"), 200, "receivers:no_public_url(warning)"},
		{"healthy", cfg("receiver: { public_url: " + srv.URL + "/ }\n"), 200, ""},
		{"visit workflows only", cfg("replies: polling\nreceiver: { visit_events: [visit_pricing], public_url: " + srv.URL + " }\n"), 404,
			"receivers:unreachable"},
		{"reachable but unhealthy", cfg("receiver: { public_url: " + srv.URL + " }\n"), 503, "receivers:unhealthy(warning)"},
		{"something else answers", cfg("receiver: { public_url: " + srv.URL + " }\n"), 404, "receivers:unreachable"},
		{"nothing answers", cfg("receiver: { public_url: " + closed.URL + " }\n"), 200, "receivers:unreachable"},
		{"not a web address", cfg("receiver: { public_url: \"leads.example.com\" }\n"), 200, "receivers:public_url"},
	}
	for _, tc := range cases {
		status = tc.status
		if got := keysOf(ck.Run(context.Background(), Env{Config: tc.cfg})); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if path != "/healthz" {
		t.Errorf("probed %q, want /healthz", path)
	}
}

func TestLeaseCheckSQLite(t *testing.T) {
	ck := doctorOnly(t, "lease")
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	env := Env{Store: s}
	if got := keysOf(ck.Run(context.Background(), env)); got != "" {
		t.Errorf("no lease: %q", got)
	}
	if _, err := s.Lease(context.Background(), "run-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	ps := ck.Run(context.Background(), env)
	if keysOf(ps) != "lease:held(warning)" || !strings.Contains(ps[0].Message, "run-1") {
		t.Errorf("held lease: %+v", ps)
	}
	// An expired lease is nothing to show.
	env.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if got := keysOf(ck.Run(context.Background(), env)); got != "" {
		t.Errorf("expired lease: %q", got)
	}
	// doctor never takes it: the lease is still run-1's.
	if owner, _, _ := s.LeaseInfo(context.Background()); owner != "run-1" {
		t.Errorf("lease owner %q", owner)
	}
}

func TestLeaseCheckSheetsBucket(t *testing.T) {
	ck := doctorOnly(t, "lease")
	bucket := gcs.New()
	srv := httptest.NewServer(gcs.Route(bucket, fakesheets.New()))
	defer srv.Close()
	svc, err := sheets.Connect(context.Background(), api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	run := func(b string) string {
		return keysOf(ck.Run(context.Background(), Env{Store: sheets.New(svc, "sheet", b)}))
	}
	if got := run(""); got != "lease:bucket" {
		t.Errorf("no bucket set: %q", got)
	}
	if got := run("missing"); got != "lease:bucket" {
		t.Errorf("missing bucket: %q", got)
	}
	bucket.CreateBucket("lease")
	if got := run("lease"); got != "" {
		t.Errorf("good bucket: %q", got)
	}
	bucket.SetObject("lease", sheets.LeaseObject, []byte(`{"owner":"run-9","expires_at":"`+model.FormatTime(time.Now().Add(time.Hour))+`"}`))
	if got := run("lease"); got != "lease:held(warning)" {
		t.Errorf("held: %q", got)
	}
	bucket.DenyPermission("lease", "storage.objects.create")
	if got := run("lease"); got != "lease:bucket" {
		t.Errorf("not writable: %q", got)
	}
}

func TestPushesEnabledCheck(t *testing.T) {
	ck := doctorOnly(t, "pushes-enabled")
	cold := doctorRubric + `  - { id: intro, kind: cold, priority: 5, when: { field: receiver_only, eq: false }, push: "apollo:sequence/Intro" }
`
	cases := []struct {
		name, yml, rubric, want string
	}{
		{"off", "pushes_enabled: false\n", doctorRubric, "pushes-enabled:off(warning)"},
		{"on", "pushes_enabled: true\n", doctorRubric, ""},
		{"a cold lane with no sink block", "pushes_enabled: true\n", cold, "cold_lane_no_sink:intro(warning)"},
		{"a cold lane with its sink", "pushes_enabled: true\nsinks: { apollo: { mailbox_id: \"m1\" } }\n", cold, ""},
		{"a rubric that does not compile", "pushes_enabled: true\n", "version: 1\nlanes: nope\n", ""},
	}
	for _, tc := range cases {
		c := loadIn(t, "version: 1\nstore: { type: sqlite }\n"+tc.yml, tc.rubric)
		ps := ck.Run(context.Background(), Env{Config: c})
		if got := keysOf(ps); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	hosted := loadIn(t, "version: 1\nstore: { type: sqlite }\nhosting: { project: p }\n", doctorRubric)
	if ps := ck.Run(context.Background(), Env{Config: hosted}); len(ps) != 1 || !strings.Contains(ps[0].Fix, "config push") {
		t.Errorf("hosted fix: %+v", ps)
	}
}
