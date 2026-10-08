// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

func TestReceiverSecretIsRegisteredOutsideRuns(t *testing.T) {
	for _, c := range All() {
		if c.Name() == "receiver-secret" {
			if c.InRun() {
				t.Error("receiver-secret runs at serve start and in doctor, not inside runs")
			}
			return
		}
	}
	t.Fatal("receiver-secret is not registered")
}

func TestReceiverSecret(t *testing.T) {
	receiver := load(t, "version: 1\nstore: { type: sqlite }\nreplies: receiver\n")
	visits := load(t, "version: 1\nstore: { type: sqlite }\nreplies: polling\nreceiver: { visit_events: [visit_pricing] }\n")
	polling := load(t, "version: 1\nstore: { type: sqlite }\nreplies: polling\n")
	hosted := load(t, "version: 1\nstore: { type: sheets, spreadsheet: s }\nreplies: receiver\nhosting: { project: p }\n")
	const missing, previous = "secret_missing:LEADSCORE_RECEIVER_SECRET", "secret_previous:LEADSCORE_RECEIVER_SECRET_PREVIOUS"
	cases := []struct {
		name string
		cfg  *config.Config
		vars map[string]string
		want []string
	}{
		{"receiver replies, secret set", receiver, map[string]string{"LEADSCORE_RECEIVER_SECRET": "x"}, nil},
		{"receiver replies, secret missing", receiver, nil, []string{missing}},
		{"receiver replies, blank secret", receiver, map[string]string{"LEADSCORE_RECEIVER_SECRET": "  "}, []string{missing}},
		{"visit workflows, secret missing", visits, nil, []string{missing}},
		{"polling and no visit workflows: not configured", polling, nil, nil},
		{"rotation not finished", receiver, map[string]string{"LEADSCORE_RECEIVER_SECRET": "x", "LEADSCORE_RECEIVER_SECRET_PREVIOUS": "y"}, []string{previous}},
		{"only the previous secret", receiver, map[string]string{"LEADSCORE_RECEIVER_SECRET_PREVIOUS": "y"}, []string{missing, previous}},
		{"hosted, a local command: Secret Manager holds it", hosted, nil, nil},
		{"hosted, inside Cloud Run", hosted, map[string]string{"K_SERVICE": "leadscore-receiver"}, []string{missing}},
	}
	for _, c := range cases {
		got := receiverSecret{getenv: env(c.vars)}.Run(context.Background(), Env{Config: c.cfg})
		var keys []string
		for _, p := range got {
			keys = append(keys, p.Key)
			if p.Warning != (p.Key == previous) {
				t.Errorf("%s: %s warning = %v; only an unfinished rotation is a warning", c.name, p.Key, p.Warning)
			}
		}
		if !reflect.DeepEqual(keys, c.want) {
			t.Errorf("%s: problems %v, want %v", c.name, keys, c.want)
		}
	}
}

// Both receiver secrets are masked in logs.
func TestReceiverSecretsAreMaskedInLogs(t *testing.T) {
	masked := map[string]bool{}
	for _, v := range logredact.SecretVariables {
		masked[v] = true
	}
	for _, v := range []string{ReceiverSecretVar, ReceiverSecretPreviousVar} {
		if !masked[v] {
			t.Errorf("%s is not in logredact.SecretVariables", v)
		}
	}
}

// The fix says where this install keeps the secret: the shell profile for the
// plain binary, .env in a container, Secret Manager inside Cloud Run.
func TestReceiverSecretFixNamesTheInstallsPlace(t *testing.T) {
	c := load(t, "version: 1\nstore: { type: sqlite }\nreplies: receiver\n")
	old := inContainer
	t.Cleanup(func() { inContainer = old })
	fixFor := func(container bool, vars map[string]string) string {
		inContainer = func() bool { return container }
		ps := ReceiverSecretProblems(c, env(vars))
		if len(ps) != 1 {
			t.Fatalf("problems %+v", ps)
		}
		return ps[0].Fix
	}
	if f := fixFor(false, nil); !strings.Contains(f, "shell profile") || strings.Contains(f, ".env") || strings.Contains(f, "Secret Manager") {
		t.Errorf("plain binary: %q", f)
	}
	if f := fixFor(true, nil); !strings.Contains(f, ".env") || strings.Contains(f, "shell profile") {
		t.Errorf("container: %q", f)
	}
	if f := fixFor(false, map[string]string{"CLOUD_RUN_JOB": "leadscore-run"}); !strings.Contains(f, "Secret Manager") {
		t.Errorf("Cloud Run: %q", f)
	}
}
