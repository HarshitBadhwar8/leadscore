// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
)

func init() { Register(hostingCheck{}) }

// hostingCheck is the `hosting` check: with a `hosting` block, it
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
	warn := func(id, msg, fix string) {
		out = append(out, Problem{Key: "hosting:" + id, Message: msg, Fix: fix, Warning: true})
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
	redeployFix := "run `setup/gcp.sh redeploy`"
	// wrongAccount names an account a service or job runs as that is not the
	// one setup made for it (an empty want is not checked).
	wrongAccount := func(id, what, got, want string) {
		if want != "" && got != want {
			add(id, fmt.Sprintf("the %s runs as %q, not %s", what, got, want), redeployFix)
		}
	}
	accountOf := func(name string) string {
		if name == "" {
			return ""
		}
		return hosting.AccountEmail(name, p)
	}

	switch svc, err := client.ReceiverService(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("service_missing", "the receiver service "+hosting.ServiceName+" does not exist", deployFix)
	case err != nil:
		unreadable("receiver service", err)
	default:
		if svc.MaxInstances != 1 {
			limit := "no limit"
			if svc.MaxInstances > 0 {
				limit = fmt.Sprint(svc.MaxInstances)
			}
			add("service_instances", fmt.Sprintf("the receiver service may run %s instances; it must run at most 1", limit), redeployFix)
		}
		wrongAccount("service_account", "receiver service", svc.Account, accountOf(hc.ReceiverAccount))
		// The receiver refuses every webhook without its secret.
		if ref, ok := svc.SecretEnv["LEADSCORE_RECEIVER_SECRET"]; !ok || ref.Secret != hosting.ReceiverSecret {
			add("receiver_secret", "the receiver service has no LEADSCORE_RECEIVER_SECRET from "+hosting.ReceiverSecret+", so it refuses every webhook",
				"run `setup/gcp.sh secrets`, then `setup/gcp.sh redeploy`")
		}
		if _, ok := svc.SecretEnv["LEADSCORE_RECEIVER_SECRET_PREVIOUS"]; ok {
			warn("receiver_secret_previous", "the receiver still accepts the previous receiver secret",
				"once every Apollo workflow sends the new secret, run `setup/gcp.sh redeploy --finish-rotation` (README, \"Rotating a secret\")")
		}
	}

	jobExists := false
	switch job, err := client.RunJob(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("job_missing", "the run job "+hosting.JobName+" does not exist", deployFix)
	case err != nil:
		unreadable("run job", err)
	default:
		jobExists = true
		wrongAccount("job_account", "run job", job.Account, accountOf(hc.RunAccount))
		if ref := job.SecretEnv[hosting.ConfigVersionVariable]; ref.Secret != hosting.ConfigVersionSecret || ref.Version != "latest" {
			add("job_config_version", "the run job does not read "+hosting.ConfigVersionVariable+" from "+hosting.ConfigVersionSecret+
				":latest, so runs cannot record which configuration they used", redeployFix)
		}
		if want := hosting.TaskTimeout(c.Deadline); job.TaskTimeout != want {
			add("job_timeout", fmt.Sprintf("the run job's task timeout is %s; it must be the deadline plus the save budget, %s", job.TaskTimeout, want),
				redeployFix)
		}
		if job.MaxRetries == nil || *job.MaxRetries != 0 {
			retries := "Cloud Run's default"
			if job.MaxRetries != nil {
				retries = fmt.Sprint(*job.MaxRetries)
			}
			add("job_retries", "the run job retries a failed run ("+retries+"); retries must be 0, since the next scheduled run is the retry",
				redeployFix)
		}
	}

	switch sched, err := client.Schedule(ctx, p, r); {
	case hosting.IsNotFound(err):
		add("schedule_missing", "the scheduler job "+hosting.SchedulerJobName+" does not exist, so nothing starts runs", "run `setup/gcp.sh schedule`")
	case err != nil:
		unreadable("scheduler job", err)
	case sched.URI != hosting.JobRunURI(p, r) || sched.Account == "":
		add("scheduler_account", "the scheduler job does not start "+hosting.JobName+" with its own service account", "run `setup/gcp.sh schedule`")
	default:
		if sched.Paused {
			warn("schedule_paused", "the scheduler job is paused, so no run starts", "resume it: gcloud scheduler jobs resume "+
				hosting.SchedulerJobName+" --location "+r+" --project "+p)
		}
		if !jobExists {
			break
		}
		members, err := client.JobInvokers(ctx, p, r)
		if err != nil {
			unreadable("run job's access policy", err)
			break
		}
		if !slices.Contains(members, "serviceAccount:"+sched.Account) {
			add("scheduler_account", "the scheduler's account "+sched.Account+" may not run "+hosting.JobName+" (no Cloud Run Invoker on the job)",
				"run `setup/gcp.sh schedule`")
		}
		if slices.Contains(members, "allUsers") || slices.Contains(members, "allAuthenticatedUsers") {
			add("job_public", "anyone may start "+hosting.JobName+" (allUsers or allAuthenticatedUsers may invoke it)",
				"remove that grant: gcloud run jobs remove-iam-policy-binding "+hosting.JobName+" --region "+r+" --project "+p+
					" --member allUsers --role roles/run.invoker (and likewise allAuthenticatedUsers)")
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

	// The bundle the next run reads; the version number the job reads with it;
	// and the version the last run recorded.
	switch _, version, err := client.AccessSecret(ctx, p, hosting.ConfigSecret); {
	case hosting.IsNotFound(err):
		add("config_missing", "Secret Manager holds no "+hosting.ConfigSecret+" version, so runs have no configuration", "run `leadscore config push`")
	case err != nil:
		unreadable("secret "+hosting.ConfigSecret, err)
	default:
		recorded, _, err := client.AccessSecret(ctx, p, hosting.ConfigVersionSecret)
		switch {
		case err != nil && !hosting.IsNotFound(err):
			unreadable("secret "+hosting.ConfigVersionSecret, err)
		case err != nil || strings.TrimSpace(string(recorded)) != version:
			add("config_interrupted", "the latest "+hosting.ConfigSecret+" is version "+version+", but "+hosting.ConfigVersionSecret+
				" does not name it: a config push was interrupted, so runs would record the wrong version", "run `leadscore config push` again")
		}
		if env.Model == nil {
			break
		}
		// A run reads the latest bundle and its number, so a difference means
		// no run has started since the push (the run account cannot read when
		// a version was made, so this stays a warning; the checks above catch
		// a job that cannot record it).
		if used := env.Model.StateValue("config_version"); used != version {
			if used == "" {
				used = "none"
			}
			warn("config_version", fmt.Sprintf("the latest %s is version %s, but the last run recorded version %s: no run has started since the push",
				hosting.ConfigSecret, version, used), "wait for the next run; if it does not come, check the scheduler job")
		}
	}
	return out
}
