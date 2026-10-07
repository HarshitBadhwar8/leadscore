package hosting

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// Connector opens a Client; nil means Connect with Google's standard
// credentials. Tests pass one that points at a fake.
type Connector func(ctx context.Context) (*Client, error)

// Open opens a client.
func (f Connector) Open(ctx context.Context) (*Client, error) {
	if f == nil {
		return Connect(ctx, nil)
	}
	return f(ctx)
}

// LoadKeys fills, from Secret Manager, the empty key variables that this
// configuration's adapters need (contracts section 3, "Keys on Google Cloud").
// It does nothing unless hosting.project is set, and nothing inside Cloud Run,
// where the service and job get their keys as environment variables from
// secret references. Only the keys a configured adapter needs are read, as
// the run account (the impersonated login setup/gcp.sh ends with). Each value
// is masked in logs (logredact) before it is set, so nothing can log it.
//
// A key that cannot be read is left empty and named in the returned error; the
// `secrets` check then reports it, so a caller may go on without it. With
// only given, just those variables are read: a command that builds one
// adapter reads that adapter's key alone.
func LoadKeys(ctx context.Context, c *config.Config, getenv func(string) string, setenv func(k, v string) error, connect Connector, only ...string) error {
	if c == nil || !c.Hosted() || InCloudRun(getenv) {
		return nil
	}
	var vars []string
	for v := range c.KeyVariables() {
		if len(only) > 0 && !slices.Contains(only, v) {
			continue
		}
		if strings.TrimSpace(getenv(v)) == "" {
			vars = append(vars, v)
		}
	}
	if len(vars) == 0 {
		return nil
	}
	sort.Strings(vars)
	client, err := connect.Open(ctx)
	if err != nil {
		return fmt.Errorf("reading %s from Secret Manager: %w", strings.Join(vars, ", "), err)
	}
	var errs []error
	for _, v := range vars {
		secret, ok := KeySecrets[v]
		if !ok {
			continue
		}
		val, err := ReadKey(ctx, client, c.Hosting.Project, secret)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", v, err))
			continue
		}
		if err := setenv(v, val); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", v, err))
		}
	}
	return errors.Join(errs...)
}

// ReadKey reads one key secret's latest value, masks it in logs and returns
// it with surrounding whitespace removed (a value added with `echo` ends in a
// newline).
func ReadKey(ctx context.Context, client *Client, project, secret string) (string, error) {
	data, _, err := client.AccessSecret(ctx, project, secret)
	if err != nil {
		return "", err
	}
	val := strings.TrimSpace(string(data))
	if val == "" {
		return "", fmt.Errorf("secret %s is empty", secret)
	}
	logredact.AddSecretValues(val)
	return val, nil
}
