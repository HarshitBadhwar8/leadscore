// Package hosting is the Google Cloud side of leadscore (contracts sections 3
// and 9): the fixed resource names, the schedule's cron form, Secret Manager
// reads and writes (`config push` and the keys a local command reads on a
// hosted install), and the reads of the Cloud Run service and job, the Cloud
// Scheduler job and the Artifact Registry proxy that the `hosting` check makes.
// Creating and changing those resources is `setup/gcp.sh`'s job, never this
// package's.
package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// The fixed resource names (contracts section 3, `hosting`).
const (
	ServiceName            = "leadscore-receiver"
	JobName                = "leadscore-run"
	SchedulerJobName       = "leadscore-schedule"
	ProxyRepository        = "ghcr-proxy"
	ConfigSecret           = "leadscore-config"
	ReceiverSecret         = "receiver-secret"
	ReceiverSecretPrevious = "receiver-secret-previous"
	// SchedulerAccount is the account Cloud Scheduler starts the job as
	// (contracts section 9.1 step 11).
	SchedulerAccount = "leadscore-scheduler"
	// ConfigVersionVariable carries the bundle's Secret Manager version into
	// the run job (contracts section 3); setup/gcp.sh deploy sets it.
	ConfigVersionVariable = "LEADSCORE_CONFIG_VERSION"
)

// KeySecrets names the Secret Manager secret holding each adapter key
// variable (config.AdapterKeyVariables).
var KeySecrets = map[string]string{
	"APOLLO_API_KEY": "apollo-api-key",
	"HUBSPOT_TOKEN":  "hubspot-token",
}

// InCloudRun reports whether this process runs on Cloud Run (a service sets
// K_SERVICE, a job CLOUD_RUN_JOB).
func InCloudRun(getenv func(string) string) bool {
	return getenv("K_SERVICE") != "" || getenv("CLOUD_RUN_JOB") != ""
}

// AccountEmail is a service account's email: a bare name gets
// @<project>.iam.gserviceaccount.com (contracts section 9).
func AccountEmail(name, project string) string {
	if strings.Contains(name, "@") || project == "" {
		return name
	}
	return name + "@" + project + ".iam.gserviceaccount.com"
}

// callTimeout bounds every Google call.
const callTimeout = 30 * time.Second

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Client calls the Google Cloud APIs leadscore reads and writes outside a
// Sheet: Secret Manager, Cloud Run, Cloud Scheduler and Artifact Registry.
type Client struct {
	http *http.Client
	base string // tests: every API under base/<api>; empty for Google's own hosts
}

// Connect builds a client from a block with the two test keys of contracts
// section 3. With `base_url` set, every API points there and skips
// authentication; only tests set it, together with `_http_client`, so a
// `base_url` without a client is refused: it would send the team's keys to
// whatever address it names. Otherwise `_http_client`, when set, is an
// already-authenticated client; else the client signs in with Google's
// standard credentials (on a person's machine, the run account through
// `gcloud auth application-default login --impersonate-service-account`). A
// nil block is Google's standard credentials.
func Connect(ctx context.Context, cfg api.Config) (*Client, error) {
	base, _ := cfg["base_url"].(string)
	base = strings.TrimRight(base, "/")
	client, _ := cfg["_http_client"].(*http.Client)
	switch {
	case base != "":
		if client == nil {
			return nil, errors.New("`base_url` is for tests only and needs a test HTTP client")
		}
		return &Client{http: client, base: base}, nil
	case client != nil:
		return &Client{http: client}, nil
	}
	hc, _, err := htransport.NewClient(ctx, option.WithScopes(cloudPlatformScope))
	if err != nil {
		return nil, fmt.Errorf("signing in to Google Cloud (run `setup/gcp.sh accounts`, which ends with the impersonated login): %w", err)
	}
	return &Client{http: hc}, nil
}

// APIError is a Google API answer other than 2xx. Detail keeps only the
// machine-issued fields of the body (logredact.VendorErrorDetail).
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d (%s)", e.Status, e.Detail)
}

// IsNotFound reports whether err is a 404: the resource does not exist.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (c *Client) endpoint(service string) string {
	if c.base != "" {
		return c.base + "/" + service
	}
	return "https://" + service + ".googleapis.com"
}

// call sends one request to a Google API and decodes a 2xx JSON answer into
// out. Errors name the API and path only, never a body or a secret.
func (c *Client) call(ctx context.Context, method, service, path string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	u := c.endpoint(service) + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", service, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is already named below
		}
		return fmt.Errorf("%s %s: %w", service, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s %s: reading the answer: %w", service, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %w", service, path,
			&APIError{Status: resp.StatusCode, Detail: logredact.VendorErrorDetail(data)})
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer: %w", service, path, err)
	}
	return nil
}

// lastSegment returns what follows the last "/" of a resource name: the
// version number of projects/p/secrets/s/versions/7.
func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

func esc(s string) string { return url.PathEscape(s) }
