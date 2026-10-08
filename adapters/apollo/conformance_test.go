package apollo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/events"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

const key = "conformance-key"

func fake(t *testing.T) (*fakeapollo.Server, api.Config) {
	t.Helper()
	t.Setenv(apollo.KeyVariable, key)
	f := fakeapollo.New(key)
	f.AddSequence("seq-0001", "Qualified founders")
	f.AddMailbox("mailbox-0001")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, api.Config{"base_url": srv.URL, "_http_client": srv.Client(), "mailbox_id": "mailbox-0001"}
}

// Proof: the apollo sink passes sinktest against the fake (crash replay and
// a repeated key leave one contact and one enrollment; each failure kind maps
// to its error).
func TestSinkConformance(t *testing.T) {
	f, cfg := fake(t)
	sinktest.Run(t, sinktest.Harness{
		New: func(api.Config) (api.Sink, error) { return apollo.NewSink(cfg) },
		// The fake is the vendor: Count and Fail act on its contacts and enrollments.
		Vendor: f,
		Dests:  []string{"sequence/Qualified founders"},
	})
}

// Proof: polling with a later label yields a new event. The engine keys a
// polled reply by message id plus label (events.Key), so the same reply
// polled again is the same event, and the reply relabelled is a new one.
func TestALaterLabelIsANewEvent(t *testing.T) {
	f, cfg := fake(t)
	sent := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	f.AddReply(fakeapollo.Reply{MessageID: "msg-1", ContactID: "contact-1", Email: "dana@acme-robotics.example",
		SentAt: sent})
	p, err := apollo.NewPoller(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys := func() []api.EventID {
		evs, err := p.Poll(context.Background(), sent.Add(-24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var out []api.EventID
		for _, e := range evs {
			e.Origin = events.OriginPolling // as the engine sets it
			out = append(out, events.Key(merge.NormalizeEventKeys(e)))
		}
		return out
	}
	first, repeat := keys(), keys()
	if len(first) != 1 || len(repeat) != 1 || first[0] != repeat[0] {
		t.Fatalf("the same reply polled twice: %v then %v, want one key", first, repeat)
	}
	f.SetLabel("msg-1", "willing_to_meet")
	later := keys()
	if len(later) != 1 || later[0] == first[0] {
		t.Fatalf("relabelled: %v, want a key other than %v", later, first)
	}
	if later[0] != apollo.PolledReplyKey("msg-1", "willing_to_meet") {
		t.Errorf("key %v, want the message id plus the label", later[0])
	}
}

// A polled reply with no message id and no time Apollo gave is keyed by its
// person and label only, so polling it again later is the same event.
func TestAnUntimedReplyKeepsItsKey(t *testing.T) {
	t.Setenv(apollo.KeyVariable, key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Page int `json:"page"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Page > 1 { // the reply search ends on an empty page
			_, _ = w.Write([]byte(`{"emailer_messages":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"emailer_messages":[{"contact_id":"contact-1","to_email":"dana@acme-robotics.example","reply_class":"not_interested"}]}`))
	}))
	t.Cleanup(srv.Close)
	p, err := apollo.NewPoller(api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := p.Poll(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(evs) != 1 {
		t.Fatalf("evs %v err %v", evs, err)
	}
	e := evs[0]
	e.Origin = events.OriginPolling
	first := events.Key(merge.NormalizeEventKeys(e))
	e.At, e.ReceivedAt = e.At.Add(6*time.Hour), e.ReceivedAt.Add(6*time.Hour) // the next poll's time
	if again := events.Key(merge.NormalizeEventKeys(e)); again != first {
		t.Errorf("keys %v then %v, want one", first, again)
	}
}
