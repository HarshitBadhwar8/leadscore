// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// runRulesCheck compiles a rubric file and reports every error with its file
// and line, or the rubric's version and any warnings.
func runRulesCheck(inv *invocation) int {
	path := inv.args[0]
	src, err := os.ReadFile(path)
	if err != nil {
		return inv.fail(err)
	}
	r, err := rules.Compile(src)
	var errs rules.LoadErrors
	if errors.As(err, &errs) {
		lines := make([]string, len(errs))
		for i, e := range errs {
			lines[i] = located(path, e, "")
		}
		return inv.fail(fmt.Errorf("%d problem%s\n%s", len(errs), pluralS(len(errs)), strings.Join(lines, "\n")))
	}
	if err != nil {
		return inv.fail(err)
	}
	for _, w := range r.Warnings() {
		fmt.Fprintln(inv.stdout, located(path, w, "warning: "))
	}
	fmt.Fprintf(inv.stdout, "%s: ok (version %s, %d lane%s, %d detector%s)\n", path, r.Version(),
		len(r.Lanes()), pluralS(len(r.Lanes())), len(r.Detectors()), pluralS(len(r.Detectors())))
	return exitOK
}

// located writes a load error or warning as file:line: field: message.
func located(path string, e rules.LoadError, prefix string) string {
	where := path
	if e.Line > 0 {
		where = fmt.Sprintf("%s:%d", path, e.Line)
	}
	e.Line = 0
	return where + ": " + prefix + e.Error()
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
