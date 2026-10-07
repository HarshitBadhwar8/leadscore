package leadscore_test

import (
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
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
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
			if strings.Contains(st.Run, "go build") && strings.Contains(st.Run, "./cmd/leadscore") {
				built = true
				for _, w := range []string{"darwin", "linux", "windows", "amd64", "arm64", "sha256sum"} {
					if !strings.Contains(st.Run, w) {
						t.Errorf("the binaries step does not mention %s", w)
					}
				}
			}
		}
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
