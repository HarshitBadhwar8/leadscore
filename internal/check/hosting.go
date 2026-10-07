package check

import (
	"context"
	"fmt"
	"slices"

	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
)

func init() { Register(hostingCheck{}) }

// hostingCheck is the `hosting` check (section 10): with a `hosting` block, it
// reads, as the run account, that setup/gcp.sh made every Google Cloud piece
// and made it right. It runs only from doctor, never inside a run.
type hostingCheck struct {
	connect hosting.Connector // nil: Google's standard credentials
}

func (hostingCheck) Name() string { return "hosting" }
func (hostingCheck) InRun() bool  { return false }

func (h hostingCheck) Run(ctx context.Context, env Env) []Problem {
	c := env.Config
	if c == nil || c.Hosting == nil {
		return nil
	}
	var out []Problem
	add := func(id, msg, fix string) {
		out = append(out, Problem{Key: "hosting:" + id, Message: msg, Fix: fix})
	}
	// Needs no Google call: a run must end before the next one starts.
	if err := hosting.CheckSchedule(c); err != nil {
		add("schedule", err.Error(), "change schedule or deadline in leadscore.yml, `leadscore config push`, then `setup/gcp.sh schedule`")
	}
	hc := c.Hosting
	if hc.Project == "" || hc.Region == "" {
		add("config", "hosting.project and hosting.region must both be set", "run `setup/gcp.sh accounts`")
		return out
	}
	client, err := h.connect.Open(ctx)
	if err != nil {
		add("api", err.Error(), "run `setup/gcp.sh accounts`, which ends with the impersonated login")
		return out
	}
	p, r := hc.Project, hc.Region
	unreadable := func(what string, err error) {
		add("unreadable:"+what, "cannot read the "+what+" as the run account: "+err.Error(),
			"run `setup/gcp.sh accounts`, which grants the run account its viewer roles")
	}
	deployFix := "run `setup/gcp.sh deploy <image>`"

	switch svc, err := client.ReceiverService(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("service_missing", "the receiver service "+hosting.ServiceName+" does not exist", deployFix)
	case err != nil:
		unreadable("receiver service", err)
	case svc.MaxInstances != 1:
		limit := "no limit"
		if svc.MaxInstances > 0 {
			limit = fmt.Sprint(svc.MaxInstances)
		}
		add("service_instances", fmt.Sprintf("the receiver service may run %s instances; it must run at most 1", limit),
			"run `setup/gcp.sh redeploy`")
	}

	jobExists := false
	switch job, err := client.RunJob(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("job_missing", "the run job "+hosting.JobName+" does not exist", deployFix)
	case err != nil:
		unreadable("run job", err)
	default:
		jobExists = true
		if want := hosting.TaskTimeout(c.Deadline); job.TaskTimeout != want {
			add("job_timeout", fmt.Sprintf("the run job's task timeout is %s; it must be the deadline plus the save budget, %s", job.TaskTimeout, want),
				"run `setup/gcp.sh redeploy`")
		}
		if job.MaxRetries == nil || *job.MaxRetries != 0 {
			retries := "Cloud Run's default"
			if job.MaxRetries != nil {
				retries = fmt.Sprint(*job.MaxRetries)
			}
			add("job_retries", "the run job retries a failed run ("+retries+"); retries must be 0, since the next scheduled run is the retry",
				"run `setup/gcp.sh redeploy`")
		}
	}

	switch sched, err := client.Schedule(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("schedule_missing", "the scheduler job "+hosting.SchedulerJobName+" does not exist, so nothing starts runs", "run `setup/gcp.sh schedule`")
	case err != nil:
		unreadable("scheduler job", err)
	case sched.URI != hosting.JobRunURI(p, r) || sched.Account == "":
		add("scheduler_account", "the scheduler job does not start "+hosting.JobName+" with its own service account", "run `setup/gcp.sh schedule`")
	case jobExists:
		members, err := client.JobInvokers(ctx, p, r)
		if err != nil {
			unreadable("run job's access policy", err)
		} else if !slices.Contains(members, "serviceAccount:"+sched.Account) {
			add("scheduler_account", "the scheduler's account "+sched.Account+" may not run "+hosting.JobName+" (no Cloud Run Invoker on the job)",
				"run `setup/gcp.sh schedule`")
		}
	}

	if hosting.UsesProxy(hc.Image) {
		switch ok, err := client.ProxyRepositoryExists(ctx, p, r); {
		case err != nil:
			unreadable("Artifact Registry repository "+hosting.ProxyRepository, err)
		case !ok:
			add("proxy_missing", "the Artifact Registry repository "+hosting.ProxyRepository+" that pulls the image from ghcr.io does not exist",
				deployFix)
		}
	}

	// The bundle the next run reads, against the one the last run recorded.
	switch _, version, err := client.AccessSecret(ctx, p, hosting.ConfigSecret); {
	case hosting.IsNotFound(err):
		add("config_missing", "Secret Manager holds no "+hosting.ConfigSecret+" version, so runs have no configuration", "run `leadscore config push`")
	case err != nil:
		unreadable("secret "+hosting.ConfigSecret, err)
	case env.Model != nil:
		// S0 confirms (open question): the job mounts the latest bundle, but
		// LEADSCORE_CONFIG_VERSION is fixed when the job is deployed.
		if used := env.Model.StateValue("config_version"); used != version {
			if used == "" {
				used = "none"
			}
			add("config_version", fmt.Sprintf("the latest %s is version %s, but the last run recorded version %s", hosting.ConfigSecret, version, used),
				"`leadscore config push` uploads the files; `setup/gcp.sh redeploy` makes runs record the latest version")
		}
	}
	return out
}
