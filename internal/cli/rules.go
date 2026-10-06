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
	if err != nil {
		var errs rules.LoadErrors
		if !errors.As(err, &errs) {
			return inv.fail(err)
		}
		lines := make([]string, len(errs))
		for i, e := range errs {
			e2 := e
			e2.Line = 0
			lines[i] = path + ": " + e2.Error()
			if e.Line > 0 {
				lines[i] = fmt.Sprintf("%s:%d: %s", path, e.Line, e2.Error())
			}
		}
		return inv.fail(fmt.Errorf("%d problem%s\n%s", len(errs), pluralS(len(errs)), strings.Join(lines, "\n")))
	}
	for _, w := range r.Warnings() {
		fmt.Fprintf(inv.stdout, "warning: %s\n", w)
	}
	fmt.Fprintf(inv.stdout, "%s: ok (version %s, %d lane%s, %d detector%s)\n", path, r.Version(),
		len(r.Lanes()), pluralS(len(r.Lanes())), len(r.Detectors()), pluralS(len(r.Detectors())))
	return exitOK
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
