// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

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
		{"a refused key fails", cfg(block("mailbox-1")), good, "wrong", map[string]bool{"apollo-sequences:key": false}},
		{"unreachable is a warning", cfg(block("mailbox-1")), good, "good-key/rate_limited", map[string]bool{"apollo-sequences:unreachable": true}},
		{"forbidden on the sequence search fails", cfg(block("mailbox-1")), good, "good-key/forbidden", map[string]bool{"apollo-sequences:key": false}},
		{"base_url without a test client", cfg(api.Config{"base_url": srv.URL}), good, "good-key", map[string]bool{"apollo-sequences:config": false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			if c, ok := strings.CutPrefix(key, "good-key/"); ok { // a good key, and the sequence search answers this case
				key = "good-key"
				fake.FailNext(fakeapollo.CallSearchSequences, c)
			}
			c := apolloSequences{getenv: func(k string) string {
				if k == "APOLLO_API_KEY" {
					return key
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

// sinks.apollo.mailbox_id may be the mailbox's address (or one of its
// aliases). It resolves quietly, in doctor and in a run; an address no
// mailbox has fails as apollo-sequences:mailbox, without the address.
func TestApolloSequencesMailboxAddress(t *testing.T) {
	fake := fakeapollo.New("good-key")
	fake.AddMailboxAddress("mailbox-7", "sales@acme.example")
	fake.AddSequence("seq-1", "Founders")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	r, err := rules.Compile([]byte("version: 1\nlanes:\n  - { id: a, kind: cold, when: { field: status, eq: new }, push: \"apollo:sequence/Founders\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	ck := apolloSequences{getenv: func(k string) string {
		if k == "APOLLO_API_KEY" {
			return "good-key"
		}
		return ""
	}}
	run := func(mailbox string, doctor bool) []Problem {
		c := &config.Config{Sinks: map[string]api.Config{"apollo": {"base_url": srv.URL, "_http_client": srv.Client(), "mailbox_id": mailbox}}}
		ps := ck.Run(context.Background(), Env{Config: c, Rubric: r, Doctor: doctor})
		for _, p := range ps {
			if strings.Contains(p.Message, "@") {
				t.Errorf("%s: message carries an email: %q", p.Key, p.Message)
			}
		}
		return ps
	}
	if ps := run("Sales@Acme.example", true); len(ps) != 0 {
		t.Errorf("doctor, a known address: %+v", ps)
	}
	if ps := run("sales@acme.example", false); len(ps) != 0 {
		t.Errorf("a run, a known address: %+v", ps)
	}
	for _, doctor := range []bool{false, true} {
		if ps := run("nobody@acme.example", doctor); len(ps) != 1 || ps[0].Key != "apollo-sequences:mailbox" || ps[0].Warning {
			t.Errorf("an unknown address (doctor %v): %+v", doctor, ps)
		}
	}
	if ps := run("mailbox-7", true); len(ps) != 0 {
		t.Errorf("doctor, an id: %+v", ps)
	}
}
