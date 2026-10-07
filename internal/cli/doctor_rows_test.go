package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// doctorRow is one contracts section 10 row, driven through `leadscore
// doctor`: setup makes an install where the check finds its problem, and
// want is the line doctor must print for it.
type doctorRow struct {
	setup func(t *testing.T) string // returns the config path
	want  string                    // "FAIL  <check>: <key>" or "warn  <check>: <key>"
}

const coldApolloRubric = doctorRubric + `  - { id: intro, kind: cold, priority: 5, when: { field: receiver_only, eq: false }, push: "apollo:sequence/Intro" }
`

// ran is a CSV-only install after one run, then changed by f.
func ran(t *testing.T, extra string, f func(m *model.Model)) string {
	t.Helper()
	cfg := doctorInstall(t, extra, doctorRubric)
	if code, out, errb := cli("run", "--config", cfg); code != 0 && f != nil {
		t.Fatalf("run: %d %s %s", code, out, errb)
	}
	if f != nil {
		seed(t, cfg, f)
	}
	return cfg
}

// doctorRows has one entry per section 10 row, keyed by the check's name.
// TestEveryDoctorRowHasATestAndASkillEntry (root package) reads these keys.
var doctorRows = map[string]doctorRow{
	"secrets": {func(t *testing.T) string {
		return doctorInstall(t, "enrich: { type: apollo }\n", doctorRubric)
	}, "FAIL  secrets: secret_missing:APOLLO_API_KEY:"},
	"receiver-secret": {func(t *testing.T) string {
		t.Setenv("LEADSCORE_RECEIVER_SECRET", "")
		return doctorInstall(t, "replies: receiver\n", doctorRubric)
	}, "FAIL  receiver-secret: secret_missing:LEADSCORE_RECEIVER_SECRET:"},
	"hosting": {func(t *testing.T) string {
		return doctorInstall(t, "hosting: { project: p }\n", doctorRubric)
	}, "FAIL  hosting: hosting:config:"},
	"rubric-version": {func(t *testing.T) string {
		return ran(t, "", func(m *model.Model) {
			m.Put(model.TableHealth, model.HealthRow{Kind: "result", Key: "rubric_version", Value: "an-older-rubric",
				FirstSeenAt: time.Now(), UpdatedAt: time.Now()})
		})
	}, "warn  rubric-version: rubric-version:differs:"},
	"sheet-access": {func(t *testing.T) string {
		_, url := fakeGoogle(t)
		return doctorInstallStore(t, "view_spreadsheet: \"not-shared\", base_url: "+url, "", doctorRubric)
	}, "FAIL  sheet-access: sheet-access:not-shared:"},
	"sheets": {func(t *testing.T) string {
		fs, url := fakeGoogle(t)
		id := fs.NewSpreadsheet("a view made by hand")
		return doctorInstallStore(t, "view_spreadsheet: \""+id+"\", base_url: "+url, "", doctorRubric)
	}, "FAIL  sheets: sheets:timezone:"},
	"hubspot": {func(t *testing.T) string {
		t.Setenv("HUBSPOT_TOKEN", "pat-test")
		return doctorInstall(t, "sinks: { hubspot: { pipeline: 123 } }\n", doctorRubric)
	}, "FAIL  hubspot: hubspot:config:"},
	"apollo-key": {func(t *testing.T) string {
		t.Setenv("APOLLO_API_KEY", "test-key")
		return doctorInstall(t, "enrich: { type: apollo, base_url: \"http://127.0.0.1:1\" }\n", doctorRubric)
	}, "FAIL  apollo-key: apollo-key:config:"},
	"apollo-sequences": {func(t *testing.T) string {
		t.Setenv("APOLLO_API_KEY", "test-key")
		return doctorInstall(t, "sinks: { apollo: { mailbox_id: \"m1\", base_url: \"http://127.0.0.1:1\" } }\n", coldApolloRubric)
	}, "FAIL  apollo-sequences: apollo-sequences:config:"},
	"receivers": {func(t *testing.T) string {
		gone := httptest.NewServer(nil)
		gone.Close()
		return doctorInstall(t, "receiver: { public_url: "+gone.URL+" }\n", doctorRubric)
	}, "FAIL  receivers: receivers:unreachable:"},
	"receiver-silence": {func(t *testing.T) string {
		return ran(t, "receiver: { public_url: "+healthyReceiver(t)+" }\n", func(m *model.Model) {
			m.SetState("first_run_at", model.FormatTime(time.Now().Add(-30*24*time.Hour)))
		})
	}, "FAIL  receiver-silence: silent:sent:"},
	"lease": {func(t *testing.T) string {
		cfg := ran(t, "", nil)
		s, err := sqlite.Open(filepath.Join(filepath.Dir(cfg), "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Lease(context.Background(), "run-in-progress", time.Hour); err != nil {
			t.Fatal(err)
		}
		return cfg
	}, "warn  lease: lease:held:"},
	"pushes": {func(t *testing.T) string {
		return ran(t, "", func(m *model.Model) {
			m.Put(model.TablePushes, model.Push{LeadID: "L-gone", LaneID: "intro", Step: "contact", LaneKind: "cold",
				Dest: "apollo:sequence/Intro", State: "failed", Attempts: 3, LastError: "refused", UpdatedAt: time.Now()})
		})
	}, "FAIL  pushes: push_failed:L-gone:intro:contact:"},
	"store": {func(t *testing.T) string {
		return ran(t, "", func(m *model.Model) { m.SetState("schema_version", "99.0") })
	}, "FAIL  store: store:newer_schema:"},
	"rubric": {func(t *testing.T) string {
		return doctorInstall(t, "", "version: 1\nlanes: nope\n")
	}, "FAIL  rubric: rubric_invalid:compile:"},
	"overrides": {func(t *testing.T) string {
		return ran(t, "", func(m *model.Model) {
			m.Put(model.TableOverrides, model.Override{Person: "nobody", Action: "status", Value: "bogus"})
		})
	}, "FAIL  overrides: override_unmatched:"},
	"pushes-enabled": {func(t *testing.T) string {
		return doctorInstall(t, "pushes_enabled: false\n", coldApolloRubric)
	}, "warn  pushes-enabled: cold_lane_no_sink:intro:"},
	"duplicates": {func(t *testing.T) string {
		return ran(t, "", func(m *model.Model) {
			m.Put(model.TablePeople, model.Person{LeadID: "L-a", MergedInto: "L-b", CreatedAt: time.Now()})
			m.Put(model.TablePeople, model.Person{LeadID: "L-b", MergedInto: "L-a", CreatedAt: time.Now()})
		})
	}, "FAIL  duplicates: merge_cycle:"},
}

// Each section 10 row, through doctor: the line and its fix are printed, and
// the exit code follows the line (1 for FAIL, 0 when only warnings).
func TestDoctorRows(t *testing.T) {
	for name, row := range doctorRows {
		t.Run(name, func(t *testing.T) {
			doctorEnv(t)
			cfg := row.setup(t)
			code, out, errb := cli("doctor", "--config", cfg)
			i := strings.Index(out, row.want)
			if i < 0 {
				t.Fatalf("doctor output lacks %q:\n%s%s", row.want, out, errb)
			}
			if rest := out[i:]; !strings.Contains(strings.SplitN(rest, "\n", 3)[1], "fix: ") {
				t.Errorf("no fix under %q:\n%s", row.want, out)
			}
			wantCode := exitOK
			if strings.Contains(out, "FAIL  ") {
				wantCode = exitFail
			}
			if code != wantCode || (strings.HasPrefix(row.want, "FAIL") && code != exitFail) {
				t.Errorf("exit %d:\n%s", code, out)
			}
		})
	}
}
