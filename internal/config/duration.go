package config

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var daysPrefix = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseDuration reads a Go duration plus `d` for days (contracts "Formats"):
// "90d", "1d12h", "15m". Negative values are refused: every duration in
// leadscore.yml is a length of time.
func ParseDuration(s string) (time.Duration, error) {
	var total time.Duration
	rest := s
	if m := daysPrefix.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days > 100000 {
			return 0, fmt.Errorf("invalid duration %q", s)
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
	return total + d, nil
}
