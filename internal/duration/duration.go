// Package duration reads the durations leadscore.yml and the rubric use.
package duration

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var daysPrefix = regexp.MustCompile(`^(\d+)d(.*)$`)

// maxDuration bounds every duration: nothing in leadscore.yml is meaningfully
// longer, and the bound keeps days*24h far from overflowing.
const maxDuration = 100 * 365 * 24 * time.Hour

// Parse reads a Go duration plus `d` for days (contracts "Formats"):
// "90d", "1d12h", "15m". Negative values and anything over 100 years are
// refused: every duration in leadscore.yml is a length of time.
func Parse(s string) (time.Duration, error) {
	var total time.Duration
	rest := s
	if m := daysPrefix.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days > 100*365 {
			return 0, fmt.Errorf("invalid duration %q: over 100 years", s)
		}
		total = time.Duration(days) * 24 * time.Hour
		rest = m[2]
		if rest == "" {
			return total, nil
		}
	}
	d, err := time.ParseDuration(rest)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use a Go duration or days, like 15m or 30d", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	if d > maxDuration || total+d > maxDuration {
		return 0, fmt.Errorf("invalid duration %q: over 100 years", s)
	}
	return total + d, nil
}
