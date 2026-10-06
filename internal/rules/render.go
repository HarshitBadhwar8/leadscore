package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// renderValue writes a value as a person reads it: numbers without a trailing
// .0, dates as days, text as is (quoted when it has spaces or is empty).
func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "no value"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		if x.Equal(x.Truncate(24 * time.Hour)) {
			return x.Format("2006-01-02")
		}
		return x.Format(time.RFC3339)
	case string:
		if x == "" || strings.ContainsAny(x, " \t,()[]\"") {
			return strconv.Quote(x)
		}
		return x
	}
	return fmt.Sprint(v)
}

func signed(f float64) string {
	if f < 0 {
		return renderValue(f)
	}
	return "+" + renderValue(f)
}

// Explain renders a verdict for a person (`leadscore explain`, the `Ranked`
// tab's reasons): each derived name in rubric order, the score and its halves,
// then the reasons in the order the rules fired.
func (r *Rubric) Explain(v api.Verdict) string {
	var b strings.Builder
	for _, d := range r.derive {
		fmt.Fprintf(&b, "%s: %s\n", d.name, renderValue(v.Values[d.name]))
	}
	fmt.Fprintf(&b, "score: %s (account %s, contact %s)\n",
		renderValue(v.AccountScore+v.ContactScore), renderValue(v.AccountScore), renderValue(v.ContactScore))
	if len(v.Reasons) > 0 {
		b.WriteString("reasons:\n")
		for _, reason := range v.Reasons {
			b.WriteString("  - " + reason + "\n")
		}
	}
	if v.RubricVersion != "" {
		fmt.Fprintf(&b, "rubric: %s\n", v.RubricVersion)
	}
	return b.String()
}

// ReasonsText joins a verdict's reasons into one line, for a table cell
// (`Ranked.reasons`, export rows, the HubSpot reasons property).
func ReasonsText(v api.Verdict) string { return strings.Join(v.Reasons, "; ") }
