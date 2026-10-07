package leadscore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/receiver"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// TestLiveCloudRun is the LEADSCORE_LIVE_CLOUDRUN live check. It sets
// a throwaway Google Cloud project up with setup/gcp.sh exactly as the README
// does, then checks what only real Cloud Run can show:
//
//   - a run started by Cloud Scheduler as its own account runs longer than 3
//     minutes (a large synthetic Leads tab, no test-only flag) and is not cut off;
//   - a webhook burst above 60 a minute while that run writes the Sheet loses
//     no event;
//   - a redeploy during a second burst loses no event (requests the old or new
//     revision refuses with 5xx are sent again, as Apollo retries them);
//   - the run job and the receiver read and write the shared Sheet as their
//     own service accounts, and a local command reaches it as the run account;
//   - the hosting check passes.
//
// It prints the run's length and the measures the README's cost section needs.
//
// Variables: LEADSCORE_LIVE_CLOUDRUN=1; LEADSCORE_LIVE_PROJECT (a billed,
// throwaway project, never one a real install uses);
// LEADSCORE_LIVE_IMAGE (the image to deploy); optional LEADSCORE_LIVE_REGION
// (asia-south1) and LEADSCORE_LIVE_ROWS (synthetic leads, 60000).
// LEADSCORE_LIVE_DENY_PROJECTS is required: a comma-separated list of project
// ids the test refuses to touch. Set it to your real projects (for example
// leadscore-dev) so a mistyped LEADSCORE_LIVE_PROJECT fails before anything
// is created. The test does not run while it is empty.
//
// Before it: `gcloud auth login --enable-gdrive-access` as a person who owns
// the project; the Cloud Run service agent of the project may pull the image
// (before release, Artifact Registry Reader on leadscore-dev's repository).
// `setup/gcp.sh accounts` opens a browser for the impersonated login. The test
// pauses the scheduler at the end; delete the project afterwards.
func TestLiveCloudRun(t *testing.T) {
	if os.Getenv("LEADSCORE_LIVE_CLOUDRUN") == "" {
		t.Skip("set LEADSCORE_LIVE_CLOUDRUN=1, LEADSCORE_LIVE_PROJECT and LEADSCORE_LIVE_IMAGE to run against real Google Cloud")
	}
	project, image := os.Getenv("LEADSCORE_LIVE_PROJECT"), os.Getenv("LEADSCORE_LIVE_IMAGE")
	if project == "" || image == "" {
		t.Fatal("LEADSCORE_LIVE_PROJECT and LEADSCORE_LIVE_IMAGE are required")
	}
	deny := os.Getenv("LEADSCORE_LIVE_DENY_PROJECTS")
	if strings.TrimSpace(strings.ReplaceAll(deny, ",", "")) == "" {
		t.Fatal("set LEADSCORE_LIVE_DENY_PROJECTS to the projects the live check must never touch (for example leadscore-dev)")
	}
	if deniedProject(project, deny) {
		t.Fatalf("project %s is in LEADSCORE_LIVE_DENY_PROJECTS; run the live check in a throwaway project", project)
	}
	region := envOr("LEADSCORE_LIVE_REGION", "asia-south1")
	rows, err := strconv.Atoi(envOr("LEADSCORE_LIVE_ROWS", "60000"))
	if err != nil || rows <= 0 {
		t.Fatal("LEADSCORE_LIVE_ROWS must be a positive number")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	cli := buildCLI(t)
	repo, _ := os.Getwd()
	dir := t.TempDir()
	marker := fmt.Sprintf("live%d", time.Now().Unix())
	yml := fmt.Sprintf(`version: 1
store: { type: sheets }
sources: [ { id: leads, type: sheetsource, tabs: [Leads] } ]
replies: receiver
receiver: { visit_events: [visit_pricing] }
# The whole synthetic sheet in one run: this is what makes the run long.
ingest_chunk_rows: %d
schedule: 15m
deadline: 12m
`, rows)
	writeFile(t, filepath.Join(dir, "leadscore.yml"), yml)
	rubric, _ := os.ReadFile(filepath.Join(repo, "testdata/compose/rubric.yml"))
	writeFile(t, filepath.Join(dir, "rubric.yml"), string(rubric))
	cfgPath := filepath.Join(dir, "leadscore.yml")

	sh := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(repo, "setup/gcp.sh"), "--config", cfgPath}, args...)...)
		// The person's own keys stay out: this install uses none, and the
		// script would otherwise upload them.
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "APOLLO_API_KEY=") && !strings.HasPrefix(kv, "HUBSPOT_TOKEN=") {
				env = append(env, kv)
			}
		}
		cmd.Dir, cmd.Env = dir, append(env, "LEADSCORE="+cli)
		cmd.Stdin = os.Stdin // the impersonated login may ask
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("setup/gcp.sh %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	ls := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, cli, append([]string{"--config", cfgPath}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("leadscore %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "gcloud", append(args, "--project", project)...).Output()
		if err != nil {
			t.Fatalf("gcloud %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	// The Google Cloud runbook, steps 5 to 11.
	sh("accounts", "--project", project, "--region", region)
	sh("bucket")
	ls("setup", "sheet")
	c, err := config.Load(config.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	fillLeads(ctx, t, c.Store.Spreadsheet, rows)
	sh("secrets")
	ls("config", "push")
	// Registered before anything that can start runs, so a failure below
	// still stops the schedule.
	t.Cleanup(func() {
		exec.Command("gcloud", "scheduler", "jobs", "pause", hosting.SchedulerJobName,
			"--location", region, "--project", project).Run()
	})
	sh("deploy", image)
	sh("schedule")
	url := gc("run", "services", "describe", hosting.ServiceName, "--region", region, "--format", "value(status.url)")
	secret := gc("secrets", "versions", "access", "latest", "--secret", hosting.ReceiverSecret)

	// A run from Cloud Scheduler, now rather than at the next tick.
	before := executions(t, gc, region)
	gc("scheduler", "jobs", "run", hosting.SchedulerJobName, "--location", region)
	exec1 := waitExecution(ctx, t, gc, region, before, func(e execution) bool { return e.Status.StartTime != "" })
	t.Logf("execution %s started", exec1.Metadata.Name)

	// Burst 1: 150 visits at 100 a minute while the run writes.
	rep1 := postBurst(ctx, url, secret, liveVisits(t, marker+"a", 150), 100)
	t.Logf("burst during the run: %+v", rep1)

	// Burst 2, with a redeploy in the middle of it.
	var wg sync.WaitGroup
	var rep2 burstReport
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep2 = postBurst(ctx, url, secret, liveVisits(t, marker+"b", 150), 100)
	}()
	time.Sleep(20 * time.Second)
	sh("redeploy")
	wg.Wait()
	t.Logf("burst during a redeploy: %+v", rep2)
	for _, r := range []burstReport{rep1, rep2} {
		if r.GaveUp > 0 {
			t.Errorf("%d requests never got a 2xx after %d tries", r.GaveUp, maxTries)
		}
	}

	done := waitExecution(ctx, t, gc, region, before, func(e execution) bool { return e.Status.CompletionTime != "" })
	if done.Status.SucceededCount != 1 {
		t.Errorf("the run did not succeed: %+v", done.Status)
	}
	start, _ := time.Parse(time.RFC3339, done.Status.StartTime)
	end, _ := time.Parse(time.RFC3339, done.Status.CompletionTime)
	length := end.Sub(start)
	t.Logf("MEASURE run length with %d leads: %s (task timeout %s)", rows, length, hosting.TaskTimeout(c.Deadline))
	if length < 3*time.Minute {
		t.Errorf("the run took %s; the check needs one longer than 3 minutes: raise LEADSCORE_LIVE_ROWS", length)
	}

	// No event lost, read back as the run account.
	store, events := openSheets(ctx, t, c)
	for _, m := range []string{marker + "a", marker + "b"} {
		got, err := storedMarkers(ctx, events, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 150 {
			t.Errorf("burst %s: %d of 150 events stored", m, len(got))
		}
	}
	m, err := codec.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Ranked) == 0 {
		t.Error("the run wrote no Ranked rows")
	}
	if !strings.Contains(ls("status"), "last_result") {
		t.Error("leadscore status, as the run account, did not read Health")
	}
	for _, chk := range check.All() {
		if chk.Name() == "hosting" {
			for _, p := range chk.Run(ctx, check.Env{Config: c, Model: m, Store: store}) {
				t.Errorf("hosting check: %s: %s", p.Key, p.Message)
			}
		}
	}

	// A config push takes effect at the next run with no redeploy, and the
	// run records the version it read.
	edited := strings.Replace(string(rubric), "name: Nurture list", "name: Nurture list (live)", 1)
	writeFile(t, filepath.Join(dir, "rubric.yml"), edited)
	r2, err := rules.Compile([]byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	pushed := regexp.MustCompile(`version (\d+);`).FindStringSubmatch(ls("config", "push"))
	if pushed == nil {
		t.Fatal("config push printed no version")
	}
	before = executions(t, gc, region)
	gc("scheduler", "jobs", "run", hosting.SchedulerJobName, "--location", region)
	if e := waitExecution(ctx, t, gc, region, before, func(e execution) bool { return e.Status.CompletionTime != "" }); e.Status.SucceededCount != 1 {
		t.Errorf("the run after the push did not succeed: %+v", e.Status)
	}
	health, err := store.ReadTable(ctx, model.TableHealth)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range health {
		if row["kind"] == "result" && row["key"] == "rubric_version" && row["value"] != r2.Version() {
			t.Errorf("the run used rubric %s, not the pushed %s", row["value"], r2.Version())
		}
	}
	if m, err := codec.Load(ctx, store); err != nil {
		t.Fatal(err)
	} else if got := m.StateValue("config_version"); got != pushed[1] {
		t.Errorf("the run recorded config_version %q, want the pushed %s", got, pushed[1])
	}
}

// deniedProject reports whether project is in deny, a comma-separated list of
// project ids. Spaces around each id and empty entries are ignored.
func deniedProject(project, deny string) bool {
	for _, p := range strings.Split(deny, ",") {
		if strings.TrimSpace(p) == project {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fillLeads writes n synthetic leads into the Leads tab, as the run account.
func fillLeads(ctx context.Context, t *testing.T, spreadsheet string, n int) {
	t.Helper()
	svc, err := sheets.Connect(ctx, api.Config{})
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 10_000
	for from := 0; from < n; from += chunk {
		var values [][]any
		for i := from; i < min(n, from+chunk); i++ {
			values = append(values, syntheticLead(i))
		}
		rng := fmt.Sprintf("Leads!A%d", from+2)
		_, err := svc.Sheets.Spreadsheets.Values.Update(spreadsheet, rng, &sheetsapi.ValueRange{Values: values}).
			ValueInputOption("RAW").Context(ctx).Do()
		if err != nil {
			t.Fatalf("writing leads %d+: %v", from, err)
		}
	}
}

// syntheticLead is one row under sheets.LeadsHeaders: Email, Full name,
// Title, Company, Company domain, LinkedIn URL. Ten leads share a company.
func syntheticLead(i int) []any {
	co := i / 10
	return []any{
		fmt.Sprintf("lead%06d@co%05d.example.com", i, co),
		fmt.Sprintf("Lead %d", i),
		[]string{"Head of Operations", "Director of Logistics", "Clerk", "VP Supply Chain"}[i%4],
		fmt.Sprintf("Company %d", co),
		fmt.Sprintf("co%05d.example.com", co),
		fmt.Sprintf("https://www.linkedin.com/in/lead-%06d", i),
	}
}

func openSheets(ctx context.Context, t *testing.T, c *config.Config) (api.Backend, api.EventLog) {
	t.Helper()
	open, ok := api.BackendFactory("sheets")
	if !ok {
		t.Fatal("the sheets store is not registered")
	}
	b, e, err := open(c.Store.Block)
	if err != nil {
		t.Fatal(err)
	}
	return b, e
}

type execution struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
		SucceededCount int    `json:"succeededCount"`
		FailedCount    int    `json:"failedCount"`
	} `json:"status"`
}

func executions(t *testing.T, gc func(...string) string, region string) []execution {
	t.Helper()
	var out []execution
	if err := json.Unmarshal([]byte(gc("run", "jobs", "executions", "list", "--job", hosting.JobName,
		"--region", region, "--format", "json")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// waitExecution waits for an execution not in before that satisfies ok.
func waitExecution(ctx context.Context, t *testing.T, gc func(...string) string, region string, before []execution, ok func(execution) bool) execution {
	t.Helper()
	known := map[string]bool{}
	for _, e := range before {
		known[e.Metadata.Name] = true
	}
	for {
		for _, e := range executions(t, gc, region) {
			if !known[e.Metadata.Name] && ok(e) {
				return e
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the run job")
		case <-time.After(10 * time.Second):
		}
	}
}

// liveVisits are n identified website-visit bodies built from the golden
// body testdata/events/apollo_visit_identified.json, each a distinct person at
// the marker's domain, so each is one event that must be stored.
func liveVisits(t *testing.T, marker string, n int) [][]byte {
	t.Helper()
	golden, err := os.ReadFile("testdata/events/apollo_visit_identified.json")
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, n)
	for i := range out {
		var body map[string]any
		if err := json.Unmarshal(golden, &body); err != nil {
			t.Fatal(err)
		}
		delete(body, "provisional")
		body["visited_at"] = time.Now().UTC().Format(time.RFC3339)
		contact := body["contact"].(map[string]any)
		contact["id"] = fmt.Sprintf("ct-%s-%d", marker, i)
		contact["email"] = fmt.Sprintf("visitor%d@%s.example", i, marker)
		delete(contact, "linkedin_url") // one identity per visitor
		body["account"] = map[string]any{"domain": marker + ".example", "name": "Live"}
		if out[i], err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// maxTries is how often a request is sent before giving up: Apollo retries a
// 5xx or a failed connection (S0 confirms how often), never a 4xx.
const maxTries = 6

// retryPause is the wait before the nth retry: n times this.
var retryPause = 2 * time.Second

type burstReport struct {
	Sent, Retried, GaveUp int
}

// postBurst posts bodies to url's /apollo/visit at perMinute, each with the
// secret header, sending any that gets no 2xx again after a pause.
func postBurst(ctx context.Context, url, secret string, bodies [][]byte, perMinute int) burstReport {
	var mu sync.Mutex
	var rep burstReport
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 30 * time.Second}
	gap := time.Minute / time.Duration(perMinute)
	for _, body := range bodies {
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			for try := 1; ; try++ {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/apollo/visit", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Leadscore-Secret", secret)
				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
				if err == nil && resp.StatusCode/100 == 2 {
					mu.Lock()
					rep.Sent++
					mu.Unlock()
					return
				}
				refused := err == nil && resp.StatusCode/100 == 4
				if refused || try == maxTries || ctx.Err() != nil {
					mu.Lock()
					rep.GaveUp++
					mu.Unlock()
					return
				}
				mu.Lock()
				rep.Retried++
				mu.Unlock()
				time.Sleep(time.Duration(try) * retryPause)
			}
		}(body)
		time.Sleep(gap)
	}
	wg.Wait()
	return rep
}

// storedMarkers returns the distinct contact emails at the marker's domain
// among every stored visit event.
func storedMarkers(ctx context.Context, events api.EventLog, marker string) (map[string]bool, error) {
	found := map[string]bool{}
	var cursor api.Cursor
	for {
		batch, next, err := events.ReadEvents(ctx, cursor)
		if err != nil {
			return nil, err
		}
		for _, e := range batch {
			var body struct {
				Contact struct {
					Email string `json:"email"`
				} `json:"contact"`
			}
			if json.Unmarshal(e.Body, &body) == nil && strings.HasSuffix(body.Contact.Email, "@"+marker+".example") {
				found[body.Contact.Email] = true
			}
		}
		if len(batch) == 0 || next == cursor {
			return found, nil
		}
		cursor = next
	}
}

// The live check's burst and read-back code, run against the real receiver
// handler on SQLite behind a flaky proxy, so it is known to work (and to
// catch a lost event) before anyone spends a billed project on it.
func TestLiveCloudRunHelpersOnFakes(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "leadscore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const secret = "live-check-secret-123"
	h := receiver.NewHandler(receiver.Options{Store: store, Events: store, Getenv: func(k string) string {
		if k == receiver.SecretVar {
			return secret
		}
		return ""
	}})
	// Every third request first gets a 503, as a revision shutting down
	// answers; the burst must send it again.
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		first := !seen[string(body)]
		seen[string(body)] = true
		n := len(seen)
		mu.Unlock()
		if first && n%3 == 0 {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	defer func(old time.Duration) { retryPause = old }(retryPause)
	retryPause = 10 * time.Millisecond
	ctx := context.Background()
	rep := postBurst(ctx, srv.URL, secret, liveVisits(t, "fakeburst", 30), 6000)
	h.Close()
	if rep.Sent != 30 || rep.Retried != 10 || rep.GaveUp != 0 {
		t.Errorf("burst report %+v, want 30 sent, 10 retried", rep)
	}
	got, err := storedMarkers(ctx, store, "fakeburst")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 30 {
		t.Errorf("%d of 30 events read back", len(got))
	}
	// A refused request (401) is not sent again and does not count as sent.
	if rep := postBurst(ctx, srv.URL, "wrong", liveVisits(t, "other", 1), 6000); rep.GaveUp != 1 || rep.Retried != 0 {
		t.Errorf("a refused request must not count as sent: %+v", rep)
	}

	if got := syntheticLead(12); got[0] != "lead000012@co00001.example.com" || got[4] != "co00001.example.com" || len(got) != len(sheets.LeadsHeaders) {
		t.Errorf("synthetic lead %v", got)
	}
}

// TestDeniedProject runs without Google Cloud: the deny-list guard parses its
// comma-separated list as the test's comment says.
func TestDeniedProject(t *testing.T) {
	for _, tt := range []struct {
		project, deny string
		want          bool
	}{
		{"scratch-1", "", false},
		{"scratch-1", "prod-a,prod-b", false},
		{"prod-b", "prod-a,prod-b", true},
		{"prod-b", " prod-a , prod-b ", true},
		{"prod", "prod-a,,", false},
	} {
		if got := deniedProject(tt.project, tt.deny); got != tt.want {
			t.Errorf("deniedProject(%q, %q) = %v, want %v", tt.project, tt.deny, got, tt.want)
		}
	}
}
