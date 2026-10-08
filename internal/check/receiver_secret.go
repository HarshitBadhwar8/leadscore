// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"os"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

func init() { Register(receiverSecret{getenv: os.Getenv}) }

// The receiver's secret variables; internal/receiver reads them.
const (
	ReceiverSecretVar         = "LEADSCORE_RECEIVER_SECRET"
	ReceiverSecretPreviousVar = "LEADSCORE_RECEIVER_SECRET_PREVIOUS"
)

// receiverSecret is the receiver-secret check: it runs
// when `serve` starts and in doctor, never inside runs.
type receiverSecret struct{ getenv func(string) string }

func (receiverSecret) Name() string { return "receiver-secret" }
func (receiverSecret) InRun() bool  { return false }

func (c receiverSecret) Run(_ context.Context, env Env) []Problem {
	return ReceiverSecretProblems(env.Config, c.getenv)
}

// ReceiverConfigured reports whether the install expects webhooks: replies
// come from the receiver, or workflows send visit events.
func ReceiverConfigured(c *config.Config) bool {
	return c.Replies == "receiver" || len(c.Receiver.VisitEvents) > 0
}

// inContainer reports whether this process runs in a Docker container; a
// variable so tests can say either.
var inContainer = func() bool { _, err := os.Stat("/.dockerenv"); return err == nil }

// receiverSecretFix says where this install keeps the receiver secret:
// Secret Manager on Google Cloud, .env on Docker, the shell profile for the
// plain binary (as the README's CSV-only path says).
func receiverSecretFix(inCloudRun bool) string {
	where := "your shell profile (`export " + ReceiverSecretVar + "=<a long random value>` in ~/.zshrc or ~/.bashrc), so every new terminal has it"
	switch {
	case inCloudRun:
		where = "Secret Manager (`setup/gcp.sh secrets`)"
	case inContainer():
		where = ".env, then `docker compose up -d`"
	}
	return "set " + ReceiverSecretVar + " in " + where + "; use the same value in each Apollo workflow (a CSV-only install needs any long random value)"
}

// ReceiverSecretProblems fails when the receiver is configured and its secret
// is missing, and warns while a previous secret is still set (a rotation not
// finished). `serve` calls it at start.
func ReceiverSecretProblems(c *config.Config, getenv func(string) string) []Problem {
	if c == nil {
		return nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	// A local command on a hosted install keeps the secret in Secret Manager,
	// not in its environment; the hosted path extends this check there, as it does
	// `secrets`. Inside Cloud Run the variables are filled from Secret
	// Manager and are checked as usual.
	inCloudRun := getenv("K_SERVICE") != "" || getenv("CLOUD_RUN_JOB") != ""
	if c.Hosted() && !inCloudRun {
		return nil
	}
	var out []Problem
	if ReceiverConfigured(c) && strings.TrimSpace(getenv(ReceiverSecretVar)) == "" {
		why := "replies: receiver is set"
		if c.Replies != "receiver" {
			why = "receiver.visit_events lists visit workflows"
		}
		out = append(out, Problem{
			Key:     "secret_missing:" + ReceiverSecretVar,
			Message: ReceiverSecretVar + " is not set, but " + why + ", so every webhook is refused",
			Fix:     receiverSecretFix(inCloudRun),
		})
	}
	if strings.TrimSpace(getenv(ReceiverSecretPreviousVar)) != "" {
		out = append(out, Problem{
			Key:     "secret_previous:" + ReceiverSecretPreviousVar,
			Message: ReceiverSecretPreviousVar + " is still set, so the old receiver secret still works",
			Fix:     "once every Apollo workflow sends the new secret, remove " + ReceiverSecretPreviousVar,
			Warning: true,
		})
	}
	return out
}
