package hosting

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func runPath(project, region, kind, name string) string {
	return "/v2/projects/" + esc(project) + "/locations/" + esc(region) + "/" + kind + "/" + esc(name)
}

// Service is what the hosting check reads of the receiver service.
type Service struct {
	// MaxInstances is the most instances the service may run; 0 means no
	// limit was set (Cloud Run's default, 100).
	MaxInstances int
}

// ReceiverService reads the receiver service (Cloud Run Admin API v2).
func (c *Client) ReceiverService(ctx context.Context, project, region string) (*Service, error) {
	var out struct {
		Template struct {
			Scaling struct {
				MaxInstanceCount int `json:"maxInstanceCount"`
			} `json:"scaling"`
		} `json:"template"`
		// A service-level limit, when set, caps every revision.
		Scaling struct {
			MaxInstanceCount int `json:"maxInstanceCount"`
		} `json:"scaling"`
	}
	if err := c.call(ctx, http.MethodGet, "run", runPath(project, region, "services", ServiceName), nil, &out); err != nil {
		return nil, err
	}
	max := out.Template.Scaling.MaxInstanceCount
	if s := out.Scaling.MaxInstanceCount; s > 0 && (max == 0 || s < max) {
		max = s
	}
	return &Service{MaxInstances: max}, nil
}

// Job is what the hosting check reads of the run job.
type Job struct {
	TaskTimeout time.Duration
	// MaxRetries is nil when the job does not say, which leaves Cloud Run's
	// default (3). S0 confirms: an unset maxRetries means retries.
	MaxRetries *int
}

// RunJob reads the run job (Cloud Run Admin API v2).
func (c *Client) RunJob(ctx context.Context, project, region string) (*Job, error) {
	var out struct {
		Template struct {
			Template struct {
				Timeout    string `json:"timeout"`
				MaxRetries *int   `json:"maxRetries"`
			} `json:"template"`
		} `json:"template"`
	}
	if err := c.call(ctx, http.MethodGet, "run", runPath(project, region, "jobs", JobName), nil, &out); err != nil {
		return nil, err
	}
	j := &Job{MaxRetries: out.Template.Template.MaxRetries}
	if t := out.Template.Template.Timeout; t != "" {
		d, err := time.ParseDuration(t) // the API writes durations as seconds: "810s"
		if err != nil {
			return nil, fmt.Errorf("run job %s: task timeout %q is not a duration", JobName, t)
		}
		j.TaskTimeout = d
	}
	return j, nil
}

// invokerRoles are the job-level roles that let an account start the job.
var invokerRoles = map[string]bool{"roles/run.invoker": true, "roles/run.developer": true, "roles/run.admin": true}

// JobInvokers lists the members granted a role on the run job that lets them
// start it. Grants made on the whole project are not seen here.
func (c *Client) JobInvokers(ctx context.Context, project, region string) ([]string, error) {
	var out struct {
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := c.call(ctx, http.MethodGet, "run", runPath(project, region, "jobs", JobName)+":getIamPolicy", nil, &out); err != nil {
		return nil, err
	}
	var members []string
	for _, b := range out.Bindings {
		if invokerRoles[b.Role] {
			members = append(members, b.Members...)
		}
	}
	return members, nil
}

// SchedulerJob is what the hosting check reads of the scheduler job.
type SchedulerJob struct {
	Schedule string
	URI      string // what it calls: the run job's :run endpoint
	Account  string // the account it calls as (its OAuth token's service account)
	Paused   bool
}

// Schedule reads the scheduler job (Cloud Scheduler API v1). S0 confirms that
// Cloud Scheduler is offered in the same region as Cloud Run.
func (c *Client) Schedule(ctx context.Context, project, region string) (*SchedulerJob, error) {
	var out struct {
		Schedule   string `json:"schedule"`
		State      string `json:"state"`
		HTTPTarget struct {
			URI        string `json:"uri"`
			OAuthToken struct {
				ServiceAccountEmail string `json:"serviceAccountEmail"`
			} `json:"oauthToken"`
		} `json:"httpTarget"`
	}
	path := "/v1/projects/" + esc(project) + "/locations/" + esc(region) + "/jobs/" + SchedulerJobName
	if err := c.call(ctx, http.MethodGet, "cloudscheduler", path, nil, &out); err != nil {
		return nil, err
	}
	return &SchedulerJob{
		Schedule: out.Schedule,
		URI:      out.HTTPTarget.URI,
		Account:  out.HTTPTarget.OAuthToken.ServiceAccountEmail,
		Paused:   strings.EqualFold(out.State, "PAUSED"),
	}, nil
}

// JobRunURI is the endpoint the scheduler job calls to start one execution of
// the run job.
func JobRunURI(project, region string) string {
	return "https://run.googleapis.com" + runPath(project, region, "jobs", JobName) + ":run"
}

// ProxyRepositoryExists reports whether the ghcr-proxy Artifact Registry
// repository exists.
func (c *Client) ProxyRepositoryExists(ctx context.Context, project, region string) (bool, error) {
	path := "/v1/projects/" + esc(project) + "/locations/" + esc(region) + "/repositories/" + ProxyRepository
	err := c.call(ctx, http.MethodGet, "artifactregistry", path, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// UsesProxy reports whether an image reference is pulled through ghcr-proxy
// (a release image), as opposed to the private registry's image passed
// directly before release (contracts section 9).
func UsesProxy(image string) bool {
	return strings.HasPrefix(image, "ghcr.io/") || strings.Contains(image, "-docker.pkg.dev/") && strings.Contains(image, "/"+ProxyRepository+"/")
}
