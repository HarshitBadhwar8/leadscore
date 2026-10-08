package merge

import (
	"fmt"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func sourceWithDomainName(id string) config.Source {
	return config.Source{ID: id, Channel: id, MatchDomainName: true}
}

// settle commits the model's changes, as a run's save would.
func (w *world) settle() { w.m.Committed(w.m.Writes()) }

// One row id twice in a source (the same email on two lines, or two rows with
// the same bad email) applies as one group under one hash: the second and
// third runs apply nothing and log nothing, so the run's backlog empties.
func TestDuplicateRowIDsSettle(t *testing.T) {
	w := newWorld(t)
	rows := []api.InputRow{
		in("list", "email", "ada@acme.example", "linkedin", "linkedin.com/in/ada"),
		in("list", "email", "ada@acme.example", "linkedin", "linkedin.com/in/ada-2"),
		in("list", "email", "{{email}}", "name", "X"),
		in("list", "email", "{{email}}", "name", "Y"),
	}
	var ns []Normalized
	for _, r := range rows {
		ns = append(ns, Normalize(r, nil))
	}
	w.apply(rows...)
	w.settle()
	logs, conflicts := len(w.m.Log), w.m.StateValue("key_conflicts")
	if conflicts != "1" || w.logKinds("row_rejected") != 1 {
		t.Fatalf("run 1: key_conflicts %q, rejects logged %d; want 1 and 1", conflicts, w.logKinds("row_rejected"))
	}
	for run := 2; run <= 3; run++ {
		w.apply(rows...)
		if writes := w.m.Writes(); len(writes) != 0 {
			t.Errorf("run %d re-applied: %+v", run, writes)
		}
		if len(w.m.Log) != logs || w.m.StateValue("key_conflicts") != "1" {
			t.Errorf("run %d logged again or counted again", run)
		}
		if p := Pending(w.m, ns); len(p) != 0 {
			t.Errorf("run %d: backlog %d groups, want none", run, len(p))
		}
	}
	if w.liveLeads() != 1 || len(w.m.AppliedRows) != 2 {
		t.Errorf("leads %d, applied rows %d; want 1 and 2", w.liveLeads(), len(w.m.AppliedRows))
	}
}

// A long same_as chain stays one person: every merge re-points the leads
// already absorbed, so no walk runs past its bound and an opt-out at one end
// reaches the other.
func TestLongSameAsChain(t *testing.T) {
	w := newWorld(t)
	const n = 70
	var rows []api.InputRow
	for i := 0; i < n; i++ {
		rows = append(rows, in("a", "email", fmt.Sprintf("p%d@acme.example", i)))
	}
	w.apply(rows...)
	// Newest first, so without re-pointing each merge would lengthen the chain.
	for i := n - 2; i >= 0; i-- {
		w.override(fmt.Sprintf("p%d@acme.example", i), "same_as", fmt.Sprintf("p%d@acme.example", i+1))
	}
	w.apply()
	if got := w.liveLeads(); got != 1 {
		t.Fatalf("live leads = %d, want 1", got)
	}
	last := w.lead("p69@acme.example")
	w.m.Put(model.TableOutcomes, model.Outcome{LeadID: w.idOf("p69@acme.example"), UnsubscribedAt: t0})
	survivor := w.lead("p0@acme.example")
	if last != survivor {
		t.Fatal("the chain's ends are different leads")
	}
	found := false
	for _, id := range NewIndex(w.m, nil).Family(survivor) {
		found = found || !w.m.Outcomes[model.Key(id)].UnsubscribedAt.IsZero()
	}
	if !found {
		t.Error("the opt-out on p69 does not reach the survivor's family")
	}
	for _, p := range w.m.People {
		if p.MergedInto != "" && w.person(p.MergedInto).MergedInto != "" {
			t.Errorf("%s points at a merged lead: chains must stay one step deep", p.LeadID)
		}
	}
}

// idOf is the lead that owns an identity, not following merged_into.
func (w *world) idOf(key string) api.LeadID { return w.m.Identities[model.Key(key)].LeadID }

// A hand-edited merged_into cycle resolves, is named by Cycles, and blocks
// every lead in it.
func TestMergeCycle(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ada@acme.example"), in("a", "email", "bo@acme.example"), in("a", "email", "cy@acme.example"))
	a, b, c := w.lead("ada@acme.example"), w.lead("bo@acme.example"), w.lead("cy@acme.example")
	for from, to := range map[api.LeadID]api.LeadID{a: b, b: a, c: b} {
		p := w.person(from)
		p.MergedInto = to
		w.m.Put(model.TablePeople, p)
	}
	if Live(w.m, a) != Live(w.m, b) || Live(w.m, c) != Live(w.m, a) {
		t.Errorf("live: %s %s %s, want one lead", Live(w.m, a), Live(w.m, b), Live(w.m, c))
	}
	cyc, dups := Cycles(w.m), Duplicates(w.m)
	for _, id := range []api.LeadID{a, b, c} {
		if !cyc[id] || !dups[id] {
			t.Errorf("%s: in cycle %v, blocked %v; want both", id, cyc[id], dups[id])
		}
	}
	w.override("ada@acme.example", "same_as", "bo@acme.example")
	w.apply() // must not hang or merge further
}

// Only a linkedin.com profile URL is a LinkedIn key, and every spelling of one
// profile is one key.
func TestCanonicalLinkedIn(t *testing.T) {
	const want = "linkedin.com/in/ada-l"
	for _, v := range []string{
		"linkedin.com/in/ada-l",
		"https://www.linkedin.com/in/ada-l/",
		"http://linkedin.com/in/ada-l",
		"HTTPS://WWW.LINKEDIN.COM/in/Ada-L",
		"https://in.linkedin.com/in/ada-l",
		"https://uk.linkedin.com/in/ada-l/",
		"https://m.linkedin.com/in/ada-l",
		"https://www.linkedin.com/in/ada-l?trk=public_profile",
		"https://www.linkedin.com/in/ada-l/?originalSubdomain=de",
		"https://www.linkedin.com/in/ada-l#experience",
		"https://www.linkedin.com/in/ada-l/en",
		"https://www.linkedin.com/in/ada-l/en/",
		"https://www.linkedin.com//in//ada-l//",
		"www.linkedin.com/in/ada-l",
		" linkedin.com:443/in/ada-l ",
	} {
		if got := CanonicalLinkedIn(v); got != want {
			t.Errorf("CanonicalLinkedIn(%q) = %q, want %q", v, got, want)
		}
	}
	if got := CanonicalLinkedIn("https://www.linkedin.com/pub/ada-l/1a/2b/3c/en"); got != "linkedin.com/pub/ada-l/1a/2b/3c" {
		t.Errorf("pub URL = %q", got)
	}
	for _, v := range []string{"N/A", "-", "none", "n/a", "linkedin", "https://linkedin.com/", "https://linkedin.com/in/",
		"https://www.linkedin.com/company/acme", "https://notlinkedin.example/in/ada", "https://linkedin.com.evil.example/in/ada", "ada"} {
		if got := CanonicalLinkedIn(v); got != "" {
			t.Errorf("CanonicalLinkedIn(%q) = %q, want no key", v, got)
		}
	}
	e := NormalizeEventKeys(api.Event{LinkedInURL: "https://uk.linkedin.com/in/Ada-L/?trk=x"})
	if e.LinkedInURL != want {
		t.Errorf("event key = %q", e.LinkedInURL)
	}
}

// Placeholder LinkedIn values are not keys, so they never join strangers.
func TestPlaceholderLinkedInIsNoKey(t *testing.T) {
	w := newWorld(t)
	w.apply(
		in("a", "email", "ada@acme.example", "linkedin", "N/A"),
		in("a", "email", "bo@acme.example", "linkedin", "N/A"),
		in("a", "linkedin", "-", "name", "Cy"),
		in("a", "linkedin", "-", "name", "Di"),
	)
	if w.liveLeads() != 2 || w.lead("ada@acme.example") == w.lead("bo@acme.example") {
		t.Errorf("leads = %d: a placeholder joined two people", w.liveLeads())
	}
	if len(w.m.Identities) != 2 || w.m.StateValue("key_conflicts") != "" {
		t.Errorf("identities %v, key_conflicts %q: a placeholder is no key", w.m.Identities, w.m.StateValue("key_conflicts"))
	}
	if w.logKinds("row_rejected") != 2 {
		t.Errorf("rows whose only key is a placeholder are rejected: %d logged", w.logKinds("row_rejected"))
	}
}

// The domain + name rung matches only when exactly one live lead fits; with a
// namesake pair (even one marked distinct) the row makes a new lead, which
// Duplicates then blocks.
func TestDomainNameRungNeedsOneMatch(t *testing.T) {
	w := newWorld(t)
	w.sources = append(w.sources, sourceWithDomainName("conf"))
	w.apply(
		in("crm", "email", "priya.r@acme.example", "name", "Priya R"),
		in("crm", "email", "priya.raj@acme.example", "name", "Priya R"),
	)
	w.override("priya.r@acme.example", "distinct", "priya.raj@acme.example")
	w.apply(in("conf", "domain", "acme.example", "name", "Priya R", "title", "CTO"))
	if w.liveLeads() != 3 {
		t.Fatalf("leads = %d: the row must not pick one namesake", w.liveLeads())
	}
	for _, e := range []string{"priya.r@acme.example", "priya.raj@acme.example"} {
		if w.person(w.lead(e)).Fields["title"].Value != "" {
			t.Errorf("%s gained the row's title", e)
		}
	}
	if got := Duplicates(w.m); len(got) != 3 {
		t.Errorf("duplicates = %v, want all three until paired", got)
	}
}

// An event's malformed email counts as missing: its LinkedIn URL finds the
// lead, so an opt-out is not lost, and a second identical event finds the lead
// the first one created.
func TestMalformedEventEmail(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "bo@acme.example", "linkedin", "linkedin.com/in/bo"))
	bo := w.lead("bo@acme.example")
	e := NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: "bo@acme", LinkedInURL: "https://www.linkedin.com/in/bo/", ReceivedAt: t0})
	if got, ok := FindPerson(w.m, e); !ok || got != bo {
		t.Errorf("FindPerson = %q %v, want bo", got, ok)
	}
	if got := ApplyEventPerson(w.m, e); got != bo {
		t.Errorf("ApplyEventPerson = %q, want bo", got)
	}

	f := NormalizeEventKeys(api.Event{Kind: "unsubscribed", Email: "cy@acme", LinkedInURL: "linkedin.com/in/cy", ReceivedAt: t0})
	first := ApplyEventPerson(w.m, f)
	if first == "" || first == bo {
		t.Fatalf("a new person by LinkedIn: %q", first)
	}
	if second := ApplyEventPerson(w.m, f); second != first {
		t.Errorf("the second event made %q, want %q", second, first)
	}
	if _, ok := w.m.Identities[model.Key("cy@acme")]; ok {
		t.Error("a malformed email is never an identity")
	}
}

// A lead created only by an event, with an email and no contact id, is
// receiver-only with one source.
func TestEventOnlyLeadIsReceiverOnly(t *testing.T) {
	w := newWorld(t)
	id := ApplyEventPerson(w.m, NormalizeEventKeys(api.Event{Kind: "visit_pricing", Email: "ada@acme.example", ReceivedAt: t0}))
	x := NewIndex(w.m, w.sources)
	if !x.ReceiverOnly(id) || x.SourcesSeen(id) != 1 {
		t.Errorf("receiver_only %v, sources_seen %d; want true and 1", x.ReceiverOnly(id), x.SourcesSeen(id))
	}
}

// A Companies cell emptied, or a row removed, takes the fact out of `facts`
// (into `previous`) so a lower origin can fill it.
func TestCompaniesTabRemovals(t *testing.T) {
	w := newWorld(t)
	_ = w.m.Load(model.TableCompanies, []api.Row{
		{"domain": "acme.example", "employees": "70", "region": "India"},
		{"domain": "beta.example", "employees": "9"},
	})
	w.apply()
	_ = w.m.Load(model.TableCompanies, []api.Row{{"domain": "acme.example", "employees": "", "region": "India"}})
	w.apply()
	acme, beta := w.m.CompanyFacts[model.Key("acme.example")], w.m.CompanyFacts[model.Key("beta.example")]
	if _, ok := acme.Facts["employees"]; ok || acme.Previous["employees"].Value != "70" {
		t.Errorf("acme: facts %v previous %v", acme.Facts, acme.Previous)
	}
	if acme.Facts["region"].Value != "India" {
		t.Error("a cell still filled stays")
	}
	if _, ok := beta.Facts["employees"]; ok || beta.Previous["employees"].Value != "9" {
		t.Errorf("beta: facts %v previous %v", beta.Facts, beta.Previous)
	}
	w.apply(in("a", "email", "ada@acme.example", "employees", "80"))
	if got := w.m.CompanyFacts[model.Key("acme.example")].Facts["employees"]; got.Value != "80" || got.Origin != OriginInput {
		t.Errorf("employees = %+v: a lower origin fills it again", got)
	}
}

// key_conflicts counts a source and row id only on its first conflict: an
// alias change re-applies the row without counting it again.
func TestKeyConflictCountedOnce(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "ravi@acme.example", "linkedin", "linkedin.com/in/shared"))
	r := in("a", "email", "priya@acme.example", "linkedin", "linkedin.com/in/shared", "Current tool", "Looker")
	w.apply(r)
	w.aliases = map[string]string{"currenttool": "uses_competitor"}
	w.apply(r)
	if w.person(w.lead("priya@acme.example")).Fields["uses_competitor"].Value != "Looker" {
		t.Fatal("setup: the alias change must re-apply the row")
	}
	if got := w.m.StateValue("key_conflicts"); got != "1" || w.logKinds("key_conflict") != 1 {
		t.Errorf("key_conflicts %q, logged %d; want 1 and 1", got, w.logKinds("key_conflict"))
	}
}

// A slug is decoded segment by segment: an encoded slash or a dot segment is
// never a slug, and case and Unicode form never split one person.
func TestLinkedInSlugDecoding(t *testing.T) {
	for _, v := range []string{
		"https://linkedin.com/in/john%2Fsmith",
		"https://linkedin.com/in/john/../mary",
		"https://linkedin.com/in/./a",
		"https://linkedin.com/in/../a",
		"https://linkedin.com/in/%2E",
		"https://linkedin.com/in/%zz",
	} {
		if got := CanonicalLinkedIn(v); got != "" {
			t.Errorf("CanonicalLinkedIn(%q) = %q, want no key", v, got)
		}
	}
	if CanonicalLinkedIn("https://linkedin.com/in/john%2Fsmith") == CanonicalLinkedIn("https://linkedin.com/in/john") {
		t.Error("an encoded slash joined two slugs")
	}
	const want = "linkedin.com/in/jörg"
	for _, v := range []string{
		"https://linkedin.com/in/J%C3%96RG",
		"https://linkedin.com/in/j%C3%B6rg",
		"https://linkedin.com/in/jörg",  // composed
		"https://linkedin.com/in/jörg", // decomposed
		"https://linkedin.com/in/JO%CC%88RG/",
	} {
		if got := CanonicalLinkedIn(v); got != want {
			t.Errorf("CanonicalLinkedIn(%q) = %q, want %q", v, got, want)
		}
	}
}

// The key conflict an event-created lead counted is not counted again by the
// first receiver row for the same contact id.
func TestEventConflictCountedOnceForTheContact(t *testing.T) {
	w := newWorld(t)
	w.apply(in("a", "email", "bo@acme.example", "linkedin", "linkedin.com/in/bo"))
	ApplyEventPerson(w.m, NormalizeEventKeys(api.Event{Kind: "visit_pricing", Email: "cy@acme.example",
		LinkedInURL: "linkedin.com/in/bo", ReceivedAt: t0, Attrs: map[string]string{"contact_id": "c-9"}}))
	w.apply(in("receiver", "contact_id", "c-9", "email", "cy@acme.example", "linkedin", "linkedin.com/in/bo"))
	if got := w.m.StateValue("key_conflicts"); got != "1" || w.logKinds("key_conflict") != 1 {
		t.Errorf("key_conflicts %q, logged %d; want 1 and 1", got, w.logKinds("key_conflict"))
	}
}
