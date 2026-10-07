package receiver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	fakesink "github.com/HarshitBadhwar8/leadscore/internal/fakes/sink"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

var outcomeVendor = fakesink.New(nil)

func init() {
	api.RegisterSink("hookfake", func(api.Config) (api.Sink, error) { return outcomeVendor.Sink(), nil })
}

// An unsubscribe posted to /apollo/reply with the secret is stored by the
// handler, and the next run does not push that person.
func TestWebhookUnsubscribeBlocksTheNextPush(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("leads.csv", "Email,Name,Title,Domain\nana@acme.example,Ana A,Clerk,acme.example\nbo@beta.example,Bo B,Clerk,beta.example\n")
	write("rubric.yml", "version: 1\nlanes:\n  - { id: cold, kind: cold, when: { field: receiver_only, eq: false }, push: \"hookfake:list\" }\n")
	config := func(pushes string) {
		write("leadscore.yml", "version: 1\nrubric: rubric.yml\nstore: { type: sqlite, path: leadscore.db }\nexport: { dir: out }\n"+
			"sources:\n  - { id: leads, type: csv, path: leads.csv }\nsinks:\n  hookfake: {}\npushes_enabled: "+pushes+"\n")
	}
	run := func() {
		t.Helper()
		var out bytes.Buffer
		if _, err := engine.RunTo(context.Background(), api.RunOptions{ConfigPath: filepath.Join(dir, "leadscore.yml")}, &out); err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
	}
	config("false")
	run()

	s, err := sqlite.Open(filepath.Join(dir, "leadscore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := newTestHandler(t, testStore{name: "sqlite", store: s, events: s}, nil)
	body := []byte(`{"event":"email_unsubscribed","contact_email":"Ana@ACME.example","contact_stage":""}`)
	if rec := post(h, "/apollo/reply", body, testSecret); rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}

	config("true")
	run()
	var pushed []string
	for _, c := range outcomeVendor.Calls() {
		pushed = append(pushed, c.Lead.Emails[0])
	}
	if len(pushed) != 1 || pushed[0] != "bo@beta.example" {
		t.Errorf("pushed %v, want only bo (ana unsubscribed by webhook)", pushed)
	}
}
