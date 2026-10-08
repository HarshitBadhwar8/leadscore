// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package leadscore_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestExampleFolders checks that every folder under examples/ has a README.md
// and an executable run.sh, and that examples/README.md links exactly those
// folders. scripts/run-examples.sh runs them and compares their output.
func TestExampleFolders(t *testing.T) {
	entries, err := os.ReadDir("examples")
	if err != nil {
		t.Fatal(err)
	}
	var present []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		present = append(present, e.Name())
		dir := filepath.Join("examples", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		info, err := os.Stat(filepath.Join(dir, "run.sh"))
		if err != nil {
			t.Errorf("%s: %v", dir, err)
		} else if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s/run.sh is not executable", dir)
		}
	}
	if len(present) < 3 {
		t.Errorf("examples/ has %d example folders, want at least 3", len(present))
	}
	var listed []string
	for _, m := range regexp.MustCompile(`\]\(([a-z0-9-]+)/\)`).FindAllStringSubmatch(readFile(t, "examples/README.md"), -1) {
		listed = append(listed, m[1])
	}
	slices.Sort(listed)
	if !slices.Equal(listed, present) {
		t.Errorf("examples/README.md lists %v, the folders are %v", listed, present)
	}
}

// quickStartSkips are the Quick start lines examples/score-a-csv/run.sh does
// not type, each with the reason.
var quickStartSkips = map[string]string{
	"git clone https://github.com/HarshitBadhwar8/leadscore.git": "needs the network; the example runs from this checkout",
	"cd leadscore":                          "scripts/example-setup.sh moves into a temporary copy instead",
	"go build -o leadscore ./cmd/leadscore": "scripts/example-setup.sh builds ./leadscore",
}

// TestQuickStartExample checks that examples/score-a-csv types every Quick
// start command verbatim, apart from the lines in quickStartSkips, and that
// its README shows the Quick start's output.
func TestQuickStartExample(t *testing.T) {
	readme := readFile(t, "README.md")
	start := strings.Index(readme, "\n## Quick start\n")
	if start < 0 {
		t.Fatal("README.md has no Quick start section")
	}
	section := readme[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	cmds := fencedBlocks(section, "sh")
	outs := fencedBlocks(section, "text")
	if len(cmds) == 0 || len(outs) == 0 {
		t.Fatal("the Quick start has no sh block or no text block")
	}

	var want []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(cmds[0], "\n"), "\n") {
		if _, skip := quickStartSkips[line]; skip {
			seen[line] = true
			continue
		}
		want = append(want, line)
	}
	for line, why := range quickStartSkips {
		if !seen[line] {
			t.Errorf("the Quick start no longer has %q, which the example skips (%s)", line, why)
		}
	}

	script := readFile(t, "examples/score-a-csv/run.sh")
	const setup = `. "$(dirname "$0")/../../scripts/example-setup.sh"` + "\n"
	i := strings.Index(script, setup)
	if i < 0 {
		t.Fatal("examples/score-a-csv/run.sh does not source scripts/example-setup.sh")
	}
	got := strings.Split(strings.Trim(script[i+len(setup):], "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("examples/score-a-csv/run.sh types\n%s\nthe Quick start types\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	exOuts := fencedBlocks(readFile(t, "examples/score-a-csv/README.md"), "text")
	if len(exOuts) == 0 || exOuts[len(exOuts)-1] != outs[0] {
		t.Errorf("examples/score-a-csv/README.md does not show the Quick start's output")
	}
}

// fencedBlocks returns the bodies of the fenced blocks tagged lang, in order.
func fencedBlocks(md, lang string) []string {
	var out []string
	var buf strings.Builder
	in := false
	for _, line := range strings.SplitAfter(md, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case !in && trimmed == "```"+lang:
			in = true
			buf.Reset()
		case in && trimmed == "```":
			in = false
			out = append(out, buf.String())
		case in:
			buf.WriteString(line)
		}
	}
	return out
}
