package check

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func TestReceiverSilenceIsInRun(t *testing.T) {
	for _, c := range All() {
		if c.Name() == "receiver-silence" {
			if !c.InRun() {
				t.Error("receiver-silence runs inside every run")
			}
			return
		}
	}
	t.Fatal("receiver-silence is not registered")
}

// Silence is measured from the later of the kind's last event and the first
// run, against silence_threshold, for the kinds the receiver expects: `sent`
// with replies: receiver, and every receiver.visit_events kind.
func TestReceiverSilence(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	receiver := load(t, "version: 1\nstore: { type: sqlite }\nreplies: receiver\nreceiver: { visit_events: [Visit_Pricing] }\n")
	visitsOnly := load(t, "version: 1\nstore: { type: sqlite }\nreplies: polling\nreceiver: { visit_events: [visit_pricing] }\n")
	polling := load(t, "version: 1\nstore: { type: sqlite }\nreplies: polling\n")
	week := load(t, "version: 1\nstore: { type: sqlite }\nreplies: receiver\nsilence_threshold: 7d\n")
	cases := []struct {
		name  string
		cfg   *config.Config
		state map[string]time.Time
		want  []string
	}{
		{"no run yet", receiver, nil, nil},
		{"new install, inside the threshold", receiver, map[string]time.Time{"first_run_at": now.Add(-2 * day)}, nil},
		{"new install, nothing ever arrived", receiver, map[string]time.Time{"first_run_at": now.Add(-4 * day)},
			[]string{"silent:sent", "silent:visit_pricing"}},
		{"each kind on its own", receiver, map[string]time.Time{
			"first_run_at": now.Add(-30 * day), "last_received:sent": now.Add(-time.Hour), "last_received:visit_pricing": now.Add(-5 * day)},
			[]string{"silent:visit_pricing"}},
		{"all arriving", receiver, map[string]time.Time{
			"first_run_at": now.Add(-30 * day), "last_received:sent": now.Add(-time.Hour), "last_received:visit_pricing": now.Add(-day)}, nil},
		{"polling expects no sent", visitsOnly, map[string]time.Time{"first_run_at": now.Add(-30 * day)}, []string{"silent:visit_pricing"}},
		{"receiver not configured", polling, map[string]time.Time{"first_run_at": now.Add(-30 * day)}, nil},
		{"the team's threshold", week, map[string]time.Time{"first_run_at": now.Add(-30 * day), "last_received:sent": now.Add(-6 * day)}, nil},
	}
	for _, c := range cases {
		m := model.New()
		for k, v := range c.state {
			m.SetState(k, model.FormatTime(v))
		}
		got := receiverSilence{}.Run(context.Background(), Env{Config: c.cfg, Model: m, Now: func() time.Time { return now }})
		var keys []string
		for _, p := range got {
			keys = append(keys, p.Key)
			if p.Warning {
				t.Errorf("%s: %s is a warning; a silent receiver makes the run unhealthy", c.name, p.Key)
			}
		}
		if !reflect.DeepEqual(keys, c.want) {
			t.Errorf("%s: problems %v, want %v", c.name, keys, c.want)
		}
	}
	// With no model (doctor could not load the store) the check skips.
	if got := (receiverSilence{}).Run(context.Background(), Env{Config: receiver}); got != nil {
		t.Errorf("no model: %v", got)
	}
}
