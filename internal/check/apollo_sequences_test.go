package check

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func TestApolloSequences(t *testing.T) {
	fake := fakeapollo.New("good-key")
	fake.AddMailbox("mailbox-1")
	fake.AddSequence("seq-1", "Founders")
	fake.AddSequence("seq-2", "Twice")
	fake.AddSequence("seq-3", "Twice")
	srv := httptest.NewServer(fake)
	defer srv.Close()

	compile := func(lanes string) *rules.Rubric {
		r, err := rules.Compile([]byte("version: 1\nlanes:\n" + lanes))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	good := compile("  - { id: a, kind: cold, when: { field: status, eq: new }, push: \"apollo:sequence/Founders\" }\n  - { id: b, kind: cold, when: { field: status, eq: new }, priority: 2, push: \"apollo:sequence/Founders\" }\n")
	bad := compile("  - { id: miss, kind: cold, when: { field: status, eq: new }, push: \"apollo:sequence/founders\" }\n" +
		"  - { id: dup, kind: cold, when: { field: status, eq: new }, priority: 2, push: \"apollo:sequence/Twice\" }\n" +
		"  - { id: other, kind: cold, when: { field: status, eq: new }, priority: 4, push: \"fake:x\" }\n")
	block := func(mailbox any) api.Config {
		b := api.Config{"base_url": srv.URL, "_http_client": srv.Client()}
		if mailbox != nil {
			b["mailbox_id"] = mailbox
		}
		return b
	}
	cfg := func(b api.Config) *config.Config {
		c := &config.Config{Sinks: map[string]api.Config{}}
		if b != nil {
			c.Sinks["apollo"] = b
		}
		return c
	}

	tests := []struct {
		name   string
		cfg    *config.Config
		rubric *rules.Rubric
		key    string
		want   map[string]bool // problem key -> warning
	}{
		{"no sinks.apollo", cfg(nil), good, "good-key", map[string]bool{}},
		{"no key: the secrets check reports it", cfg(block("mailbox-1")), good, "", map[string]bool{}},
		{"all good", cfg(block("mailbox-1")), good, "good-key", map[string]bool{}},
		{"no Apollo lanes: nothing to check", cfg(block(nil)), compile("  - { id: x, kind: cold, when: { field: status, eq: new }, push: \"fake:x\" }\n"), "good-key", map[string]bool{}},
		{"mailbox missing", cfg(block(nil)), good, "good-key", map[string]bool{"apollo-sequences:mailbox": false}},
		{"mailbox unknown", cfg(block("mailbox-9")), good, "good-key", map[string]bool{"apollo-sequences:mailbox": false}},
		{"mailbox as a number", cfg(block(12)), good, "good-key", map[string]bool{"apollo-sequences:mailbox": false}},
		{"names missing or ambiguous", cfg(block("mailbox-1")), bad, "good-key",
			map[string]bool{"apollo-sequences:miss": false, "apollo-sequences:dup": false}},
		{"unreachable is a warning", cfg(block("mailbox-1")), good, "wrong", map[string]bool{"apollo-sequences:unreachable": true}},
		{"base_url without a test client", cfg(api.Config{"base_url": srv.URL}), good, "good-key", map[string]bool{"apollo-sequences:config": false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := apolloSequences{getenv: func(k string) string {
				if k == "APOLLO_API_KEY" {
					return tc.key
				}
				return ""
			}}
			got := map[string]bool{}
			for _, p := range c.Run(context.Background(), Env{Config: tc.cfg, Rubric: tc.rubric}) {
				got[p.Key] = p.Warning
				if strings.Contains(p.Message, "@") {
					t.Errorf("%s: message carries an email: %q", p.Key, p.Message)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("problems %v, want %v", got, tc.want)
			}
			for k, w := range tc.want {
				if warn, ok := got[k]; !ok || warn != w {
					t.Errorf("problem %s: got (warning %v, present %v), want warning %v", k, warn, ok, w)
				}
			}
		})
	}

	// In doctor there is no run rubric: the file is compiled.
	dir := t.TempDir()
	path := filepath.Join(dir, "rubric.yml")
	if err := os.WriteFile(path, []byte("version: 1\nlanes:\n  - { id: miss, kind: cold, when: { field: status, eq: new }, push: \"apollo:sequence/Nope\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := cfg(block("mailbox-1"))
	c.RubricPath = path
	ps := apolloSequences{getenv: func(string) string { return "good-key" }}.Run(context.Background(), Env{Config: c})
	if len(ps) != 1 || ps[0].Key != "apollo-sequences:miss" {
		t.Errorf("doctor: problems %+v, want apollo-sequences:miss", ps)
	}
}
