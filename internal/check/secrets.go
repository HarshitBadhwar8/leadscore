package check

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/hosting"
)

func init() { Register(secrets{getenv: os.Getenv}) }

// secrets fails when a configured adapter's key variable is missing (section
// 10). On a hosted install, outside Cloud Run, an empty variable is read from
// Secret Manager instead (contracts section 3), so there the check fails only
// when that read fails.
type secrets struct {
	getenv  func(string) string
	connect hosting.Connector // nil: Google's standard credentials
}

func (secrets) Name() string { return "secrets" }
func (secrets) InRun() bool  { return true }

func (s secrets) Run(ctx context.Context, env Env) []Problem {
	if env.Config == nil {
		return nil
	}
	// A local command on a hosted install reads empty keys from Secret Manager
	// as the run account. Inside Cloud Run the variables are filled from
	// Secret Manager by the service and job, so they are checked as usual.
	fromSecretManager := env.Config.Hosted() && !hosting.InCloudRun(s.getenv)
	// Variable -> the config places that need it, so one problem names every user.
	needs := env.Config.KeyVariables()

	vars := make([]string, 0, len(needs))
	for v := range needs {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	var out []Problem
	for _, v := range vars {
		if strings.TrimSpace(s.getenv(v)) != "" {
			continue
		}
		users := needs[v]
		need := strings.Join(users, ", ") + " need" + plural(len(users)) + " it"
		if fromSecretManager {
			secret := hosting.KeySecrets[v]
			err := s.readSecret(ctx, env.Config.Hosting.Project, secret)
			if err == nil {
				continue
			}
			out = append(out, Problem{
				Key:     "secret_missing:" + v,
				Message: v + " is not set here and Secret Manager secret " + secret + " cannot be read as the run account (" + err.Error() + "); " + need,
				Fix:     "add it with `setup/gcp.sh secrets`; `setup/gcp.sh accounts` gives the run account access",
			})
			continue
		}
		out = append(out, Problem{
			Key:     "secret_missing:" + v,
			Message: v + " is not set; " + need,
			Fix:     "add " + v + " to Secret Manager or .env",
		})
	}
	return out
}

// readSecret reads a key secret, as the run account, to see that it can be.
func (s secrets) readSecret(ctx context.Context, project, secret string) error {
	if secret == "" {
		return errors.New("no secret holds this key")
	}
	client, err := s.connect.Open(ctx)
	if err != nil {
		return err
	}
	_, err = hosting.ReadKey(ctx, client, project, secret)
	return err
}

func plural(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}
