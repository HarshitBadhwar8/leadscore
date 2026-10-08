// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package leadscore_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The release workflow publishes, so its trigger and permissions are held by a
// test: it runs only for a pushed v* tag, every action is pinned to a commit,
// and only the image job may write packages.
func TestReleaseWorkflowSettings(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On          map[string]map[string][]string `yaml:"on"`
		Permissions map[string]string              `yaml:"permissions"`
		Jobs        map[string]struct {
			Needs       any               `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	// Trigger: a push of a v* tag, and nothing else.
	if len(wf.On) != 1 || wf.On["push"] == nil {
		t.Fatalf("on = %v, want only push", wf.On)
	}
	push := wf.On["push"]
	if len(push) != 1 || len(push["tags"]) != 1 || push["tags"][0] != "v*" {
		t.Errorf("on.push = %v, want only tags: [v*]", push)
	}

	// Least privilege: nothing at the top; write access only where needed.
	if len(wf.Permissions) != 0 {
		t.Errorf("top-level permissions = %v, want {}", wf.Permissions)
	}
	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	var packageWriters []string
	built := false
	for name, job := range wf.Jobs {
		if len(job.Permissions) == 0 {
			t.Errorf("job %s must list its permissions", name)
		}
		for scope, level := range job.Permissions {
			if level != "write" {
				continue
			}
			switch {
			case scope == "packages":
				packageWriters = append(packageWriters, name)
			case scope == "contents" && name == "binaries":
			default:
				t.Errorf("job %s may not have %s: write", name, scope)
			}
		}
		for _, st := range job.Steps {
			if st.Uses != "" && !pinned.MatchString(st.Uses) {
				t.Errorf("job %s: %q is not pinned to a commit SHA", name, st.Uses)
			}
			// No token left in .git for later steps, and no shared Go cache
			// feeding a published build.
			if strings.HasPrefix(st.Uses, "actions/checkout@") && st.With["persist-credentials"] != "false" {
				t.Errorf("job %s: checkout must set persist-credentials: false", name)
			}
			if strings.HasPrefix(st.Uses, "actions/setup-go@") && st.With["cache"] != "false" {
				t.Errorf("job %s: setup-go must set cache: false", name)
			}
			if strings.Contains(st.Run, "go build") && strings.Contains(st.Run, "./cmd/leadscore") {
				built = true
				for _, w := range []string{"darwin", "linux", "windows", "amd64", "arm64", "sha256sum", "-trimpath"} {
					if !strings.Contains(st.Run, w) {
						t.Errorf("the binaries step does not mention %s", w)
					}
				}
			}
		}
	}
	// Nothing is published unless the tagged commit passes its tests.
	for _, name := range []string{"binaries", "image"} {
		job, ok := wf.Jobs[name]
		if !ok {
			t.Errorf("no %s job", name)
			continue
		}
		if needs := fmt.Sprint(job.Needs); needs != "test" && needs != "[test]" {
			t.Errorf("job %s needs %v, want test", name, job.Needs)
		}
	}
	// latest moves only for a final version, never for a v1.2.0-rc1 tag.
	if !strings.Contains(string(data), `"$TAG" != *-*`) {
		t.Error("the image job must tag latest only when the tag has no -")
	}
	if len(packageWriters) != 1 || packageWriters[0] != "image" {
		t.Errorf("jobs with packages: write = %v, want only image", packageWriters)
	}
	if !built {
		t.Error("no step builds the leadscore binaries")
	}
	if !strings.Contains(string(data), `CGO_ENABLED: "0"`) {
		t.Error("release binaries must be built with CGO_ENABLED=0")
	}
}
