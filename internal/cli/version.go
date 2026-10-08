// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cli

import (
	"fmt"
	"runtime/debug"
)

// Version is the release version. The release build sets it with
// -ldflags "-X github.com/HarshitBadhwar8/leadscore/internal/cli.Version=<tag>"
// (release.yml and the Dockerfile's VERSION build argument).
var Version = "dev"

// versionLine is what `leadscore version` and `leadscore --version` print: the
// version, and for a build that did not set one, the commit it was built from
// when Go recorded it.
func versionLine() string {
	v := Version
	if v == "dev" {
		if rev := vcsRevision(); rev != "" {
			v += " (" + rev + ")"
		}
	}
	return "leadscore " + v
}

// vcsRevision is the short commit the binary was built from, with -dirty for
// uncommitted changes, or "" when Go recorded none.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}

func runVersion(inv *invocation) int {
	fmt.Fprintln(inv.stdout, versionLine())
	return exitOK
}
