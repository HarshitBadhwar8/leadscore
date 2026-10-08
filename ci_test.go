// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package leadscore_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The CI workflow's safety settings, held by a test so an edit cannot drop them
// silently.
func TestCIWorkflowSettings(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress string `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	// Every push to main is its own group, so no main build is cancelled or dropped.
	if !strings.Contains(wf.Concurrency.Group, "github.sha") {
		t.Errorf("concurrency group %q must key push builds by commit", wf.Concurrency.Group)
	}
	if !strings.Contains(wf.Concurrency.CancelInProgress, "pull_request") {
		t.Errorf("cancel-in-progress %q must apply to pull requests only", wf.Concurrency.CancelInProgress)
	}
	var authSeen, windowsSeen bool
	for _, job := range wf.Jobs {
		for _, st := range job.Steps {
			if strings.HasPrefix(st.Uses, "google-github-actions/auth@") {
				authSeen = true
				if st.With["create_credentials_file"] != "false" {
					t.Error("the auth step must not write a credentials file into the workspace")
				}
			}
			if strings.Contains(st.Run, "GOOS=windows go build") && strings.Contains(st.Run, "GOOS=windows go vet") {
				windowsSeen = true
			}
		}
	}
	if !authSeen {
		t.Error("no auth step found")
	}
	if !windowsSeen {
		t.Error("CI must build and vet for Windows")
	}
}
