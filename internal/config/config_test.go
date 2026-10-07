package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

func noEnv(string) string { return "" }

func mustParse(t *testing.T, yml string) *Config {
	t.Helper()
	c, err := Parse([]byte(yml), "/team", noEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

const minimal = "version: 1\nstore: { type: sqlite }\n"

// Every leadscore.yml default, from a file that sets only the required keys.
func TestDefaults(t *testing.T) {
	c := mustParse(t, minimal)
	checks := []struct {
		name      string
		got, want any
	}{
		{"version", c.Version, 1},
		{"rubric", c.RubricPath, "/team/rubric.yml"},
		{"store.type", c.Store.Type, "sqlite"},
		{"store.path", c.Store.Path, "/data/leadscore.db"},
		{"store.credentials", c.Store.Credentials, ""},
		{"sources", len(c.Sources), 0},
		{"enrich", c.Enrich == nil, true},
		{"replies", c.Replies, "receiver"},
		{"polling.sequence_length", c.Polling.SequenceLength, 30 * day},
		{"polling.window_margin", c.Polling.WindowMargin, 7 * day},
		{"receiver.public_url", c.Receiver.PublicURL, ""},
		{"receiver.visit_events", c.Receiver.VisitEvents, []string{}},
		{"receiver.port", c.Receiver.Port, 8080},
		{"sinks", len(c.Sinks), 0},
		{"export.dir", c.Export.Dir, "/out"},
		{"reply_labels", c.ReplyLabels, map[string]string{}},
		{"pushes_enabled", c.PushesEnabled, false},
		{"schedule", c.Schedule, 15 * time.Minute},
		{"deadline", c.Deadline, 12 * time.Minute},
		{"ingest_chunk_rows", c.IngestChunkRows, 2000},
		{"silence_threshold", c.SilenceThreshold, 3 * day},
		{"log_retention", c.LogRetention, 90 * day},
		{"hosting", c.Hosting == nil, true},
	}
	for _, ck := range checks {
		if !reflect.DeepEqual(ck.got, ck.want) {
			t.Errorf("%s = %#v, want %#v", ck.name, ck.got, ck.want)
		}
	}
}

func TestBlockDefaults(t *testing.T) {
	c := mustParse(t, `
version: 1
store: { type: sheets, spreadsheet: sheet-1, lease_bucket: b }
sources:
  - { id: leads, type: sheetsource, tabs: [Leads] }
  - { id: conf, type: csv, path: in/conf.csv, channel: conference }
enrich: { type: apollo }
sinks: { apollo: { mailbox_id: m1 }, hubspot: { pipeline: Sales, stage: Prospect } }
`)
	if c.Enrich.Type != "apollo" || c.Enrich.MaxAge != 30*day || c.Enrich.MaxLookupsPerRun != 100 || c.Enrich.MaxLookupsPerDay != 400 {
		t.Errorf("enrich defaults = %+v", *c.Enrich)
	}
	if c.Store.Path != "" {
		t.Errorf("store.path on sheets = %q, want empty (the default is SQLite's)", c.Store.Path)
	}
	leads, conf := c.Sources[0], c.Sources[1]
	if leads.Channel != "leads" || leads.Events || leads.ApolloHeld || leads.MatchDomainName {
		t.Errorf("source defaults = %+v", leads)
	}
	if conf.Channel != "conference" {
		t.Errorf("explicit channel = %q", conf.Channel)
	}
	if got := conf.Block["path"]; got != "/team/in/conf.csv" {
		t.Errorf("source path = %v, want resolved against the config folder", got)
	}
	if got := c.Sinks["hubspot"]["property_prefix"]; got != "leadscore_" {
		t.Errorf("sinks.hubspot.property_prefix = %v", got)
	}
	if _, has := c.Sinks["apollo"]["property_prefix"]; has {
		t.Error("property_prefix is a HubSpot key only")
	}
	if c.Sinks["apollo"]["mailbox_id"] != "m1" {
		t.Errorf("sinks.apollo = %v", c.Sinks["apollo"])
	}
}

func TestSheetSourceGetsStoreSpreadsheetAndCredentials(t *testing.T) {
	c := mustParse(t, `
version: 1
store: { type: sheets, spreadsheet: S, credentials: key.json }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] }, { id: c, type: csv, path: a.csv } ]
`)
	if b := c.Sources[0].Block; b["spreadsheet"] != "S" || b["credentials"] != "/team/key.json" {
		t.Errorf("sheetsource block = %v", b)
	}
	if _, has := c.Sources[1].Block["spreadsheet"]; has {
		t.Error("only sheetsource entries get the spreadsheet")
	}

	sq := mustParse(t, `
version: 1
store: { type: sqlite, view_spreadsheet: V }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]
`)
	if got := sq.Sources[0].Block["spreadsheet"]; got != "V" {
		t.Errorf("on SQLite the sheetsource gets view_spreadsheet, got %v", got)
	}
}

func TestOverridesAndPaths(t *testing.T) {
	c := mustParse(t, `
version: 1
rubric: rules/icp.yml
store: { type: sqlite, path: data/ls.db }
replies: polling
polling: { sequence_length: 45d, window_margin: 1d12h }
receiver: { public_url: "https://x.example", visit_events: [visit_pricing], port: 9000 }
export: { dir: out }
reply_labels: { out_of_office: replied_neutral, _unlabelled: none }
pushes_enabled: true
schedule: 1h
deadline: 20m
ingest_chunk_rows: 500
silence_threshold: 1d
log_retention: 30d
hosting: { project: p, region: asia-south1 }
`)
	if c.RubricPath != "/team/rules/icp.yml" || c.Store.Path != "/team/data/ls.db" || c.Export.Dir != "/team/out" {
		t.Errorf("paths not resolved: %q %q %q", c.RubricPath, c.Store.Path, c.Export.Dir)
	}
	if c.Replies != "polling" || c.Polling.SequenceLength != 45*day || c.Polling.WindowMargin != 36*time.Hour {
		t.Errorf("polling = %v %+v", c.Replies, c.Polling)
	}
	if c.Receiver.Port != 9000 || !reflect.DeepEqual(c.Receiver.VisitEvents, []string{"visit_pricing"}) {
		t.Errorf("receiver = %+v", c.Receiver)
	}
	if !c.PushesEnabled || c.Schedule != time.Hour || c.Deadline != 20*time.Minute || c.IngestChunkRows != 500 ||
		c.SilenceThreshold != day || c.LogRetention != 30*day {
		t.Errorf("scalars not read: %+v", c)
	}
	if c.ReplyLabels["out_of_office"] != "replied_neutral" || c.ReplyLabels["_unlabelled"] != "none" {
		t.Errorf("reply_labels = %v", c.ReplyLabels)
	}
	if c.Hosting.Project != "p" || c.Hosting.Region != "asia-south1" {
		t.Errorf("hosting = %+v", *c.Hosting)
	}
}

func TestReceiverPortFromEnv(t *testing.T) {
	env := func(k string) string {
		if k == "PORT" {
			return "7070"
		}
		return ""
	}
	c, err := Parse([]byte(minimal), "/team", env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Receiver.Port != 7070 {
		t.Errorf("port = %d, want $PORT", c.Receiver.Port)
	}
	c, err = Parse([]byte(minimal+"receiver: { port: 9000 }\n"), "/team", env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Receiver.Port != 9000 {
		t.Errorf("port = %d, want the explicit key over $PORT", c.Receiver.Port)
	}
	if _, err := Parse([]byte(minimal), "/team", func(string) string { return "http" }); err == nil {
		t.Error("a non-numeric $PORT must fail loading")
	}
}

func TestRejects(t *testing.T) {
	tests := []struct {
		name, yml, wantErr string
	}{
		{"unknown top-level key", minimal + "pushes_enabeld: true\n", `"pushes_enabeld"`},
		{"unknown polling key", minimal + "polling: { length: 30d }\n", `"polling.length"`},
		{"unknown receiver key", minimal + "receiver: { url: x }\n", `"receiver.url"`},
		{"unknown export key", minimal + "export: { path: x }\n", `"export.path"`},
		{"unknown hosting key", minimal + "hosting: { zone: x }\n", `"hosting.zone"`},
		{"missing version", "store: { type: sqlite }\n", "version"},
		{"wrong version", "version: 2\nstore: { type: sqlite }\n", "version"},
		{"missing store", "version: 1\n", "store"},
		{"missing store type", "version: 1\nstore: { path: x }\n", "store.type"},
		{"source without id", minimal + "sources: [ { type: csv } ]\n", "sources[0].id"},
		{"duplicate source id", minimal + "sources: [ { id: a, type: csv }, { id: a, type: csv } ]\n", "another source"},
		{"source id receiver", minimal + "sources: [ { id: receiver, type: csv } ]\n", "reserved"},
		{"source id polling", minimal + "sources: [ { id: polling, type: csv } ]\n", "reserved"},
		{"source id hubspot", minimal + "sources: [ { id: hubspot, type: csv } ]\n", "reserved"},
		{"source id apollo_lookup", minimal + "sources: [ { id: apollo_lookup, type: csv } ]\n", "reserved"},
		{"unsubscribe override in another case", minimal + "reply_labels: { Unsubscribe: none }\n", "unsubscribe"},
		{"reply_labels colliding in case", minimal + "reply_labels: { Not_Interested: none, not_interested: replied_neutral }\n", "twice"},
		{"bad replies", minimal + "replies: webhook\n", "replies"},
		{"bad duration", minimal + "schedule: soon\n", "schedule"},
		{"numeric duration", minimal + "deadline: 12\n", "deadline"},
		{"zero deadline", minimal + "deadline: 0s\n", "deadline"},
		{"bad bool", minimal + "pushes_enabled: maybe\n", "pushes_enabled"},
		{"bad chunk", minimal + "ingest_chunk_rows: 0\n", "ingest_chunk_rows"},
		{"unsubscribe override", minimal + "reply_labels: { unsubscribe: none }\n", "unsubscribe"},
		{"bad label value", minimal + "reply_labels: { willing_to_meet: contacted }\n", "willing_to_meet"},
		{"bad enrich budget", minimal + "enrich: { type: apollo, max_lookups_per_run: -1 }\n", "max_lookups_per_run"},
		{"empty file", "", "version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yml), "/team", noEnv)
			if err == nil {
				t.Fatalf("Parse succeeded, want an error naming %s", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not name %s", err, tt.wantErr)
			}
		})
	}
}

// Adapter blocks are passed through: a plug-in may read keys the engine does not know.
func TestAdapterBlocksPassThrough(t *testing.T) {
	c := mustParse(t, `
version: 1
store: { type: postgres, dsn: "postgres://x", pool: 4 }
sources: [ { id: crm, type: mycrm, endpoint: "https://crm.example" } ]
enrich: { type: clearbit, tier: gold }
sinks: { mysink: { anything: [1, 2] } }
`)
	if c.Store.Block["dsn"] != "postgres://x" || c.Store.Block["pool"] != 4 {
		t.Errorf("store block = %v", c.Store.Block)
	}
	if c.Sources[0].Block["endpoint"] != "https://crm.example" || c.Sources[0].Block["id"] != "crm" {
		t.Errorf("source block = %v", c.Sources[0].Block)
	}
	if c.Enrich.Block["tier"] != "gold" || c.Enrich.Block["max_age"] != "30d" {
		t.Errorf("enrich block = %v", c.Enrich.Block)
	}
	if !reflect.DeepEqual(c.Sinks["mysink"]["anything"], []any{1, 2}) {
		t.Errorf("sink block = %v", c.Sinks["mysink"])
	}
	// A factory changing its block must not change what Get reports.
	c.Store.Block["dsn"] = "changed"
	if v, _ := c.Get("store.dsn"); v != "postgres://x" {
		t.Errorf("Get(store.dsn) = %q after a block was mutated", v)
	}
}

func TestParseDuration(t *testing.T) {
	good := map[string]time.Duration{
		"15m": 15 * time.Minute, "90d": 90 * day, "1d12h": 36 * time.Hour, "0s": 0, "2h30m": 150 * time.Minute,
	}
	for in, want := range good {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-5m", "1d-1h", "3 days", "15"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded, want an error", in)
		}
	}
}

func TestGet(t *testing.T) {
	c := mustParse(t, `
version: 1
store: { type: sqlite }
sources: [ { id: leads, type: csv, path: leads.csv } ]
hosting: { project: proj-1 }
`)
	tests := map[string]string{
		"version":                     "1",
		"store.type":                  "sqlite",
		"store.path":                  "/data/leadscore.db",
		"rubric":                      "/team/rubric.yml",
		"schedule":                    "15m",
		"deadline":                    "12m",
		"log_retention":               "90d",
		"ingest_chunk_rows":           "2000",
		"pushes_enabled":              "false",
		"replies":                     "receiver",
		"polling.sequence_length":     "30d",
		"receiver.port":               "8080",
		"export.dir":                  "/out",
		"hosting.project":             "proj-1",
		"sources.0.id":                "leads",
		"sources.0.channel":           "leads",
		"sources.0.path":              "/team/leads.csv",
		"sources.0.match_domain_name": "false",
	}
	for key, want := range tests {
		if got, err := c.Get(key); err != nil || got != want {
			t.Errorf("Get(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range []string{"hosting.region", "nope", "sources.1.id", "store.type.x", "enrich.type"} {
		if _, err := c.Get(key); !errors.Is(err, ErrKeyNotSet) {
			t.Errorf("Get(%q) err = %v, want ErrKeyNotSet", key, err)
		}
	}
	if got, _ := c.Get("hosting"); got != "project: proj-1" {
		t.Errorf("Get(hosting) = %q, want the block as YAML", got)
	}
}

// withDefaultPaths points the default locations into a temp folder.
func withDefaultPaths(t *testing.T) (configDir, cwd string) {
	t.Helper()
	root := t.TempDir()
	configDir = filepath.Join(root, "config")
	cwd = filepath.Join(root, "work")
	for _, d := range []string{configDir, cwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldB, oldC, oldL := defaultBundlePath, defaultConfigPath, localConfigPath
	defaultBundlePath = filepath.Join(configDir, "bundle.yaml")
	defaultConfigPath = filepath.Join(configDir, "leadscore.yml")
	localConfigPath = filepath.Join(cwd, "leadscore.yml")
	t.Cleanup(func() { defaultBundlePath, defaultConfigPath, localConfigPath = oldB, oldC, oldL })
	return configDir, cwd
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPathOrder(t *testing.T) {
	configDir, cwd := withDefaultPaths(t)
	write(t, filepath.Join(cwd, "leadscore.yml"), minimal+"schedule: 1h\n")

	c, err := Load(Options{Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if c.Path != filepath.Join(cwd, "leadscore.yml") || c.Schedule != time.Hour {
		t.Fatalf("with only ./leadscore.yml, loaded %s", c.Path)
	}
	if c.RubricPath != filepath.Join(cwd, "rubric.yml") {
		t.Errorf("rubric = %s, want beside leadscore.yml", c.RubricPath)
	}

	write(t, filepath.Join(configDir, "leadscore.yml"), minimal+"schedule: 2h\n")
	if c, err = Load(Options{Getenv: noEnv}); err != nil || c.Schedule != 2*time.Hour {
		t.Fatalf("/config/leadscore.yml must win over ./leadscore.yml: %v %v", c, err)
	}

	write(t, filepath.Join(configDir, "bundle.yaml"),
		"config: |\n  version: 1\n  store: { type: sqlite }\n  schedule: 3h\nrubric: |\n  version: 1\n  lanes: []\n")
	c, err = Load(Options{Getenv: noEnv, RubricPath: "/ignored/rubric.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Bundle || c.Schedule != 3*time.Hour {
		t.Fatalf("/config/bundle.yaml must win when present: bundle=%v schedule=%v", c.Bundle, c.Schedule)
	}
	if c.RubricPath != "" {
		t.Errorf("with a bundle, --rubric is ignored; RubricPath = %q", c.RubricPath)
	}
	if r, _ := c.Rubric(); string(r) != "version: 1\nlanes: []\n" {
		t.Errorf("bundle rubric = %q", r)
	}

	// --config overrides every default.
	other := filepath.Join(cwd, "other.yml")
	write(t, other, minimal+"schedule: 4h\n")
	if c, err = Load(Options{ConfigPath: other, Getenv: noEnv}); err != nil || c.Schedule != 4*time.Hour {
		t.Fatalf("--config must override the defaults: %v %v", c, err)
	}
}

func TestRubricFlagAndKey(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "leadscore.yml")
	write(t, cfg, minimal+"rubric: icp.yml\n")
	write(t, filepath.Join(dir, "icp.yml"), "version: 1\n")

	c, err := Load(Options{ConfigPath: cfg, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if c.RubricPath != filepath.Join(dir, "icp.yml") {
		t.Errorf("rubric key = %s", c.RubricPath)
	}
	if r, err := c.Rubric(); err != nil || string(r) != "version: 1\n" {
		t.Errorf("Rubric() = %q, %v", r, err)
	}

	flag := filepath.Join(dir, "flag.yml")
	c, err = Load(Options{ConfigPath: cfg, RubricPath: flag, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if c.RubricPath != flag {
		t.Errorf("--rubric must override the key: %s", c.RubricPath)
	}
	if got, _ := c.Get("rubric"); got != flag {
		t.Errorf("Get(rubric) = %s, want the flag's path", got)
	}
}

func TestBundleRejects(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"extra key":       "config: |\n  version: 1\nrubric: x\nnotes: y\n",
		"missing rubric":  "config: |\n  version: 1\n  store: { type: sqlite }\n",
		"config not text": "config: { version: 1 }\nrubric: x\n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		write(t, p, text)
		if _, err := Load(Options{ConfigPath: p, Getenv: noEnv}); err == nil {
			t.Errorf("%s: Load succeeded, want an error", name)
		}
	}
	// A leadscore.yml with a stray `config` key is not a bundle: the key is named.
	p := filepath.Join(dir, "stray.yml")
	write(t, p, minimal+"config: x\n")
	if _, err := Load(Options{ConfigPath: p, Getenv: noEnv}); err == nil || !strings.Contains(err.Error(), `"config"`) {
		t.Errorf("stray config key: err = %v", err)
	}
}

// Polled labels compare lowercased, so reply_labels keys are lowercased too.
func TestReplyLabelKeysAreLowercased(t *testing.T) {
	c := mustParse(t, minimal+"reply_labels: { Not_Interested: replied_neutral }\n")
	if c.ReplyLabels["not_interested"] != "replied_neutral" || len(c.ReplyLabels) != 1 {
		t.Errorf("%v", c.ReplyLabels)
	}
}

func TestNoConfigAnywhereNamesEveryPath(t *testing.T) {
	configDir, cwd := withDefaultPaths(t)
	_, err := Load(Options{Getenv: noEnv})
	if err == nil {
		t.Fatal("Load with no config anywhere succeeded")
	}
	for _, want := range []string{
		filepath.Join(configDir, "bundle.yaml"),
		filepath.Join(configDir, "leadscore.yml"),
		filepath.Join(cwd, "leadscore.yml"),
		"mounted at /config",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestMissingConfigFlagKeepsPlainError(t *testing.T) {
	withDefaultPaths(t)
	_, err := Load(Options{Getenv: noEnv, ConfigPath: filepath.Join(t.TempDir(), "missing.yml")})
	if err == nil || !strings.Contains(err.Error(), "reading config") || strings.Contains(err.Error(), "mounted at /config") {
		t.Fatalf("--config to a missing file: got %v, want the plain reading config error", err)
	}
}
