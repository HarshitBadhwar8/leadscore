package receiver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

const repo = "../.."

func readRepo(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The image runs as the non-root user leadscore with its own HOME (contracts
// section 5.1), with leadscore as the entrypoint.
func TestDockerfileRunsAsLeadscore(t *testing.T) {
	df := string(readRepo(t, "Dockerfile"))
	for _, want := range []string{"USER leadscore", "ENV HOME=/home/leadscore", `ENTRYPOINT ["leadscore"]`, "-h /home/leadscore leadscore", "chmod 700 /data /out"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

// compose.yaml carries every setting the brief and RFC 6.9 rely on.
func TestComposeFile(t *testing.T) {
	var c struct {
		Services map[string]struct {
			Image           string   `yaml:"image"`
			Command         []string `yaml:"command"`
			Volumes         []string `yaml:"volumes"`
			Ports           []string `yaml:"ports"`
			Restart         string   `yaml:"restart"`
			StopGracePeriod string   `yaml:"stop_grace_period"`
			EnvFile         string   `yaml:"env_file"`
			Profiles        []string `yaml:"profiles"`
			Healthcheck     struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
		Volumes map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(readRepo(t, "compose.yaml"), &c); err != nil {
		t.Fatal(err)
	}
	ls, ok := c.Services["leadscore"]
	if !ok {
		t.Fatal("no leadscore service")
	}
	if !slices.Equal(ls.Command, []string{"serve", "--every"}) {
		t.Errorf("command = %v, want serve --every", ls.Command)
	}
	for _, v := range []string{"./:/config:ro", "leadscore-data:/data", "./out:/out"} {
		if !slices.Contains(ls.Volumes, v) {
			t.Errorf("volumes %v lack %s", ls.Volumes, v)
		}
	}
	if _, ok := c.Volumes["leadscore-data"]; !ok {
		t.Error("leadscore-data is not a named volume")
	}
	if ls.Restart == "" || ls.Restart == "no" {
		t.Error("no restart policy")
	}
	if d, err := time.ParseDuration(ls.StopGracePeriod); err != nil || d < 120*time.Second {
		t.Errorf("stop_grace_period = %q, want 120s: the run's save budget is 90 seconds", ls.StopGracePeriod)
	}
	if !slices.Equal(ls.Healthcheck.Test, []string{"CMD", "leadscore", "healthz"}) {
		t.Errorf("healthcheck = %v, want leadscore healthz", ls.Healthcheck.Test)
	}
	if ls.EnvFile != ".env" {
		t.Errorf("env_file = %q, want .env", ls.EnvFile)
	}
	for _, p := range ls.Ports {
		if !strings.HasPrefix(p, "127.0.0.1:") {
			t.Errorf("port %q is published beyond this machine", p)
		}
	}
	caddy, ok := c.Services["caddy"]
	if !ok || !slices.Contains(caddy.Profiles, "caddy") {
		t.Error("Caddy must be an optional service behind the caddy profile")
	}
	pinned := regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	if !pinned.MatchString(caddy.Image) {
		t.Errorf("caddy image %q is not pinned by digest", caddy.Image)
	}
}

// The example leadscore.yml files load, and set what their path needs.
func TestExampleConfigs(t *testing.T) {
	load := func(name string) *config.Config {
		c, err := config.Parse(readRepo(t, name), "/config", func(string) string { return "" })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return c
	}
	laptop := load("examples/leadscore.laptop.yml")
	if laptop.Replies != "polling" || laptop.Store.Type != "sqlite" || laptop.PushesEnabled {
		t.Errorf("laptop example: replies %q, store %q, pushes %v; want polling, sqlite, off", laptop.Replies, laptop.Store.Type, laptop.PushesEnabled)
	}
	server := load("examples/leadscore.server.yml")
	if server.Replies != "receiver" || server.Receiver.PublicURL == "" || server.PushesEnabled {
		t.Errorf("server example: replies %q, public_url %q, pushes %v", server.Replies, server.Receiver.PublicURL, server.PushesEnabled)
	}
	ci := load("testdata/compose/leadscore.yml")
	if ci.Store.Path != "/data/leadscore.db" || ci.Export.Dir != "/out" {
		t.Errorf("the CI install must use the compose mounts: store %q, export %q", ci.Store.Path, ci.Export.Dir)
	}
	if _, err := rules.Compile(readRepo(t, "testdata/compose/rubric.yml")); err != nil {
		t.Errorf("the CI rubric does not compile: %v", err)
	}
}

// The Apollo workflow templates use the C5.1 event names, and a body built
// from each (its placeholders filled) parses to the event it is for.
func TestWorkflowTemplates(t *testing.T) {
	placeholder := regexp.MustCompile(`"<[^">]*>"`)
	values := map[string]string{
		"contact_email": "dana@example.com", "contact_stage": "Interested", "last_conversation_link": "https://app.apollo.io/#/conv/1",
		"contact_id": "ct-1", "contact_name": "Dana Example", "contact_title": "VP Ops", "contact_linkedin_url": "https://www.linkedin.com/in/dana-example",
		"account_domain": "example.com", "visited_at": "2026-09-01T10:00:00Z", "id": "ct-1", "email": "dana@example.com",
		"linkedin_url": "https://www.linkedin.com/in/dana-example", "first_name": "Dana", "last_name": "Example", "title": "VP Ops",
		"company": "Example Co", "domain": "example.com", "website_url": "https://example.com", "name": "Example Co",
	}
	cases := map[string]struct{ kind, want string }{
		"email_sent.json":             {apollo.KindReply, "sent"},
		"email_replied.json":          {apollo.KindReply, "replied"},
		"email_replied_positive.json": {apollo.KindReply, "replied_positive"},
		"email_unsubscribed.json":     {apollo.KindReply, "unsubscribed"},
		"website_visited.json":        {apollo.KindVisit, "visit_pricing"},
	}
	files, _ := filepath.Glob(filepath.Join(repo, "setup/apollo/*.json"))
	if len(files) != len(cases) {
		t.Errorf("setup/apollo has %d templates, want %d", len(files), len(cases))
	}
	for name, c := range cases {
		text := string(readRepo(t, "setup/apollo/"+name))
		// Fill each "<...>" with a sample value for its key.
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			if m := regexp.MustCompile(`"([a-z_]+)": "<`).FindStringSubmatch(l); m != nil {
				v, ok := values[m[1]]
				if !ok {
					t.Fatalf("%s: no sample value for %s", name, m[1])
				}
				lines[i] = placeholder.ReplaceAllString(l, `"`+v+`"`)
			}
		}
		body := []byte(strings.Join(lines, "\n"))
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s is not JSON once filled: %v", name, err)
		}
		if strings.Contains(string(body), secretField) {
			t.Errorf("%s carries the secret field; the secret goes in the header", name)
		}
		evs, rows, err := apollo.ParseRaw(api.RawEvent{Kind: c.kind, ReceivedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), Body: body})
		if err != nil || len(evs) != 1 || evs[0].Kind != c.want || len(rows) != 1 {
			t.Errorf("%s parses to %+v, %d rows, %v; want one %s event and its receiver row", name, evs, len(rows), err, c.want)
		}
	}
}

// The README warns, in the setup steps and next to the templates, that the
// receiver secret must be kept private (contracts section 5.1).
func TestSecretWarningIsPresent(t *testing.T) {
	for _, f := range []string{"README.md", "setup/apollo/README.md", "compose.yaml"} {
		text := strings.ToLower(string(readRepo(t, f)))
		for _, want := range []string{"works like a password", "fake positive reply", "rotate"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", f, want)
			}
		}
	}
}
