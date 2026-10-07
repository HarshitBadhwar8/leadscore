package leadscore_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
)

// setup/gcp.sh is tested offline against a fake gcloud (testdata/gcloud/gcloud)
// and the real leadscore CLI. PATH holds only the fake, the CLI and the
// system folders, so the real gcloud can never run.

func needBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("setup/gcp.sh runs on macOS, Linux and Cloud Shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	return bash
}

// gcpEnv is one fake project: a folder with leadscore.yml and the rubric, and a
// bin folder with the fake gcloud and the leadscore CLI.
type gcpEnv struct {
	t      *testing.T
	bash   string
	dir    string // the team's folder
	bin    string
	gcloud string // the fake's log and state
	extra  []string
}

func newGCPEnv(t *testing.T, cli, yml string) *gcpEnv {
	t.Helper()
	e := &gcpEnv{t: t, bash: needBash(t), dir: t.TempDir(), bin: t.TempDir(), gcloud: t.TempDir()}
	fake, err := os.ReadFile("testdata/gcloud/gcloud")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bin, "gcloud"), fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, filepath.Join(e.bin, "leadscore")); err != nil {
		t.Fatal(err)
	}
	e.write("leadscore.yml", yml)
	rubric, err := os.ReadFile("testdata/compose/rubric.yml")
	if err != nil {
		t.Fatal(err)
	}
	e.write("rubric.yml", string(rubric))
	return e
}

func (e *gcpEnv) write(name, text string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *gcpEnv) touch(name string) {
	e.t.Helper()
	os.MkdirAll(filepath.Join(e.gcloud, "state"), 0o755)
	if err := os.WriteFile(filepath.Join(e.gcloud, "state", name), []byte("x\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// script runs setup/gcp.sh in the team's folder with stdin not a terminal.
func (e *gcpEnv) script(args ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	repo, _ := os.Getwd()
	cmd := exec.Command(e.bash, append([]string{filepath.Join(repo, "setup/gcp.sh")}, args...)...)
	cmd.Dir = e.dir
	cmd.Env = append([]string{
		"PATH=" + e.bin + ":/usr/bin:/bin",
		"HOME=" + e.dir,
		"FAKE_GCLOUD_DIR=" + e.gcloud,
	}, e.extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

// mustRun runs a step that must succeed, returning the gcloud calls it made.
func (e *gcpEnv) mustRun(args ...string) (calls []string, stdout string) {
	e.t.Helper()
	before := len(e.log())
	code, stdout, stderr := e.script(args...)
	if code != 0 {
		e.t.Fatalf("setup/gcp.sh %v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	return e.log()[before:], stdout
}

func (e *gcpEnv) log() []string {
	data, _ := os.ReadFile(filepath.Join(e.gcloud, "log"))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func (e *gcpEnv) configGet(key string) string {
	e.t.Helper()
	c, err := config.Load(config.Options{ConfigPath: filepath.Join(e.dir, "leadscore.yml")})
	if err != nil {
		e.t.Fatal(err)
	}
	v, _ := c.Get(key)
	return v
}

// wantCall fails unless some call contains every part, in order.
func wantCall(t *testing.T, calls []string, parts ...string) {
	t.Helper()
	for _, c := range calls {
		rest, ok := c, true
		for _, p := range parts {
			i := strings.Index(rest, p)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(p):]
		}
		if ok {
			return
		}
	}
	t.Errorf("no gcloud call with %q in:\n  %s", parts, strings.Join(calls, "\n  "))
}

func noCall(t *testing.T, calls []string, part string) {
	t.Helper()
	for _, c := range calls {
		if strings.Contains(c, part) {
			t.Errorf("unexpected gcloud call %q", c)
		}
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	needBash(t)
	cli := filepath.Join(t.TempDir(), "leadscore")
	out, err := exec.Command("go", "build", "-o", cli, "./cmd/leadscore").CombinedOutput()
	if err != nil {
		t.Fatalf("building the CLI: %v\n%s", err, out)
	}
	return cli
}

const gcpYAML = `version: 1
# the team's settings
store: { type: sheets, spreadsheet: "0123" }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]
enrich: { type: apollo }
replies: receiver
`

func TestGCPScript(t *testing.T) {
	cli := buildCLI(t)

	// The whole runbook, step by step, as a person runs it.
	t.Run("runbook", func(t *testing.T) {
		e := newGCPEnv(t, cli, gcpYAML)
		const runSA = "leadscore-run@p.iam.gserviceaccount.com"
		const recvSA = "leadscore-receiver@p.iam.gserviceaccount.com"

		e.extra = []string{"FAKE_GCLOUD_PROJECT=active-project"} // never used: --project is required
		calls, stdout := e.mustRun("accounts", "--project", "p")
		e.extra = nil
		if !strings.Contains(stdout, "now acts as "+runSA) || !strings.Contains(stdout, "gcloud auth application-default revoke") {
			t.Errorf("accounts must say the machine's login now acts as the run account, and how to undo it: %q", stdout)
		}
		wantCall(t, calls, "services enable", "run.googleapis.com", "cloudscheduler.googleapis.com", "secretmanager.googleapis.com", "iamcredentials.googleapis.com")
		wantCall(t, calls, "iam service-accounts create leadscore-run --project p")
		wantCall(t, calls, "iam service-accounts create leadscore-receiver --project p")
		wantCall(t, calls, "projects add-iam-policy-binding p --member serviceAccount:"+runSA+" --role roles/run.viewer")
		wantCall(t, calls, "projects add-iam-policy-binding p --member serviceAccount:"+runSA+" --role roles/cloudscheduler.viewer")
		wantCall(t, calls, "iam service-accounts add-iam-policy-binding "+runSA, "--member user:person@example.com --role roles/iam.serviceAccountTokenCreator")
		if last := calls[len(calls)-1]; last != "auth application-default login --impersonate-service-account "+runSA {
			t.Errorf("accounts must end with the impersonated login, ended with %q", last)
		}
		for k, want := range map[string]string{"hosting.project": "p", "hosting.region": "asia-south1",
			"hosting.run_account": "leadscore-run", "hosting.receiver_account": "leadscore-receiver"} {
			if got := e.configGet(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		if data, _ := os.ReadFile(filepath.Join(e.dir, "leadscore.yml")); !strings.Contains(string(data), "# the team's settings") {
			t.Error("set-hosting lost the file's comments")
		}
		// Run again: the accounts exist and are not created twice.
		calls, _ = e.mustRun("accounts")
		noCall(t, calls, "service-accounts create")

		// The lease bucket defaults to <project>-leadscore-lease.
		calls, _ = e.mustRun("bucket")
		wantCall(t, calls, "storage buckets create gs://p-leadscore-lease --project p --location asia-south1")
		wantCall(t, calls, "storage buckets add-iam-policy-binding gs://p-leadscore-lease --member serviceAccount:"+runSA+" --role roles/storage.objectAdmin")

		// Keys come from the variables (or a hidden prompt), never from
		// arguments, and are never printed.
		e.extra = []string{"APOLLO_API_KEY=apollo-key-value-123"}
		calls, stdout = e.mustRun("secrets")
		e.extra = nil
		for _, s := range []string{"leadscore-config", "leadscore-config-version", "apollo-api-key", "hubspot-token", "receiver-secret", "receiver-secret-previous"} {
			wantCall(t, calls, "secrets create "+s+" --project p")
		}
		// The C9 role table.
		for _, g := range [][2]string{
			{"apollo-api-key", runSA + " --role roles/secretmanager.secretAccessor"},
			{"hubspot-token", runSA + " --role roles/secretmanager.secretAccessor"},
			{"leadscore-config", runSA + " --role roles/secretmanager.secretAccessor"},
			{"leadscore-config", runSA + " --role roles/secretmanager.secretVersionAdder"},
			{"leadscore-config-version", runSA + " --role roles/secretmanager.secretAccessor"},
			{"leadscore-config-version", runSA + " --role roles/secretmanager.secretVersionAdder"},
			{"leadscore-config-version", runSA + " --role roles/secretmanager.viewer"},
			{"receiver-secret", recvSA + " --role roles/secretmanager.secretAccessor"},
			{"receiver-secret-previous", recvSA + " --role roles/secretmanager.secretAccessor"},
			{"leadscore-config", recvSA + " --role roles/secretmanager.secretAccessor"},
		} {
			wantCall(t, calls, "secrets add-iam-policy-binding "+g[0]+" --project p --member serviceAccount:"+g[1])
		}
		noCall(t, calls, "add-iam-policy-binding apollo-api-key --project p --member serviceAccount:"+recvSA)
		noCall(t, calls, "add-iam-policy-binding leadscore-config-version --project p --member serviceAccount:"+recvSA)
		if got, _ := os.ReadFile(filepath.Join(e.gcloud, "stdin-apollo-api-key")); string(got) != "apollo-key-value-123" {
			t.Errorf("apollo-api-key was given %q", got)
		}
		noCall(t, calls, "versions add hubspot-token") // not set, no terminal: skipped
		secret, _ := os.ReadFile(filepath.Join(e.gcloud, "stdin-receiver-secret"))
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).Match(secret) {
			t.Errorf("generated receiver secret %q", secret)
		}
		for _, line := range append(calls, stdout) {
			if strings.Contains(line, "apollo-key-value-123") || strings.Contains(line, string(secret)) {
				t.Errorf("a secret value leaked into %q", line)
			}
		}
		noCall(t, calls, "versions add receiver-secret-previous")

		// deploy refuses until a bundle is pushed.
		if code, _, stderr := e.script("deploy", "ghcr.io/tetriz-ai/leadscore:v0.1.0"); code == 0 || !strings.Contains(stderr, "leadscore config push") {
			t.Errorf("deploy with no bundle: exit %d %q", code, stderr)
		}
		e.touch("version-leadscore-config") // config push made version 1 ...
		if code, _, stderr := e.script("deploy", "ghcr.io/tetriz-ai/leadscore:v0.1.0"); code == 0 || !strings.Contains(stderr, "leadscore config push") {
			t.Errorf("deploy with no version number: exit %d %q", code, stderr)
		}
		e.touch("version-leadscore-config-version") // ... and recorded its number
		calls, stdout = e.mustRun("deploy", "ghcr.io/tetriz-ai/leadscore:v0.1.0")
		const proxied = "asia-south1-docker.pkg.dev/p/ghcr-proxy/tetriz-ai/leadscore:v0.1.0"
		wantCall(t, calls, "artifacts repositories create ghcr-proxy --project p --location asia-south1", "--mode remote-repository", "--remote-docker-repo https://ghcr.io")
		wantCall(t, calls, "artifacts repositories add-iam-policy-binding ghcr-proxy", "--member serviceAccount:"+runSA+" --role roles/artifactregistry.reader")
		wantCall(t, calls, "run deploy leadscore-receiver --project p --region asia-south1 --image "+proxied,
			"--service-account "+recvSA+" --args serve --min-instances 0 --max-instances 1 --no-invoker-iam-check",
			"--set-secrets LEADSCORE_RECEIVER_SECRET=receiver-secret:latest,/config/bundle.yaml=leadscore-config:latest")
		wantCall(t, calls, "run jobs deploy leadscore-run --project p --region asia-south1 --image "+proxied,
			"--service-account "+runSA+" --args run --tasks 1 --parallelism 1 --max-retries 0 --task-timeout 810s",
			"--set-secrets APOLLO_API_KEY=apollo-api-key:latest,LEADSCORE_CONFIG_VERSION=leadscore-config-version:latest,/config/bundle.yaml=leadscore-config:latest")
		noCall(t, calls, "--set-env-vars")
		if got := e.configGet("hosting.image"); got != "ghcr.io/tetriz-ai/leadscore:v0.1.0" {
			t.Errorf("hosting.image = %q", got)
		}
		if !strings.Contains(stdout, "https://leadscore-receiver-abc-el.a.run.app") || !strings.Contains(stdout, "receiver.public_url") ||
			!strings.Contains(stdout, "leadscore config push again") {
			t.Errorf("deploy must print the receiver's address and say to set receiver.public_url and push again: %q", stdout)
		}

		// A key added once the job exists needs a redeploy to reach it.
		e.extra = []string{"HUBSPOT_TOKEN=hubspot-token-value-1"}
		_, stdout = e.mustRun("secrets")
		e.extra = nil
		if !strings.Contains(stdout, "added HUBSPOT_TOKEN") || !strings.Contains(stdout, "setup/gcp.sh redeploy") {
			t.Errorf("a key added after deploy must say to redeploy: %q", stdout)
		}
		calls, _ = e.mustRun("redeploy")
		wantCall(t, calls, "run jobs deploy leadscore-run", "HUBSPOT_TOKEN=hubspot-token:latest")

		// Rotation (C5.1): the previous secret is attached while it has an
		// enabled version; --finish-rotation detaches it, and only then are
		// its versions disabled.
		e.touch("version-receiver-secret-previous")
		calls, _ = e.mustRun("redeploy")
		wantCall(t, calls, "run deploy leadscore-receiver", "--image "+proxied,
			"LEADSCORE_RECEIVER_SECRET=receiver-secret:latest,LEADSCORE_RECEIVER_SECRET_PREVIOUS=receiver-secret-previous:latest,/config/bundle.yaml")
		noCall(t, calls, "artifacts repositories create")
		calls, stdout = e.mustRun("redeploy", "--finish-rotation")
		noCall(t, calls, "LEADSCORE_RECEIVER_SECRET_PREVIOUS")
		wantCall(t, calls, "run deploy leadscore-receiver", "LEADSCORE_RECEIVER_SECRET=receiver-secret:latest,/config/bundle.yaml")
		if !strings.Contains(stdout, "gcloud secrets versions disable latest --secret receiver-secret-previous --project p") {
			t.Errorf("--finish-rotation must give the disable command: %q", stdout)
		}
		os.Remove(filepath.Join(e.gcloud, "state", "version-receiver-secret-previous"))
		calls, _ = e.mustRun("redeploy")
		noCall(t, calls, "LEADSCORE_RECEIVER_SECRET_PREVIOUS")

		calls, _ = e.mustRun("schedule")
		const schedSA = "leadscore-scheduler@p.iam.gserviceaccount.com"
		wantCall(t, calls, "iam service-accounts create leadscore-scheduler --project p")
		wantCall(t, calls, "run jobs add-iam-policy-binding leadscore-run --project p --region asia-south1 --member serviceAccount:"+schedSA+" --role roles/run.invoker")
		wantCall(t, calls, "scheduler jobs create http leadscore-schedule --project p --location asia-south1 --schedule */15 * * * * --time-zone UTC",
			"--uri "+hosting.JobRunURI("p", "asia-south1")+" --http-method POST --oauth-service-account-email "+schedSA)
		calls, _ = e.mustRun("schedule")
		wantCall(t, calls, "scheduler jobs update http leadscore-schedule")
	})

	// Before release the private registry's image is deployed directly.
	t.Run("pre-release image", func(t *testing.T) {
		e := newGCPEnv(t, cli, gcpYAML+"hosting: { project: p, region: asia-south1, run_account: leadscore-run, receiver_account: leadscore-receiver }\n")
		e.touch("version-leadscore-config")
		e.touch("version-leadscore-config-version")
		const img = "asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:abc123"
		calls, _ := e.mustRun("deploy", img)
		noCall(t, calls, "artifacts repositories")
		wantCall(t, calls, "run jobs deploy leadscore-run", "--image "+img)
	})

	// A dry run changes nothing, in the project or in leadscore.yml, and
	// prints every change it would make.
	t.Run("dry run", func(t *testing.T) {
		yml := gcpYAML + "hosting: { project: p, region: asia-south1, run_account: leadscore-run, receiver_account: leadscore-receiver }\n"
		e := newGCPEnv(t, cli, yml)
		e.touch("version-leadscore-config")
		e.touch("version-leadscore-config-version")
		e.extra = []string{"FAKE_GCLOUD_READONLY=1", "APOLLO_API_KEY=apollo-key-value-123"}
		var all string
		for _, step := range [][]string{{"accounts"}, {"bucket"}, {"secrets"}, {"deploy", "ghcr.io/tetriz-ai/leadscore:v0.1.0"}, {"schedule"}} {
			args := append([]string{"--dry-run"}, step...)
			_, stdout := e.mustRun(args...)
			all += stdout
		}
		for _, line := range e.log() {
			if strings.HasPrefix(line, "MUTATION") {
				t.Errorf("a dry run changed the project: %s", line)
			}
		}
		for _, want := range []string{
			"+ gcloud iam service-accounts create leadscore-run",
			"+ leadscore config set-hosting project=p",
			"+ gcloud auth application-default login --impersonate-service-account leadscore-run@p.iam.gserviceaccount.com",
			"+ gcloud storage buckets create gs://p-leadscore-lease",
			"+ printf %s <hidden> | gcloud secrets versions add apollo-api-key --project p --data-file=-",
			"+ gcloud run jobs deploy leadscore-run",
			"--task-timeout 810s",
			"+ gcloud scheduler jobs create http leadscore-schedule",
		} {
			if !strings.Contains(all, want) {
				t.Errorf("dry-run output lacks %q", want)
			}
		}
		if strings.Contains(all, "apollo-key-value-123") {
			t.Error("a dry run printed a key")
		}
		if data, _ := os.ReadFile(filepath.Join(e.dir, "leadscore.yml")); string(data) != yml {
			t.Errorf("a dry run changed leadscore.yml:\n%s", data)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		hostingBlock := "hosting: { project: p, region: asia-south1, run_account: leadscore-run, receiver_account: leadscore-receiver }\n"
		for _, tt := range []struct {
			name, yml string
			args      []string
			want      string
		}{
			{"no hosting yet", gcpYAML, []string{"bucket"}, "run setup/gcp.sh accounts first"},
			{"no project", gcpYAML, []string{"accounts"}, "pass --project"},
			{"sqlite has no lease bucket", strings.Replace(gcpYAML, `{ type: sheets, spreadsheet: "0123" }`, "{ type: sqlite }", 1) + hostingBlock, []string{"bucket"}, "store.type: sheets"},
			{"lease bucket name taken", gcpYAML + hostingBlock, []string{"bucket"}, "set store.lease_bucket in leadscore.yml to another name"},
			{"leadscore.yml does not load", gcpYAML + hostingBlock + "nonsense: 1\n", []string{"bucket"}, `leadscore.yml does not load: leadscore config get: `},
			{"versions cannot be listed", gcpYAML + hostingBlock, []string{"deploy", "img"}, "cannot list the versions of secret leadscore-config"},
			{"finish-rotation without redeploy", gcpYAML + hostingBlock, []string{"deploy", "img", "--finish-rotation"}, "--finish-rotation goes with redeploy"},
			{"schedule with no cron form", gcpYAML + hostingBlock + "schedule: 7m\n", []string{"schedule"}, "cannot run on Cloud Scheduler"},
			{"runs would overlap", gcpYAML + hostingBlock + "deadline: 14m\n", []string{"deploy", "img"}, "runs would overlap"},
			{"redeploy before deploy", gcpYAML + hostingBlock, []string{"redeploy"}, "hosting.image is not set"},
			{"unknown step", gcpYAML, []string{"nope"}, "unknown step"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				e := newGCPEnv(t, cli, tt.yml)
				e.extra = []string{"FAKE_GCLOUD_PROJECT=active-project", "FAKE_GCLOUD_BUCKET_TAKEN=1", "FAKE_GCLOUD_FAIL_LIST=1"}
				code, _, stderr := e.script(tt.args...)
				if code == 0 || !strings.Contains(stderr, tt.want) {
					t.Errorf("exit %d, stderr %q; want %q", code, stderr, tt.want)
				}
			})
		}
	})
}

// The script's schedule conversion is hosting.ScheduleCron's, and its
// durations are config's, for every form: what one refuses the other refuses.
func TestGCPScriptCronMatchesGo(t *testing.T) {
	bash := needBash(t)
	inputs := []string{"1m", "2m", "5m", "7m", "10m", "15m", "20m", "30m", "45m", "60m", "90m", "1h", "2h", "5h",
		"6h", "12h", "24h", "1d", "2d", "36h", "90s", "60s", "15m30s", "1h30m", "1d12h", "0m", "0.25h", "900000ms", "1.5h"}
	var script strings.Builder
	script.WriteString("source setup/gcp.sh\n")
	for _, in := range inputs {
		script.WriteString("echo \"" + in + "|$(cron_for " + in + " || echo refused)|$(duration_seconds " + in + " || echo bad)\"\n")
	}
	out, err := exec.Command(bash, "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	whole := regexp.MustCompile(`^([0-9]+[dhms])+$`)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		in, gotCron, gotSecs := parts[0], parts[1], parts[2]
		d, err := config.ParseDuration(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		wantCron, err := hosting.ScheduleCron(in)
		if err != nil {
			wantCron = "refused"
		}
		if gotCron != wantCron {
			t.Errorf("cron_for %s = %q, hosting.ScheduleCron = %q", in, gotCron, wantCron)
		}
		want := "bad"
		if whole.MatchString(in) {
			want = strconv.Itoa(int(d / time.Second))
		}
		if gotSecs != want {
			t.Errorf("duration_seconds %s = %s, want %s", in, gotSecs, want)
		}
	}
}

// The names the script uses are the ones Go uses: the fixed resource names
// and each key variable's secret.
func TestGCPScriptNamesMatchGo(t *testing.T) {
	bash := needBash(t)
	out, err := exec.Command(bash, "-c", `source setup/gcp.sh
echo "SERVICE=$SERVICE JOB=$JOB SCHEDULER_JOB=$SCHEDULER_JOB PROXY_REPO=$PROXY_REPO SCHEDULER_ACCOUNT=$SCHEDULER_ACCOUNT"
echo "CONFIG_SECRET=$CONFIG_SECRET CONFIG_VERSION_SECRET=$CONFIG_VERSION_SECRET CONFIG_VERSION_SECRET_VAR=$CONFIG_VERSION_SECRET_VAR"
echo "RECEIVER_SECRET=$RECEIVER_SECRET RECEIVER_SECRET_PREVIOUS=$RECEIVER_SECRET_PREVIOUS SAVE_BUDGET_SECONDS=$SAVE_BUDGET_SECONDS"
for v in $KEY_VARIABLES; do echo "key $v=$(secret_for "$v")"; done`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]string{}
	keys := map[string]string{}
	for _, f := range strings.Fields(strings.ReplaceAll(string(out), "key ", "key:")) {
		k, v, _ := strings.Cut(f, "=")
		if name, ok := strings.CutPrefix(k, "key:"); ok {
			keys[name] = v
			continue
		}
		got[k] = v
	}
	want := map[string]string{
		"SERVICE": hosting.ServiceName, "JOB": hosting.JobName, "SCHEDULER_JOB": hosting.SchedulerJobName,
		"PROXY_REPO": hosting.ProxyRepository, "SCHEDULER_ACCOUNT": hosting.SchedulerAccount,
		"CONFIG_SECRET": hosting.ConfigSecret, "CONFIG_VERSION_SECRET": hosting.ConfigVersionSecret,
		"CONFIG_VERSION_SECRET_VAR": hosting.ConfigVersionVariable,
		"RECEIVER_SECRET":           hosting.ReceiverSecret, "RECEIVER_SECRET_PREVIOUS": hosting.ReceiverSecretPrevious,
		"SAVE_BUDGET_SECONDS": strconv.Itoa(int(config.SaveBudget / time.Second)),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("setup/gcp.sh %s = %q, Go has %q", k, got[k], v)
		}
	}
	if len(keys) != len(hosting.KeySecrets) {
		t.Errorf("script keys %v, Go %v", keys, hosting.KeySecrets)
	}
	for v, secret := range hosting.KeySecrets {
		if keys[v] != secret {
			t.Errorf("setup/gcp.sh stores %s in %q, Go reads %q", v, keys[v], secret)
		}
	}
}

// The Google Cloud example loads, passes config push's checks, and needs no
// file Cloud Run lacks.
func TestGCPExampleConfig(t *testing.T) {
	data, err := os.ReadFile("examples/leadscore.gcp.yml")
	if err != nil {
		t.Fatal(err)
	}
	// setup/gcp.sh accounts adds the hosting block.
	data = append(data, "hosting: { project: team-proj, region: asia-south1 }\n"...)
	c, err := config.Parse(data, "/config", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Store.Type != "sheets" || c.Replies != "receiver" || c.PushesEnabled || c.Store.LeaseBucket != "team-proj-leadscore-lease" {
		t.Errorf("store %q, replies %q, pushes %v, lease bucket %q", c.Store.Type, c.Replies, c.PushesEnabled, c.Store.LeaseBucket)
	}
	if err := hosting.CheckSchedule(c); err != nil {
		t.Error(err)
	}
	if ps := check.CloudRunRefusal(c, func(k string) string { return map[string]string{"CLOUD_RUN_JOB": "x"}[k] }); len(ps) > 0 {
		t.Errorf("Cloud Run would refuse the example: %+v", ps)
	}
}
