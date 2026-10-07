package leadscore_test

// The S15 proof through the public entry point: an unsubscribe the receiver
// stores while leadscore.Run is pushing blocks its lead in the next batch,
// with the production hooks (the default re-read before each batch).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore"
	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	fakesink "github.com/HarshitBadhwar8/leadscore/internal/fakes/sink"
)

// outcomeVendor is the fake behind the `s15fake` sink type.
var outcomeVendor = fakesink.New(nil)

func init() {
	leadscore.RegisterSink("s15fake", func(leadscore.Config) (leadscore.Sink, error) { return outcomeVendor.Sink(), nil })
}

func TestUnsubscribeMidRunBlocksTheNextBatchThroughRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lines := []string{"Email,Name,Title,Domain"}
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("p%02d@p%02d.example,P%02d,Clerk,p%02d.example", i, i, i, i))
	}
	write("leads.csv", strings.Join(lines, "\n")+"\n")
	write("rubric.yml", `version: 1
lanes:
  - { id: cold, kind: cold, when: { field: receiver_only, eq: false }, push: "s15fake:list" }
`)
	db := filepath.Join(dir, "leadscore.db")
	write("leadscore.yml", "version: 1\nrubric: rubric.yml\nstore: { type: sqlite, path: leadscore.db }\nexport: { dir: out }\n"+
		"sources:\n  - { id: leads, type: csv, path: leads.csv }\nsinks:\n  s15fake: {}\npushes_enabled: true\n")

	open, ok := api.BackendFactory("sqlite") // registered by the root package
	if !ok {
		t.Fatal("no sqlite store")
	}
	store, events, err := open(leadscore.Config{"path": db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := store.(interface{ Close() error }); ok {
			c.Close()
		}
	})

	// While batch 1 is being pushed, the receiver stores p29's unsubscribe.
	var once sync.Once
	outcomeVendor.Before(func(ctx context.Context, _ leadscore.StepRequest) {
		once.Do(func() {
			body := `{"event":"email_unsubscribed","contact_email":"P29@p29.example","contact_stage":""}`
			if err := events.AppendEvents(ctx, []leadscore.RawEvent{{Kind: "apollo_reply", ReceivedAt: time.Now().UTC(), Body: []byte(body)}}); err != nil {
				t.Error(err)
			}
		})
	})

	res, err := leadscore.Run(context.Background(), leadscore.RunOptions{ConfigPath: filepath.Join(dir, "leadscore.yml")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 29 {
		t.Errorf("pushed %d, want 29 (p29 opted out mid-run)", res.Pushed)
	}
	for _, c := range outcomeVendor.Calls() {
		if c.Lead.Emails[0] == "p29@p29.example" {
			t.Fatal("a lead who opted out mid-run was pushed in the next batch")
		}
	}
	rows, err := store.ReadTable(context.Background(), "Outcomes")
	if err != nil {
		t.Fatal(err)
	}
	unsub := 0
	for _, r := range rows {
		if r["status"] == "unsubscribed" && r["unsubscribed_origin"] == "event" {
			unsub++
		}
	}
	if unsub != 1 {
		t.Errorf("%d unsubscribed leads saved, want p29", unsub)
	}
}
