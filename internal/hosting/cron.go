package hosting

import (
	"fmt"
	"regexp"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// Cron converts `schedule` into Cloud Scheduler's cron form (contracts section
// 3): a whole number of minutes dividing 60 is `*/N * * * *`, a whole number of
// hours dividing 24 is `0 */N * * *`, and 24h is `0 0 * * *`. Anything else
// cannot be written as a fixed cron interval and is refused. setup/gcp.sh does
// the same conversion in bash; a test holds the two equal.
func Cron(schedule time.Duration) (string, error) {
	bad := fmt.Errorf("schedule %s cannot run on Cloud Scheduler: use a whole number of minutes that divides 60 "+
		"(1m, 2m, 3m, 4m, 5m, 6m, 10m, 12m, 15m, 20m, 30m) or of hours that divides 24 (1h, 2h, 3h, 4h, 6h, 8h, 12h, 24h)", schedule)
	switch {
	case schedule <= 0 || schedule%time.Minute != 0:
		return "", bad
	case schedule < time.Hour:
		if m := int(schedule / time.Minute); 60%m == 0 {
			return fmt.Sprintf("*/%d * * * *", m), nil
		}
	case schedule == 24*time.Hour:
		return "0 0 * * *", nil
	case schedule < 24*time.Hour && schedule%time.Hour == 0:
		if h := int(schedule / time.Hour); 24%h == 0 {
			return fmt.Sprintf("0 */%d * * *", h), nil
		}
	}
	return "", bad
}

// TaskTimeout is the run job's task timeout: the deadline plus the save budget
// (contracts section 9.1 step 9).
func TaskTimeout(deadline time.Duration) time.Duration { return deadline + config.SaveBudget }

// scriptDuration is the duration form setup/gcp.sh reads: whole days, hours,
// minutes and seconds (15m, 1h30m, 1d). Go accepts more (0.25h, 900000ms);
// those are refused on Google Cloud so the script and Go always agree.
var scriptDuration = regexp.MustCompile(`^([0-9]+[dhms])+$`)

// ScheduleCron is the cron form of `schedule` as written in leadscore.yml,
// the same answer setup/gcp.sh's cron_for gives.
func ScheduleCron(text string) (string, error) {
	if !scriptDuration.MatchString(text) {
		return "", fmt.Errorf("schedule %q: on Google Cloud write it in whole days, hours, minutes or seconds, like 15m or 2h", text)
	}
	d, err := config.ParseDuration(text)
	if err != nil {
		return "", err
	}
	return Cron(d)
}

// CheckSchedule refuses a configuration Google Cloud cannot run safely: a
// schedule with no cron form, a schedule or deadline not written in the form
// setup/gcp.sh reads, or a run (deadline plus save budget) that does not end
// before the next one starts.
func CheckSchedule(c *config.Config) error {
	schedule, _ := c.Get("schedule")
	deadline, _ := c.Get("deadline")
	if _, err := ScheduleCron(schedule); err != nil {
		return err
	}
	if !scriptDuration.MatchString(deadline) {
		return fmt.Errorf("deadline %q: on Google Cloud write it in whole days, hours, minutes or seconds, like 12m", deadline)
	}
	if t := TaskTimeout(c.Deadline); t >= c.Schedule {
		return fmt.Errorf("deadline %s plus the %s save budget is %s, not below schedule %s: runs would overlap; "+
			"shorten deadline or lengthen schedule", c.Deadline, config.SaveBudget, t, c.Schedule)
	}
	return nil
}
