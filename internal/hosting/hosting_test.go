// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hosting_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcp"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

func fake(t *testing.T) (*gcp.Server, hosting.Connector) {
	t.Helper()
	f := gcp.New()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, func(ctx context.Context) (*hosting.Client, error) {
		return hosting.Connect(ctx, api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	}
}

func TestCron(t *testing.T) {
	ok := map[string]string{
		"1m": "*/1 * * * *", "5m": "*/5 * * * *", "15m": "*/15 * * * *", "30m": "*/30 * * * *",
		"60m": "0 */1 * * *", "1h": "0 */1 * * *", "2h": "0 */2 * * *", "12h": "0 */12 * * *",
		"24h": "0 0 * * *", "1d": "0 0 * * *",
	}
	for in, want := range ok {
		d, err := config.ParseDuration(in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := hosting.Cron(d)
		if err != nil || got != want {
			t.Errorf("Cron(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"7m", "45m", "90m", "5h", "36h", "48h", "2d", "90s", "15m30s"} {
		d, _ := config.ParseDuration(in)
		if got, err := hosting.Cron(d); err == nil {
			t.Errorf("Cron(%s) = %q; want refused", in, got)
		}
	}
}

func TestCheckSchedule(t *testing.T) {
	for _, tt := range []struct {
		schedule, deadline string
		ok                 bool
	}{
		{"15m", "12m", true},     // 12m + 90s = 13m30s < 15m
		{"15m", "13m30s", false}, // equal: the next run would find the lease held
		{"15m", "14m", false},
		{"7m", "5m", false}, // no cron form
		{"1h", "50m", true},
		{"0.25h", "12m", false},    // 15m, but not a form setup/gcp.sh reads
		{"900000ms", "12m", false}, // likewise
		{"15m", "0.2h", false},
	} {
		c := hostedConfig(t, fmt.Sprintf("version: 1\nstore: { type: sqlite }\nschedule: %q\ndeadline: %q\n", tt.schedule, tt.deadline))
		if err := hosting.CheckSchedule(c); (err == nil) != tt.ok {
			t.Errorf("schedule %s deadline %s: err %v, want ok %v", tt.schedule, tt.deadline, err, tt.ok)
		}
	}
	if got := hosting.TaskTimeout(12 * time.Minute); got != 810*time.Second {
		t.Errorf("TaskTimeout(12m) = %s, want 810s", got)
	}
}

// base_url is for tests only: without a test client it is refused, so a
// file could never send keys to an address it names.
func TestConnectRefusesBaseURLWithoutClient(t *testing.T) {
	_, err := hosting.Connect(context.Background(), api.Config{"base_url": "https://attacker.example"})
	if err == nil || !strings.Contains(err.Error(), "tests only") {
		t.Fatalf("Connect with base_url and no client: %v", err)
	}
	var typedNil *http.Client
	if _, err := hosting.Connect(context.Background(), api.Config{"base_url": "https://x.example", "_http_client": typedNil}); err == nil {
		t.Fatal("a typed-nil client must count as absent")
	}
}

func TestSecretVersions(t *testing.T) {
	f, connect := fake(t)
	ctx := context.Background()
	c, err := connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddSecretVersion(ctx, "p", hosting.ConfigSecret, []byte("x")); err == nil || !strings.Contains(err.Error(), "setup/gcp.sh secrets") {
		t.Errorf("adding to a missing secret: %v", err)
	}
	f.CreateSecret("p", hosting.ConfigSecret)
	if _, _, err := c.AccessSecret(ctx, "p", hosting.ConfigSecret); !hosting.IsNotFound(err) {
		t.Errorf("reading a secret with no version: %v, want not found", err)
	}
	for i, want := range []string{"1", "2"} {
		v, err := c.AddSecretVersion(ctx, "p", hosting.ConfigSecret, []byte{'a' + byte(i)})
		if err != nil || v != want {
			t.Fatalf("AddSecretVersion = %q, %v; want %s", v, err, want)
		}
	}
	data, v, err := c.AccessSecret(ctx, "p", hosting.ConfigSecret)
	if err != nil || string(data) != "b" || v != "2" {
		t.Errorf("AccessSecret = %q, %q, %v; want b, 2", data, v, err)
	}
	f.DisableVersion("p", hosting.ConfigSecret, "2")
	if data, v, _ := c.AccessSecret(ctx, "p", hosting.ConfigSecret); string(data) != "a" || v != "1" {
		t.Errorf("latest skips a disabled version: got %q, %q", data, v)
	}
}

// A value that does not match its checksum is refused rather than used.
func TestAccessSecretChecksMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"projects/1/secrets/s/versions/3","payload":{"data":"aGVsbG8=","dataCrc32c":"1"}}`))
	}))
	defer srv.Close()
	c, _ := hosting.Connect(context.Background(), api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	if _, _, err := c.AccessSecret(context.Background(), "p", "s"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a bad checksum: %v", err)
	}
}

func hostedConfig(t *testing.T, yml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(yml), t.TempDir(), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const hostedYAML = `version: 1
store: { type: sheets, spreadsheet: s, lease_bucket: b }
enrich: { type: apollo }
hosting: { project: p, region: asia-south1, run_account: leadscore-run }
`

func TestLoadKeys(t *testing.T) {
	t.Cleanup(func() { logredact.MaskEnvSecrets(func(string) string { return "" }) })
	f, connect := fake(t)
	f.CreateSecret("p", "apollo-api-key")
	f.AddVersion("p", "apollo-api-key", []byte("apollo-key-from-sm\n"))
	f.CreateSecret("p", "hubspot-token")
	f.AddVersion("p", "hubspot-token", []byte("hubspot-token-from-sm"))
	ctx := context.Background()

	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	setenv := func(k, v string) error { env[k] = v; return nil }

	// Only the keys a configured adapter needs: enrich is Apollo, so the
	// HubSpot token is never read.
	c := hostedConfig(t, hostedYAML)
	if err := hosting.LoadKeys(ctx, c, getenv, setenv, connect); err != nil {
		t.Fatal(err)
	}
	if env["APOLLO_API_KEY"] != "apollo-key-from-sm" {
		t.Errorf("APOLLO_API_KEY = %q", env["APOLLO_API_KEY"])
	}
	if _, read := env["HUBSPOT_TOKEN"]; read || slices.ContainsFunc(f.Calls(), func(c string) bool { return strings.Contains(c, "hubspot-token") }) {
		t.Error("read the HubSpot token though no adapter needs it")
	}
	if got := logredact.Redact("key apollo-key-from-sm"); got != "key [REDACTED]" {
		t.Errorf("the key read from Secret Manager is not masked: %q", got)
	}

	// A variable already set is kept and not read again.
	before := len(f.Calls())
	env["APOLLO_API_KEY"] = "from-env"
	if err := hosting.LoadKeys(ctx, c, getenv, setenv, connect); err != nil || env["APOLLO_API_KEY"] != "from-env" || len(f.Calls()) != before {
		t.Errorf("a set variable was replaced or read: %q, %v", env["APOLLO_API_KEY"], err)
	}
}

// Inside Cloud Run, and on an install that is not hosted, nothing is read
// from Secret Manager.
func TestLoadKeysNeverInsideCloudRun(t *testing.T) {
	f, connect := fake(t)
	f.CreateSecret("p", "apollo-api-key")
	f.AddVersion("p", "apollo-api-key", []byte("apollo-key-from-sm"))
	for name, envv := range map[string]map[string]string{
		"job":     {"CLOUD_RUN_JOB": "leadscore-run"},
		"service": {"K_SERVICE": "leadscore-receiver"},
	} {
		env := envv
		set := func(k, v string) error { env[k] = v; return nil }
		if err := hosting.LoadKeys(context.Background(), hostedConfig(t, hostedYAML), func(k string) string { return env[k] }, set, connect); err != nil {
			t.Fatal(err)
		}
		if env["APOLLO_API_KEY"] != "" {
			t.Errorf("%s: read a key inside Cloud Run", name)
		}
	}
	env := map[string]string{}
	notHosted := hostedConfig(t, strings.Replace(hostedYAML, "hosting: { project: p,", "hosting: {", 1))
	if err := hosting.LoadKeys(context.Background(), notHosted, func(k string) string { return env[k] }, func(k, v string) error { env[k] = v; return nil }, connect); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls()) != 0 || env["APOLLO_API_KEY"] != "" {
		t.Errorf("Secret Manager was called: %v", f.Calls())
	}
}

// A key the run account cannot read is named in the error and left empty;
// the others are still read.
func TestLoadKeysReportsUnreadableKey(t *testing.T) {
	t.Cleanup(func() { logredact.MaskEnvSecrets(func(string) string { return "" }) })
	f, connect := fake(t)
	f.CreateSecret("p", "apollo-api-key")
	f.AddVersion("p", "apollo-api-key", []byte("apollo-key-from-sm"))
	f.CreateSecret("p", "hubspot-token")
	f.Deny("p", "hubspot-token")
	env := map[string]string{}
	c := hostedConfig(t, hostedYAML+"sinks: { hubspot: { pipeline: x, stage: y } }\n")
	err := hosting.LoadKeys(context.Background(), c, func(k string) string { return env[k] }, func(k, v string) error { env[k] = v; return nil }, connect)
	if err == nil || !strings.Contains(err.Error(), "HUBSPOT_TOKEN") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error %v must name HUBSPOT_TOKEN and the refusal", err)
	}
	if env["APOLLO_API_KEY"] != "apollo-key-from-sm" || env["HUBSPOT_TOKEN"] != "" {
		t.Errorf("env after a partial read: %v", env)
	}
}

func TestResourceReads(t *testing.T) {
	f, connect := fake(t)
	ctx := context.Background()
	c, _ := connect(ctx)
	const run = "/run/v2/projects/p/locations/r/"
	if _, err := c.ReceiverService(ctx, "p", "r"); !hosting.IsNotFound(err) {
		t.Errorf("missing service: %v", err)
	}
	f.SetResource(run+"services/leadscore-receiver", `{"template":{"scaling":{"maxInstanceCount":3}},"scaling":{"maxInstanceCount":1}}`)
	if s, err := c.ReceiverService(ctx, "p", "r"); err != nil || s.MaxInstances != 1 {
		t.Errorf("service-level limit caps the template's: %+v, %v", s, err)
	}
	f.SetResource(run+"services/leadscore-receiver", `{"template":{}}`)
	if s, _ := c.ReceiverService(ctx, "p", "r"); s.MaxInstances != 0 {
		t.Errorf("no limit set: %+v", s)
	}

	f.SetResource(run+"jobs/leadscore-run", `{"template":{"template":{"timeout":"810s","maxRetries":0}}}`)
	j, err := c.RunJob(ctx, "p", "r")
	if err != nil || j.TaskTimeout != 810*time.Second || j.MaxRetries == nil || *j.MaxRetries != 0 {
		t.Errorf("RunJob = %+v, %v", j, err)
	}
	f.SetResource(run+"jobs/leadscore-run", `{"template":{"template":{"serviceAccount":"r@p.iam.gserviceaccount.com","timeout":"810s","containers":[{"env":[`+
		`{"name":"PLAIN","value":"x"},{"name":"LEADSCORE_CONFIG_VERSION","valueSource":{"secretKeyRef":{"secret":"projects/p/secrets/leadscore-config-version","version":"latest"}}}]}]}}}`)
	if j, _ := c.RunJob(ctx, "p", "r"); j.Account != "r@p.iam.gserviceaccount.com" || len(j.SecretEnv) != 1 ||
		j.SecretEnv["LEADSCORE_CONFIG_VERSION"] != (hosting.SecretRef{Secret: "leadscore-config-version", Version: "latest"}) {
		t.Errorf("job account %q, secret env %v", j.Account, j.SecretEnv)
	}
	if j, _ := c.RunJob(ctx, "p", "r"); j.MaxRetries != nil {
		t.Errorf("an unset maxRetries must read as unset, not 0: %v", *j.MaxRetries)
	}

	f.SetResource(run+"jobs/leadscore-run:getIamPolicy", `{"bindings":[{"role":"roles/run.invoker","members":["serviceAccount:s@p.iam.gserviceaccount.com"]},{"role":"roles/run.viewer","members":["user:x"]}]}`)
	if m, err := c.JobInvokers(ctx, "p", "r"); err != nil || len(m) != 1 || m[0] != "serviceAccount:s@p.iam.gserviceaccount.com" {
		t.Errorf("JobInvokers = %v, %v", m, err)
	}

	f.SetResource("/cloudscheduler/v1/projects/p/locations/r/jobs/leadscore-schedule",
		`{"schedule":"*/15 * * * *","state":"PAUSED","httpTarget":{"uri":"`+hosting.JobRunURI("p", "r")+`","oauthToken":{"serviceAccountEmail":"s@p.iam.gserviceaccount.com"}}}`)
	if s, err := c.Schedule(ctx, "p", "r"); err != nil || s.Schedule != "*/15 * * * *" || !s.Paused || s.Account != "s@p.iam.gserviceaccount.com" ||
		s.URI != "https://run.googleapis.com/v2/projects/p/locations/r/jobs/leadscore-run:run" {
		t.Errorf("Schedule = %+v, %v", s, err)
	}

	if ok, err := c.ProxyRepositoryExists(ctx, "p", "r"); ok || err != nil {
		t.Errorf("missing proxy: %v, %v", ok, err)
	}
	f.SetResource("/artifactregistry/v1/projects/p/locations/r/repositories/ghcr-proxy", `{"mode":"REMOTE_REPOSITORY"}`)
	if ok, err := c.ProxyRepositoryExists(ctx, "p", "r"); !ok || err != nil {
		t.Errorf("proxy: %v, %v", ok, err)
	}
}

func TestUsesProxy(t *testing.T) {
	for image, want := range map[string]bool{
		"ghcr.io/tetriz-ai/leadscore:v0.1.0":                              true,
		"asia-south1-docker.pkg.dev/p/ghcr-proxy/tetriz-ai/leadscore:v1":  true,
		"asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:ab": false,
	} {
		if got := hosting.UsesProxy(image); got != want {
			t.Errorf("UsesProxy(%s) = %v", image, got)
		}
	}
	if got := hosting.AccountEmail("leadscore-run", "p"); got != "leadscore-run@p.iam.gserviceaccount.com" {
		t.Errorf("AccountEmail = %s", got)
	}
}

// A redirect is never followed: Google's sign-in transport adds the token to
// every request, so a redirect would carry it to another host. The caller's
// client is left alone.
func TestCallDoesNotFollowRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect to %s", r.URL.Path)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	for _, cfg := range []api.Config{
		{"base_url": srv.URL, "_http_client": srv.Client()},
		{"_http_client": &http.Client{Transport: rewrite{srv.URL}}},
	} {
		c, err := hosting.Connect(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.AccessSecret(context.Background(), "p", "s"); err == nil || !strings.Contains(err.Error(), "302") {
			t.Errorf("a redirect answer: %v", err)
		}
		if cfg["_http_client"].(*http.Client).CheckRedirect != nil {
			t.Error("the caller's client was changed")
		}
	}
}

// rewrite sends every request to one test server, as a signed-in client
// without base_url would send it to Google.
type rewrite struct{ base string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.base)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}
