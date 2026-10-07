package check

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcp"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

const (
	runBase   = "/run/v2/projects/p/locations/r/"
	schedPath = "/cloudscheduler/v1/projects/p/locations/r/jobs/leadscore-schedule"
	proxyPath = "/artifactregistry/v1/projects/p/locations/r/repositories/ghcr-proxy"
	schedSA   = "leadscore-scheduler@p.iam.gserviceaccount.com"
	runSA     = "leadscore-run@p.iam.gserviceaccount.com"
	recvSA    = "leadscore-receiver@p.iam.gserviceaccount.com"
)

// secretEnv is a container's env JSON with each variable from a secret.
func secretEnv(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, `{"name":"`+pairs[i]+`","valueSource":{"secretKeyRef":{"secret":"projects/p/secrets/`+pairs[i+1]+`","version":"latest"}}}`)
	}
	return `[{"env":[` + strings.Join(parts, ",") + `]}]`
}

var (
	healthyService = `{"template":{"serviceAccount":"` + recvSA + `","scaling":{"maxInstanceCount":1},"containers":` +
		secretEnv("LEADSCORE_RECEIVER_SECRET", "receiver-secret") + `}}`
	healthyJob = `{"template":{"template":{"serviceAccount":"` + runSA + `","timeout":"810s","maxRetries":0,"containers":` +
		secretEnv("LEADSCORE_CONFIG_VERSION", "leadscore-config-version") + `}}}`
)

// healthyProject is everything setup/gcp.sh makes for deadline 12m, as the
// hosting check should find it.
func healthyProject(f *gcp.Server) {
	f.SetResource(runBase+"services/leadscore-receiver", healthyService)
	f.SetResource(runBase+"jobs/leadscore-run", healthyJob)
	f.SetResource(runBase+"jobs/leadscore-run:getIamPolicy",
		`{"bindings":[{"role":"roles/run.invoker","members":["serviceAccount:`+schedSA+`"]}]}`)
	f.SetResource(schedPath, `{"schedule":"*/15 * * * *","state":"ENABLED","httpTarget":{"uri":"`+
		hosting.JobRunURI("p", "r")+`","oauthToken":{"serviceAccountEmail":"`+schedSA+`"}}}`)
	f.SetResource(proxyPath, `{"mode":"REMOTE_REPOSITORY"}`)
	f.CreateSecret("p", hosting.ConfigSecret)
	f.AddVersion("p", hosting.ConfigSecret, []byte("config: x\nrubric: y\n"))
	f.CreateSecret("p", hosting.ConfigVersionSecret)
	f.AddVersion("p", hosting.ConfigVersionSecret, []byte("1"))
}

func runHosting(t *testing.T, f *gcp.Server, yml string, m *model.Model) []string {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	h := hostingCheck{connect: func(ctx context.Context) (*hosting.Client, error) {
		return hosting.Connect(ctx, api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	}}
	var keys []string
	for _, p := range h.Run(context.Background(), Env{Config: load(t, yml), Model: m}) {
		if p.Message == "" || p.Fix == "" {
			t.Errorf("%s: every hosting problem has a message and a fix: %+v", p.Key, p)
		}
		if p.Warning {
			p.Key += " (warning)"
		}
		keys = append(keys, p.Key)
	}
	sort.Strings(keys)
	return keys
}

const hostedSheets = "version: 1\nstore: { type: sheets, spreadsheet: s, lease_bucket: b }\n" +
	"hosting: { project: p, region: r, run_account: leadscore-run, receiver_account: leadscore-receiver, image: ghcr.io/tetriz-ai/leadscore:v0.1.0 }\n"

func modelWithConfigVersion(v string) *model.Model {
	m := model.New()
	m.SetState("config_version", v)
	return m
}

func TestHostingCheckPassesOnAHealthyProject(t *testing.T) {
	f := gcp.New()
	healthyProject(f)
	if got := runHosting(t, f, hostedSheets, modelWithConfigVersion("1")); len(got) != 0 {
		t.Errorf("problems on a healthy project: %v", got)
	}
	// Doctor without a store still checks everything but the version.
	if got := runHosting(t, f, hostedSheets, nil); len(got) != 0 {
		t.Errorf("problems with no model: %v", got)
	}
}

func TestHostingCheckOnlyWithAHostingBlock(t *testing.T) {
	f := gcp.New()
	if got := runHosting(t, f, "version: 1\nstore: { type: sqlite }\n", nil); len(got) != 0 || len(f.Calls()) != 0 {
		t.Errorf("without hosting: problems %v, calls %v", got, f.Calls())
	}
	if got := runHosting(t, f, "version: 1\nstore: { type: sqlite }\nhosting: { project: p }\n", nil); strings.Join(got, ",") != "hosting:config" {
		t.Errorf("hosting with no region: %v", got)
	}
}

func TestHostingCheckFindsEachProblem(t *testing.T) {
	for _, tt := range []struct {
		name  string
		spoil func(f *gcp.Server)
		yml   string
		v     string
		want  string
	}{
		{"service missing", func(f *gcp.Server) { f.SetResource(runBase+"services/leadscore-receiver", "") }, hostedSheets, "1", "hosting:service_missing"},
		{"service may scale out", func(f *gcp.Server) {
			f.SetResource(runBase+"services/leadscore-receiver", strings.Replace(healthyService, `"maxInstanceCount":1`, `"maxInstanceCount":2`, 1))
		}, hostedSheets, "1", "hosting:service_instances"},
		{"service with no instance limit", func(f *gcp.Server) {
			f.SetResource(runBase+"services/leadscore-receiver", strings.Replace(healthyService, `"maxInstanceCount":1`, `"maxInstanceCount":0`, 1))
		}, hostedSheets, "1", "hosting:service_instances"},
		{"job missing", func(f *gcp.Server) { f.SetResource(runBase+"jobs/leadscore-run", "") }, hostedSheets, "1", "hosting:job_missing"},
		{"job timeout not deadline plus budget", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run", strings.Replace(healthyJob, "810s", "600s", 1))
		}, hostedSheets, "1", "hosting:job_timeout"},
		{"job retries", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run", strings.Replace(healthyJob, `"maxRetries":0`, `"maxRetries":3`, 1))
		}, hostedSheets, "1", "hosting:job_retries"},
		{"job retries left at the default", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run", strings.Replace(healthyJob, `"maxRetries":0,`, ``, 1))
		}, hostedSheets, "1", "hosting:job_retries"},
		{"scheduler missing", func(f *gcp.Server) { f.SetResource(schedPath, "") }, hostedSheets, "1", "hosting:schedule_missing"},
		{"scheduler account cannot run the job", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run:getIamPolicy", `{"bindings":[]}`)
		}, hostedSheets, "1", "hosting:scheduler_account"},
		{"scheduler calls something else", func(f *gcp.Server) {
			f.SetResource(schedPath, `{"httpTarget":{"uri":"https://example.com/","oauthToken":{"serviceAccountEmail":"`+schedSA+`"}}}`)
		}, hostedSheets, "1", "hosting:scheduler_account"},
		{"proxy missing for a release image", func(f *gcp.Server) { f.SetResource(proxyPath, "") }, hostedSheets, "1", "hosting:proxy_missing"},
		{"deadline plus budget not below schedule", func(*gcp.Server) {}, hostedSheets + "deadline: 14m\n", "1", "hosting:job_timeout,hosting:schedule"},
		{"a newer bundle than the last run used", func(f *gcp.Server) {
			f.AddVersion("p", hosting.ConfigSecret, []byte("config: x2\nrubric: y\n"))
			f.AddVersion("p", hosting.ConfigVersionSecret, []byte("2"))
		}, hostedSheets, "1", "hosting:config_version (warning)"},
		{"no run recorded a version", func(*gcp.Server) {}, hostedSheets, "", "hosting:config_version (warning)"},
		{"an interrupted push", func(f *gcp.Server) {
			f.AddVersion("p", hosting.ConfigSecret, []byte("config: x2\nrubric: y\n"))
		}, hostedSheets, "1", "hosting:config_interrupted,hosting:config_version (warning)"},
		{"a push from before the version secret", func(f *gcp.Server) {
			f.DisableVersion("p", hosting.ConfigVersionSecret, "1")
		}, hostedSheets, "1", "hosting:config_interrupted"},
		{"job does not read the version secret", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run", strings.Replace(healthyJob, "leadscore-config-version", "leadscore-config", 1))
		}, hostedSheets, "1", "hosting:job_config_version"},
		{"job runs as another account", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run", strings.Replace(healthyJob, runSA, "123-compute@developer.gserviceaccount.com", 1))
		}, hostedSheets, "1", "hosting:job_account"},
		{"receiver runs as another account", func(f *gcp.Server) {
			f.SetResource(runBase+"services/leadscore-receiver", strings.Replace(healthyService, recvSA, runSA, 1))
		}, hostedSheets, "1", "hosting:service_account"},
		{"receiver without its secret", func(f *gcp.Server) {
			f.SetResource(runBase+"services/leadscore-receiver", strings.Replace(healthyService, "LEADSCORE_RECEIVER_SECRET", "OTHER", 1))
		}, hostedSheets, "1", "hosting:receiver_secret"},
		{"rotation not finished", func(f *gcp.Server) {
			f.SetResource(runBase+"services/leadscore-receiver", strings.Replace(healthyService, `"containers":`+secretEnv("LEADSCORE_RECEIVER_SECRET", "receiver-secret"),
				`"containers":`+secretEnv("LEADSCORE_RECEIVER_SECRET", "receiver-secret", "LEADSCORE_RECEIVER_SECRET_PREVIOUS", "receiver-secret-previous"), 1))
		}, hostedSheets, "1", "hosting:receiver_secret_previous (warning)"},
		{"anyone may start the job", func(f *gcp.Server) {
			f.SetResource(runBase+"jobs/leadscore-run:getIamPolicy",
				`{"bindings":[{"role":"roles/run.invoker","members":["serviceAccount:`+schedSA+`","allUsers"]}]}`)
		}, hostedSheets, "1", "hosting:job_public"},
		{"scheduler paused", func(f *gcp.Server) {
			f.SetResource(schedPath, `{"schedule":"*/15 * * * *","state":"PAUSED","httpTarget":{"uri":"`+
				hosting.JobRunURI("p", "r")+`","oauthToken":{"serviceAccountEmail":"`+schedSA+`"}}}`)
		}, hostedSheets, "1", "hosting:schedule_paused (warning)"},
		{"no bundle pushed", func(f *gcp.Server) { f.DisableVersion("p", hosting.ConfigSecret, "1") }, hostedSheets, "1", "hosting:config_missing"},
		{"a schedule setup/gcp.sh cannot read", func(*gcp.Server) {}, hostedSheets + "schedule: 0.25h\n", "1", "hosting:schedule"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := gcp.New()
			healthyProject(f)
			tt.spoil(f)
			if got := strings.Join(runHosting(t, f, tt.yml, modelWithConfigVersion(tt.v)), ","); got != tt.want {
				t.Errorf("problems %q, want %q", got, tt.want)
			}
		})
	}
}

// Before release the private registry's image is deployed directly, with no
// ghcr-proxy repository, and that is not a problem.
func TestHostingCheckPreReleaseImageNeedsNoProxy(t *testing.T) {
	f := gcp.New()
	healthyProject(f)
	f.SetResource(proxyPath, "")
	yml := strings.Replace(hostedSheets, "ghcr.io/tetriz-ai/leadscore:v0.1.0", "asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:abc", 1)
	if got := runHosting(t, f, yml, modelWithConfigVersion("1")); len(got) != 0 {
		t.Errorf("problems: %v", got)
	}
}

// A refusal (the run account lacks a viewer role) is reported as unreadable,
// not as a missing resource.
func TestHostingCheckUnreadable(t *testing.T) {
	f := gcp.New()
	healthyProject(f)
	f.Deny("p", hosting.ConfigSecret)
	if got := strings.Join(runHosting(t, f, hostedSheets, modelWithConfigVersion("1")), ","); got != "hosting:unreadable:secret leadscore-config" {
		t.Errorf("problems %q", got)
	}
}

func TestHostingCheckIsDoctorOnly(t *testing.T) {
	for _, c := range InRun() {
		if c.Name() == "hosting" {
			t.Fatal("the hosting check must not run inside runs (section 10)")
		}
	}
	found := false
	for _, c := range All() {
		found = found || c.Name() == "hosting"
	}
	if !found {
		t.Fatal("the hosting check is not registered")
	}
}
