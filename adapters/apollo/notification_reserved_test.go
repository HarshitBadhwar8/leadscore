package apollo

import (
	"strings"
	"testing"
)

// A delivered stage equal to either reserved marker must be refused: both
// markers stand for sticky outcomes (an opt-out, a positive reply), so "nobody
// would type that" is not a guarantee to rest on.
func TestAReservedStageCannotArriveAsAStage(t *testing.T) {
	for _, marker := range []string{stageUnsubscribed, stageRepliedPositive} {
		t.Run(marker, func(t *testing.T) {
			_, err := parseNotification([]byte(
				`{"event":"email_replied","contact_email":"dana@example.com","contact_stage":"` + marker + `"}`))
			if err == nil {
				t.Fatalf("a delivered %s stage was accepted", marker)
			}
			// By reason, not merely non-nil: a missing-email or bad-JSON error
			// would also be non-nil and leave the guard itself unproven.
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error = %v, want it to name the stage as reserved", err)
			}
		})
	}
	// The genuine kinds still parse and act.
	for _, kind := range []string{"email_unsubscribed", "email_replied_positive"} {
		n, err := parseNotification([]byte(`{"event":"` + kind + `","contact_email":"dana@example.com"}`))
		if err != nil {
			t.Fatalf("a genuine %s engagement was refused: %v", kind, err)
		}
		if !n.acts() {
			t.Errorf("a %s engagement must still be actionable", kind)
		}
	}
}
