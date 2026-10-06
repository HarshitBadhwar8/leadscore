package merge

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// world is a model plus a clock and the run's source settings, so a test reads
// as a sequence of runs.
type world struct {
	t       *testing.T
	m       *model.Model
	now     time.Time
	sources []config.Source
	aliases map[string]string
}

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func newWorld(t *testing.T, sources ...config.Source) *world {
	t.Helper()
	seq := 0
	prev := newLeadID
	newLeadID = func() api.LeadID { seq++; return api.LeadID(fmt.Sprintf("L%03d", seq)) }
	t.Cleanup(func() { newLeadID = prev })
	for i := range sources {
		if sources[i].Channel == "" {
			sources[i].Channel = sources[i].ID
		}
	}
	return &world{t: t, m: model.New(), now: t0, sources: sources}
}

// apply normalizes and applies rows as one run, then moves the clock on.
func (w *world) apply(rows ...api.InputRow) {
	w.t.Helper()
	var ns []Normalized
	for _, r := range rows {
		n := Normalize(r, w.aliases)
		ns = append(ns, n)
	}
	Apply(w.m, ns, ApplyCtx{Now: w.now, RunID: "run-" + w.now.Format("1504"), Sources: w.sources, Aliases: w.aliases})
	w.now = w.now.Add(time.Hour)
}

func (w *world) lead(person string) api.LeadID {
	w.t.Helper()
	id, ok := Resolve(w.m, person)
	if !ok {
		w.t.Fatalf("no lead for %s", person)
	}
	return id
}

func (w *world) person(id api.LeadID) model.Person { return w.m.People[model.Key(id)] }

func (w *world) logKinds(kind string) int {
	n := 0
	for _, l := range w.m.Log {
		if l.Kind == kind {
			n++
		}
	}
	return n
}

func (w *world) override(person, action, value string) {
	w.m.Put(model.TableOverrides, model.Override{Person: person, Action: action, Value: value})
}

func (w *world) liveLeads() int {
	n := 0
	for _, p := range w.m.People {
		if p.MergedInto == "" {
			n++
		}
	}
	return n
}

// Identity scenarios: which two sightings are one person.
func TestTwoSightings(t *testing.T) {
	tests := []struct {
		name          string
		sources       []config.Source
		first, second api.InputRow
		wantOne       bool
	}{
		{name: "same email merges",
			first: in("apollo", "email", "priya@acme.com"), second: in("quiz", "email", "Priya@Acme.com"), wantOne: true},
		{name: "same linkedin url merges",
			first:   in("kubecon", "linkedin", "linkedin.com/in/priya"),
			second:  in("referral", "linkedin", "https://www.linkedin.com/in/priya/"),
			wantOne: true},
		{name: "same company domain and name merges when the source opts in",
			sources: []config.Source{{ID: "list-a", MatchDomainName: true}, {ID: "list-b", MatchDomainName: true}},
			first:   in("list-a", "domain", "acme.com", "name", "Priya R"),
			second:  in("list-b", "domain", "acme.com", "name", "priya  r"),
			wantOne: true},
		// Two real people can share a company and a name, and a duplicate is
		// recoverable where a false merge is not.
		{name: "same company domain and name stays split when only the second source opts in",
			sources: []config.Source{{ID: "list-a", MatchDomainName: true}, {ID: "list-b"}},
			first:   in("list-a", "domain", "acme.com", "name", "Priya R"),
			second:  in("list-b", "domain", "acme.com", "name", "Priya R", "linkedin", "linkedin.com/in/pr"),
			wantOne: false},
		{name: "different emails stay separate",
			first: in("apollo", "email", "priya@acme.com"), second: in("apollo", "email", "ravi@acme.com"), wantOne: false},
		// Email outranks domain + name, so two real people who share a name are
		// not collapsed because the weaker key matches.
		{name: "same name at one company but different emails stay separate",
			sources: []config.Source{{ID: "s", MatchDomainName: true}},
			first:   in("s", "email", "priya.r@acme.com", "domain", "acme.com", "name", "Priya R"),
			second:  in("s", "email", "priya.raj@acme.com", "domain", "acme.com", "name", "Priya R"),
			wantOne: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.sources...)
			w.apply(tt.first)
			w.apply(tt.second)
			if got := w.liveLeads() == 1; got != tt.wantOne {
				t.Errorf("one lead = %v (leads: %d), want %v", got, w.liveLeads(), tt.wantOne)
			}
		})
	}
}

// A later, poorer sighting never overwrites what the first knew, but fills
// what it lacked.
func TestFillsOnlyMissingFields(t *testing.T) {
	w := newWorld(t)
	w.apply(in("apollo", "email", "priya@acme.com", "company", "Acme", "title", "VP Engineering"))
	w.apply(in("apollo", "email", "priya@acme.com", "company", "acme inc", "name", "Priya R"))
	p := w.person(w.lead("priya@acme.com"))
	for f, want := range map[string]string{"company.name": "Acme", "title": "VP Engineering", "full_name": "Priya R"} {
		if got := p.Fields[f].Value; got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
}

// Proof: the unknown-email key conflict. A row whose email matches no lead but
// whose LinkedIn URL belongs to an existing lead becomes a new lead for that
// email; the URL is not written and the conflict is counted and logged.
func TestUnknownEmailWithAnotherLeadsLinkedIn(t *testing.T) {
	w := newWorld(t)
	w.apply(in("apollo", "email", "ravi@acme.com", "linkedin", "linkedin.com/in/shared"))
	w.apply(in("apollo", "email", "priya@acme.com", "linkedin", "linkedin.com/in/shared"))

	ravi, priya := w.lead("ravi@acme.com"), w.lead("priya@acme.com")
	if ravi == priya {
		t.Fatal("one wrong URL joined two people")
	}
	if got := w.lead("linkedin.com/in/shared"); got != ravi {
		t.Errorf("the URL moved to %s, want it to stay with %s", got, ravi)
	}
	if v := w.person(priya).Fields["linkedin_url"].Value; v != "" {
		t.Errorf("priya's linkedin_url = %q, want the conflicting fill skipped", v)
	}
	if w.m.StateValue("key_conflicts") != "1" || w.logKinds("key_conflict") != 1 {
		t.Errorf("key_conflicts = %q, logged %d; want 1 and 1", w.m.StateValue("key_conflicts"), w.logKinds("key_conflict"))
	}
	for _, l := range w.m.Log {
		if strings.Contains(l.Message, "@") || l.Email != "" {
			t.Errorf("log carries an email: %+v", l)
		}
	}
}

// An email-matched row whose LinkedIn URL belongs to another lead, or differs
// from the one the lead has, is applied to the email's lead without the URL.
func TestKnownEmailWithConflictingLinkedIn(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ravi@acme.com", "linkedin", "linkedin.com/in/ravi"))
	w.apply(in("a", "email", "priya@acme.com", "linkedin", "linkedin.com/in/priya"))
	w.apply(in("b", "email", "priya@acme.com", "linkedin", "linkedin.com/in/ravi", "title", "CTO"))
	w.apply(in("c", "email", "priya@acme.com", "linkedin", "linkedin.com/in/priya-2"))
	priya := w.lead("priya@acme.com")
	if w.person(priya).Fields["title"].Value != "CTO" {
		t.Error("the row must still apply to the email's lead")
	}
	if w.lead("linkedin.com/in/ravi") == priya {
		t.Error("ravi's URL moved to priya")
	}
	if _, ok := w.m.Identities[model.Key("linkedin.com/in/priya-2")]; ok {
		t.Error("a second URL for a lead that has one must not be written")
	}
	if w.m.StateValue("key_conflicts") != "2" {
		t.Errorf("key_conflicts = %q, want 2", w.m.StateValue("key_conflicts"))
	}
}

// A keyless row, and a domain + name row from a source with that matching off,
// are rejected, recorded once in Applied rows with no lead, and logged once.
func TestRejectsRecordedOnce(t *testing.T) {
	w := newWorld(t)
	bad := in("list", "company", "Acme", "title", "CTO")
	nameOnly := in("list", "domain", "acme.io", "name", "Ada")
	w.apply(bad, nameOnly)
	w.apply(bad, nameOnly)
	if len(w.m.People) != 0 {
		t.Errorf("a rejected row made %d leads", len(w.m.People))
	}
	if len(w.m.AppliedRows) != 2 || w.logKinds("row_rejected") != 2 {
		t.Errorf("applied rows %d, rejections logged %d; want 2 and 2 (once each)", len(w.m.AppliedRows), w.logKinds("row_rejected"))
	}
	for _, ar := range w.m.AppliedRows {
		if ar.LeadID != "" {
			t.Errorf("a reject is recorded with an empty lead id: %+v", ar)
		}
	}
}

// Re-applying unchanged rows records nothing, so sorting a tab changes nothing.
func TestReapplyingUnchangedRowsChangesNothing(t *testing.T) {
	w := newWorld(t)
	rows := []api.InputRow{
		in("a", "email", "priya@acme.com", "title", "CTO"),
		in("a", "linkedin", "linkedin.com/in/ravi", "name", "Ravi"),
	}
	w.apply(rows...)
	w.m.Committed(w.m.Writes())
	w.apply(rows[1], rows[0])
	if writes := w.m.Writes(); len(writes) != 0 {
		t.Errorf("re-applying recorded %d writes: %+v", len(writes), writes)
	}
}

// The same source and row id resolves ahead of every contact key: a receiver
// contact correcting its email stays one lead, and the correction becomes the
// primary email while the old address stays an identity.
func TestSameSourceCorrection(t *testing.T) {
	w := newWorld(t)
	w.apply(in("receiver", "contact_id", "c-1", "email", "typo@acme.io", "name", "Dana Q"))
	w.apply(in("receiver", "contact_id", "c-1", "email", "dana@acme.io", "name", "Dana Q"))
	if w.liveLeads() != 1 {
		t.Fatalf("a corrected answer made %d leads", w.liveLeads())
	}
	lead := w.lead("typo@acme.io")
	if w.lead("dana@acme.io") != lead {
		t.Fatal("the corrected email resolves to another lead")
	}
	x := NewIndex(w.m, w.sources)
	if got := x.PrimaryEmail(lead); got != "dana@acme.io" {
		t.Errorf("primary = %q, want the source's own correction", got)
	}
	if got := x.Emails(lead); !reflect.DeepEqual(got, []string{"dana@acme.io", "typo@acme.io"}) {
		t.Errorf("emails = %v: every email is kept, primary first", got)
	}
}

// The correction is refused when another source vouched for the current
// address: two sources disagreeing is the false-merge signal, and silently
// taking the newer one would erase it.
func TestCorrectionRefusedWhenAnotherSourceVouched(t *testing.T) {
	for name, order := range map[string][]api.InputRow{
		"another source gave it first": {
			in("list", "email", "shared@acme.io"),
			in("receiver", "contact_id", "c-9", "email", "shared@acme.io"),
		},
		"another source joined later": {
			in("receiver", "contact_id", "c-9", "email", "shared@acme.io"),
			in("list", "email", "shared@acme.io"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			for _, r := range order {
				w.apply(r)
			}
			w.apply(in("receiver", "contact_id", "c-9", "email", "other@acme.io"))
			lead := w.lead("shared@acme.io")
			if got := NewIndex(w.m, w.sources).PrimaryEmail(lead); got != "shared@acme.io" {
				t.Errorf("primary = %q, want shared@acme.io: another source vouched for it", got)
			}
			if w.lead("other@acme.io") != lead {
				t.Error("the new address is still an identity of the same lead")
			}
		})
	}
}

// The company domain: derived from a work email when absent (and marked so),
// never from a personal one, filled when it arrives late, and never re-pointed.
func TestCompanyDomain(t *testing.T) {
	w := newWorld(t)
	w.apply(
		in("a", "email", "ada@acme.io"),
		in("a", "email", "bo@gmail.com"),
		in("a", "linkedin", "linkedin.com/in/cy"),
		in("a", "email", "di@acme.io", "website", "https://www.di-corp.com/"),
	)
	if f := w.person(w.lead("ada@acme.io")).Fields["company.domain"]; f.Value != "acme.io" || !f.Derived {
		t.Errorf("ada's domain = %+v, want acme.io derived", f)
	}
	if f := w.person(w.lead("bo@gmail.com")).Fields["company.domain"]; f.Value != "" {
		t.Errorf("a personal address names no company, got %q", f.Value)
	}
	if f := w.person(w.lead("di@acme.io")).Fields["company.domain"]; f.Value != "di-corp.com" || f.Derived {
		t.Errorf("a row's own domain wins over deriving: %+v", f)
	}

	w.apply(in("b", "linkedin", "linkedin.com/in/cy", "domain", "cy.io"))
	cy := w.lead("linkedin.com/in/cy")
	if got := w.person(cy).Fields["company.domain"].Value; got != "cy.io" {
		t.Errorf("late-arriving domain = %q, want cy.io filled", got)
	}
	if got := w.m.PeopleAt("cy.io"); !reflect.DeepEqual(got, []api.LeadID{cy}) {
		t.Errorf("people at cy.io = %v", got)
	}

	w.apply(in("c", "email", "ada@acme.io", "domain", "elsewhere.io"))
	ada := w.person(w.lead("ada@acme.io"))
	if got := ada.Fields["company.domain"].Value; got != "acme.io" {
		t.Errorf("the domain was re-pointed to %q", got)
	}
	if got := ada.Conflicts["company.domain"]; len(got) != 1 || got[0].Value != "elsewhere.io" {
		t.Errorf("the disagreement is recorded: %+v", got)
	}
}

// Proof: a recorded conflict. A different value from another source is kept
// in People.conflicts and the first value stays; the same source changing its
// value is not a conflict.
func TestRecordedConflict(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ada@acme.io", "segment", "AI-native"))
	// The same source changing its value: the row re-applies, nothing changes.
	w.apply(in("a", "email", "ada@acme.io", "segment", "Data-curious"))
	if p := w.person(w.lead("ada@acme.io")); len(p.Conflicts) != 0 || p.Fields["segment"].Value != "AI-native" {
		t.Fatalf("same source edit: segment %q, conflicts %v; want the first value and no conflict",
			p.Fields["segment"].Value, p.Conflicts)
	}
	w.apply(in("b", "email", "ada@acme.io", "segment", "Legacy"))
	w.apply(in("b", "email", "ada@acme.io", "segment", "Legacy", "note", "x")) // same row again, edited
	w.apply(in("c", "email", "ada@acme.io", "segment", "AI-native"))
	p := w.person(w.lead("ada@acme.io"))
	if p.Fields["segment"].Value != "AI-native" {
		t.Errorf("kept value = %q, want the first", p.Fields["segment"].Value)
	}
	want := []model.Conflict{{Value: "Legacy", SourceID: "b"}}
	if !reflect.DeepEqual(p.Conflicts["segment"], want) {
		t.Errorf("conflicts = %+v, want %+v", p.Conflicts["segment"], want)
	}
}

// Proof: an alias change re-applies rows. The rubric starts reading a column
// under a new name; the row's hash changes, so it is applied again and the lead
// gains the field without re-importing.
func TestAliasChangeReappliesRows(t *testing.T) {
	w := newWorld(t)
	r := in("a", "email", "ada@acme.io", "Current tool", "Looker")
	w.apply(r)
	lead := w.lead("ada@acme.io")
	if w.person(lead).Fields["currenttool"].Value != "Looker" {
		t.Fatalf("an unaliased header is kept under its squashed name: %v", w.person(lead).Fields)
	}
	w.m.Committed(w.m.Writes())
	w.apply(r)
	if len(w.m.Writes()) != 0 {
		t.Fatal("an unchanged row with unchanged aliases must not re-apply")
	}
	w.aliases = map[string]string{"currenttool": "uses_competitor"}
	w.apply(r)
	if got := w.person(lead).Fields["uses_competitor"].Value; got != "Looker" {
		t.Errorf("uses_competitor = %q: the alias change must re-apply the row", got)
	}
}

// Proof: the three built-in fields merge produces.
func TestBuiltInFields(t *testing.T) {
	w := newWorld(t,
		config.Source{ID: "kubecon", Channel: "conference"},
		config.Source{ID: "saastr", Channel: "conference"},
		config.Source{ID: "crm"},
	)
	w.apply(
		in("kubecon", "email", "ada@acme.io"),
		in("saastr", "email", "ada@acme.io"),
		in("receiver", "contact_id", "c-1", "email", "ada@acme.io"),
		in("receiver", "contact_id", "c-2", "email", "bo@acme.io"),
		in("crm", "email", "cy@beta.io"),
	)
	x := NewIndex(w.m, w.sources)
	ada, bo, cy := w.lead("ada@acme.io"), w.lead("bo@acme.io"), w.lead("cy@beta.io")
	if got := x.SourcesSeen(ada); got != 2 {
		t.Errorf("ada sources_seen = %d, want 2 (two conference CSVs count once, plus the receiver)", got)
	}
	if !x.ReceiverOnly(bo) || x.ReceiverOnly(ada) || x.ReceiverOnly(cy) {
		t.Errorf("receiver_only: bo %v ada %v cy %v; want true false false", x.ReceiverOnly(bo), x.ReceiverOnly(ada), x.ReceiverOnly(cy))
	}
	if got := x.LeadsSeen(); got["acme.io"] != 2 || got["beta.io"] != 1 {
		t.Errorf("leads_seen = %v", got)
	}

	w.apply(in("crm", "email", "bo@acme.io"))
	w.override("bo@acme.io", "same_as", "ada@acme.io")
	w.apply()
	x = NewIndex(w.m, w.sources)
	if x.ReceiverOnly(ada) || x.LeadsSeen()["acme.io"] != 1 {
		t.Errorf("after the merge: receiver_only %v, leads_seen %v", x.ReceiverOnly(ada), x.LeadsSeen())
	}
	if got := x.SourcesSeen(ada); got != 3 {
		t.Errorf("sources_seen counts the absorbed lead's channels: %d, want 3", got)
	}
}

// Proof: a source switched to apollo_held marks every lead it already
// supplied, not only leads from new rows.
func TestApolloHeldBackfill(t *testing.T) {
	w := newWorld(t, config.Source{ID: "apollo-export"}, config.Source{ID: "crm"})
	w.apply(in("apollo-export", "email", "ada@acme.io"), in("crm", "email", "bo@acme.io"))
	if !w.person(w.lead("ada@acme.io")).ApolloHeldAt.IsZero() {
		t.Fatal("held before the flag was on")
	}
	w.sources[0].ApolloHeld = true
	at := w.now
	w.apply() // no new rows
	if got := w.person(w.lead("ada@acme.io")).ApolloHeldAt; !got.Equal(at) {
		t.Errorf("apollo_held_at = %v, want %v", got, at)
	}
	if !w.person(w.lead("bo@acme.io")).ApolloHeldAt.IsZero() {
		t.Error("a lead the held source never supplied was marked")
	}
	w.apply()
	if got := w.person(w.lead("ada@acme.io")).ApolloHeldAt; !got.Equal(at) {
		t.Error("apollo_held_at is never moved once set")
	}
}

// Proof: a namesake pair resolved both ways, and a third namesake blocked
// until paired with each.
func TestNamesakes(t *testing.T) {
	setup := func(t *testing.T) *world {
		w := newWorld(t)
		w.apply(
			in("a", "email", "priya.r@acme.io", "name", "Priya R"),
			in("a", "email", "priya.raj@acme.io", "name", "priya  r"),
			in("a", "email", "ravi@acme.io", "name", "Ravi"),
		)
		return w
	}
	t.Run("unresolved", func(t *testing.T) {
		w := setup(t)
		got := Duplicates(w.m)
		if len(got) != 2 || !got[w.lead("priya.r@acme.io")] || !got[w.lead("priya.raj@acme.io")] {
			t.Errorf("duplicates = %v, want both Priyas", got)
		}
	})
	t.Run("distinct keeps them apart", func(t *testing.T) {
		w := setup(t)
		w.override("Priya.R@acme.io", "distinct", "priya.raj@acme.io")
		w.apply()
		if got := Duplicates(w.m); len(got) != 0 {
			t.Errorf("duplicates = %v, want none", got)
		}
		if w.liveLeads() != 3 {
			t.Error("distinct must not merge")
		}
		// A third namesake is blocked until paired with each.
		w.apply(in("b", "email", "p.r@acme.io", "name", "Priya R"))
		if got := Duplicates(w.m); len(got) != 3 {
			t.Errorf("with a third namesake duplicates = %v, want all three", got)
		}
		w.override("p.r@acme.io", "distinct", "priya.r@acme.io")
		w.override("p.r@acme.io", "distinct", "priya.raj@acme.io")
		if got := Duplicates(w.m); len(got) != 0 {
			t.Errorf("paired with each: duplicates = %v, want none", got)
		}
	})
	t.Run("same_as merges them", func(t *testing.T) {
		w := setup(t)
		older, newer := w.lead("priya.r@acme.io"), w.lead("priya.raj@acme.io")
		w.override("priya.raj@acme.io", "same_as", "priya.r@acme.io")
		w.apply()
		if got := w.person(newer).MergedInto; got != older {
			t.Errorf("merged_into = %q, want the older lead %q", got, older)
		}
		if got := Duplicates(w.m); len(got) != 0 {
			t.Errorf("duplicates = %v, want none", got)
		}
		if w.lead("priya.raj@acme.io") != older {
			t.Error("the absorbed lead's email resolves to the survivor")
		}
	})
}

// Proof: an opted-out lead merged by same_as into a clean one. The survivor
// reads as unsubscribed through the fold's chain rule (contracts section 7),
// checked here with a stub fold that reads Outcomes across the family.
func TestOptOutSurvivesSameAs(t *testing.T) {
	stubFold := func(m *model.Model, lead api.LeadID) string {
		for _, id := range NewIndex(m, nil).Family(lead) {
			if !m.Outcomes[model.Key(id)].UnsubscribedAt.IsZero() {
				return "unsubscribed"
			}
		}
		return "new"
	}
	for name, optedOutFirst := range map[string]bool{"opted-out lead is absorbed": false, "opted-out lead survives": true} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if optedOutFirst {
				w.apply(in("a", "email", "old@acme.io", "title", "CTO"))
				w.apply(in("b", "email", "new@acme.io", "title", "VP", "segment", "AI"))
			} else {
				w.apply(in("b", "email", "new@acme.io", "title", "VP", "segment", "AI"))
				w.apply(in("a", "email", "old@acme.io", "title", "CTO"))
			}
			optedOut := w.lead("old@acme.io")
			w.m.Put(model.TableOutcomes, model.Outcome{LeadID: optedOut, UnsubscribedAt: t0, UnsubscribedOrigin: "event"})
			clean := w.lead("new@acme.io")
			if stubFold(w.m, clean) != "new" {
				t.Fatal("setup: the clean lead starts clean")
			}
			w.override("old@acme.io", "same_as", "new@acme.io")
			w.apply()

			survivor := w.lead("new@acme.io")
			if w.lead("old@acme.io") != survivor {
				t.Fatal("same_as did not make one lead")
			}
			if got := stubFold(w.m, survivor); got != "unsubscribed" {
				t.Errorf("survivor folds to %q: an opt-out must follow the merge", got)
			}
			if _, ok := w.m.Outcomes[model.Key(optedOut)]; !ok {
				t.Error("the opted-out lead's Outcomes row stays under its own id")
			}
			if NewIndex(w.m, w.sources).Emails(survivor)[0] == "" || len(NewIndex(w.m, w.sources).Emails(survivor)) != 2 {
				t.Errorf("the survivor carries both emails: %v", NewIndex(w.m, w.sources).Emails(survivor))
			}
		})
	}
}

// The same_as fill: survivor fields fill from the absorbed lead, disagreements
// are recorded, apollo_held_at and first_seen take the earliest values.
func TestSameAsFill(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "old@acme.io", "title", "CTO"))
	w.apply(in("b", "email", "new@acme.io", "title", "VP", "segment", "AI"))
	old, nw := w.lead("old@acme.io"), w.lead("new@acme.io")
	p := w.person(nw)
	p.ApolloHeldAt = t0.Add(-time.Hour)
	p.FirstSeen = map[string]time.Time{"visit_pricing": t0.Add(-2 * time.Hour)}
	w.m.Put(model.TablePeople, p)
	q := w.person(old)
	q.FirstSeen = map[string]time.Time{"visit_pricing": t0, "sent": t0}
	w.m.Put(model.TablePeople, q)

	w.override("new@acme.io", "same_as", "old@acme.io")
	w.apply()
	s := w.person(old)
	if s.Fields["title"].Value != "CTO" || s.Fields["segment"].Value != "AI" {
		t.Errorf("fields = %v", s.Fields)
	}
	if got := s.Conflicts["title"]; len(got) != 1 || got[0].Value != "VP" {
		t.Errorf("conflicts = %v", s.Conflicts)
	}
	if !s.ApolloHeldAt.Equal(t0.Add(-time.Hour)) {
		t.Errorf("apollo_held_at = %v", s.ApolloHeldAt)
	}
	if !s.FirstSeen["visit_pricing"].Equal(t0.Add(-2*time.Hour)) || !s.FirstSeen["sent"].Equal(t0) {
		t.Errorf("first_seen = %v", s.FirstSeen)
	}
	if w.logKinds("merged") != 1 {
		t.Error("the merge is logged")
	}
	w.apply()
	if w.logKinds("merged") != 1 {
		t.Error("a merge applies once")
	}
}

// A same_as row naming a person not yet known waits, and merges when the
// person appears.
func TestSameAsWaitsForThePerson(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ada@acme.io"))
	w.override("ada@acme.io", "same_as", "ada.l@acme.io")
	w.apply()
	if w.liveLeads() != 1 || len(ParseOverrides(w.m).Unmatched) != 1 {
		t.Fatal("the row waits")
	}
	w.apply(in("b", "email", "ada.l@acme.io"))
	if w.liveLeads() != 1 || len(w.m.People) != 2 {
		t.Errorf("live %d of %d: the row applies when the person appears", w.liveLeads(), len(w.m.People))
	}
}

// Company facts: Companies tab first, then enrichment, then the first input
// value; a source never replaces a higher origin.
func TestCompanyFactOrigins(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ada@acme.io", "company", "Acme", "employees", "40", "country", "India"))
	w.apply(in("b", "email", "bo@acme.io", "company", "ACME Inc", "funding", "Series B"))
	cf := w.m.CompanyFacts[model.Key("acme.io")]
	for f, want := range map[string]string{"name": "Acme", "employees": "40", "region": "India", "funding_stage": "Series B"} {
		if got := cf.Facts[f]; got.Value != want || got.Origin != OriginInput {
			t.Errorf("%s = %+v, want %q from input", f, got, want)
		}
	}

	// An enrichment value is never replaced by input.
	cf = w.m.CompanyFacts[model.Key("acme.io")]
	cf = cloneCompany(cf)
	cf.Facts["employees"] = model.Fact{Value: "55", Origin: OriginEnrichment, At: t0}
	w.m.Put(model.TableCompanyFacts, cf)
	w.apply(in("c", "email", "cy@acme.io", "employees", "60"))
	if got := w.m.CompanyFacts[model.Key("acme.io")].Facts["employees"]; got.Value != "55" {
		t.Errorf("employees = %+v, input replaced enrichment", got)
	}

	// The Companies tab wins: a changed value moves the old one to previous; the
	// same value is taken over without counting as a change.
	w.m.Load(model.TableCompanies, []api.Row{
		{"Website": "https://www.acme.io", "Headcount": "70", "Name": "Acme Corp", "Region": "India", "Tier note": "key"},
		{"domain": "acme.io", "Headcount": "1"}, // a second row for the domain is ignored
	})
	at := w.now
	w.apply()
	cf = w.m.CompanyFacts[model.Key("acme.io")]
	if got := cf.Facts["employees"]; got.Value != "70" || got.Origin != OriginCompaniesTab || !got.At.Equal(at) {
		t.Errorf("employees = %+v", got)
	}
	if got := cf.Previous["employees"]; got.Value != "55" {
		t.Errorf("previous employees = %+v", got)
	}
	if got := cf.Facts["name"]; got.Value != "Acme Corp" {
		t.Errorf("name = %+v: a Name column in Companies is the company's name", got)
	}
	if got := cf.Facts["region"]; got.Value != "India" || got.Origin != OriginCompaniesTab || !got.At.Equal(t0) {
		t.Errorf("region = %+v: same value, taken over, not a change", got)
	}
	if _, moved := cf.Previous["region"]; moved {
		t.Error("an unchanged value never moves to previous")
	}
	if cf.Facts["tiernote"].Value != "key" {
		t.Errorf("other columns are facts by squashed name: %v", cf.Facts)
	}
	w.m.Committed(w.m.Writes())
	w.apply()
	if len(w.m.Writes()) != 0 {
		t.Error("re-applying the Companies tab changes nothing")
	}
}

// A sighting whose domain differs from the lead's describes another company,
// so its company columns are not credited to the lead's company.
func TestRowFactsOnlyForTheLeadsCompany(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ada@acme.io"))
	w.apply(in("b", "email", "ada@acme.io", "domain", "other.io", "employees", "9000"))
	if _, ok := w.m.CompanyFacts[model.Key("acme.io")].Facts["employees"]; ok {
		t.Error("another company's headcount was credited to acme.io")
	}
	if _, ok := w.m.CompanyFacts[model.Key("other.io")]; ok {
		t.Error("the lead does not work at other.io")
	}
}

func TestFindPerson(t *testing.T) {
	w := newWorld(t)
	w.apply(
		in("receiver", "contact_id", "c-1", "email", "ada@acme.io"),
		in("a", "email", "bo@acme.io", "linkedin", "linkedin.com/in/bo"),
	)
	w.m.Committed(w.m.Writes())
	ada, bo := w.lead("ada@acme.io"), w.lead("bo@acme.io")
	ev := func(email, li, cid string) api.Event {
		return NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: email, LinkedInURL: li,
			Attrs: map[string]string{"contact_id": cid}, ReceivedAt: t0})
	}
	for name, tt := range map[string]struct {
		e    api.Event
		want api.LeadID
	}{
		"by contact id":                   {ev("", "", "c-1"), ada},
		"contact id wins over email":      {ev("bo@acme.io", "", "c-1"), ada},
		"by email, any case":              {ev("Bo@Acme.io", "", ""), bo},
		"by linkedin":                     {ev("", "https://linkedin.com/in/bo/", ""), bo},
		"unknown email never by linkedin": {ev("stranger@acme.io", "linkedin.com/in/bo", ""), ""},
		"company-only":                    {api.Event{Kind: "visit_pricing", Domain: "acme.io"}, ""},
	} {
		got, ok := FindPerson(w.m, tt.e)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: %q %v, want %q", name, got, ok, tt.want)
		}
	}
	if len(w.m.Writes()) != 0 {
		t.Error("FindPerson writes nothing")
	}

	// merged_into is followed.
	w.override("bo@acme.io", "same_as", "ada@acme.io")
	w.apply()
	if got, _ := FindPerson(w.m, ev("", "linkedin.com/in/bo", "")); got != ada {
		t.Errorf("after the merge: %q, want the survivor %q", got, ada)
	}
}

func TestApplyEventPerson(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "bo@acme.io", "linkedin", "linkedin.com/in/bo"))
	bo := w.lead("bo@acme.io")

	e := NormalizeEventKeys(api.Event{Kind: "visit_pricing", Email: "Stranger@Beta.io", LinkedInURL: "linkedin.com/in/bo",
		ReceivedAt: t0, Attrs: map[string]string{"contact_id": "c-7", "full_name": "S Tranger", "title": "CEO", "company": "Beta"}})
	id := ApplyEventPerson(w.m, e)
	if id == "" || id == bo {
		t.Fatalf("an unknown email makes a new lead, got %q", id)
	}
	p := w.person(id)
	if p.Fields["linkedin_url"].Value != "" || w.lead("linkedin.com/in/bo") != bo {
		t.Error("another lead's LinkedIn URL is not written")
	}
	if w.m.StateValue("key_conflicts") != "1" || w.logKinds("key_conflict") != 1 {
		t.Error("the skipped URL counts as a key conflict and is logged")
	}
	for _, l := range w.m.Log {
		if l.Kind == "key_conflict" && (l.LeadID != id || l.Email != "" || strings.Contains(l.Message, "@")) {
			t.Errorf("the log row carries the lead id only: %+v", l)
		}
	}
	if p.Fields["company.domain"].Value != "beta.io" || !p.Fields["company.domain"].Derived ||
		p.Fields["full_name"].Value != "S Tranger" || p.Fields["company.name"].Value != "Beta" {
		t.Errorf("fields = %v", p.Fields)
	}
	if got := ApplyEventPerson(w.m, e); got != id {
		t.Error("the second event finds the lead it created")
	}
	if got, _ := FindPerson(w.m, api.Event{Attrs: map[string]string{"contact_id": "c-7"}}); got != id {
		t.Error("the contact id finds the created lead")
	}
	if NewIndex(w.m, w.sources).ReceiverOnly(id) != true {
		t.Error("a lead created for an event is receiver-only")
	}
	if got := ApplyEventPerson(w.m, api.Event{Kind: "visit_pricing", Domain: "acme.io"}); got != "" {
		t.Errorf("a company-only event names no person, got %q", got)
	}
	// The receiver row for the contact later applies to the same lead.
	w.apply(in("receiver", "contact_id", "c-7", "email", "stranger@beta.io", "title", "Founder"))
	if w.liveLeads() != 2 {
		t.Errorf("leads = %d, want 2", w.liveLeads())
	}
}

func TestIndexKeys(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "linkedin", "linkedin.com/in/ada"))
	w.apply(in("b", "email", "bo@acme.io"))
	x := NewIndex(w.m, w.sources)
	ada, bo := w.lead("linkedin.com/in/ada"), w.lead("bo@acme.io")
	if x.PersonKey(ada) != "linkedin.com/in/ada" || x.PersonKey(bo) != "bo@acme.io" {
		t.Errorf("person keys %q %q", x.PersonKey(ada), x.PersonKey(bo))
	}
	if got := x.LiveLeads(); len(got) != 2 {
		t.Errorf("live leads %v", got)
	}
	if got := x.Family(ada); !reflect.DeepEqual(got, []api.LeadID{ada}) {
		t.Errorf("family %v", got)
	}
}
