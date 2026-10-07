package merge

import (
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func overridesWorld(t *testing.T) *world {
	w := newWorld(t)
	w.apply(
		in("a", "email", "ada@acme.example", "linkedin", "linkedin.com/in/ada"),
		in("a", "email", "bo@acme.example"),
		in("a", "linkedin", "linkedin.com/in/cy"),
	)
	return w
}

func TestParseOverrides(t *testing.T) {
	w := overridesWorld(t)
	ada, bo, cy := w.lead("ada@acme.example"), w.lead("bo@acme.example"), w.lead("linkedin.com/in/cy")
	w.override(" ADA@Acme.example ", "Status", "Unsubscribed")               // 1: a capitalized email matches
	w.override("https://www.linkedin.com/in/ada/", "status", "unsubscribed") // 2: same value under another key
	w.override("bo@acme.example", "status", "replied_positive")              // 3
	w.override("BO@acme.example", "status", "unsubscribed")                  // 4: conflicts with 3
	w.override("linkedin.com/in/cy", "status", "unsubscibed")                // 5: a typo blocks
	w.override("dee@acme.example", "status", "unsubscribed")                 // 6: unknown person waits
	w.override("ada@acme.example", "status", "resubscribe")                  // 7: not a status row
	w.override("*", "retry", "")                                             // 8: every lead
	w.override("*", "status", "unsubscribed")                                // 9: * is only for retry
	w.override("ada@acme.example", "same_as", "eve@acme.example")            // 10: other person unknown

	ov := ParseOverrides(w.m)
	if got := ov.Status[ada]; got != "unsubscribed" {
		t.Errorf("ada's status = %q", got)
	}
	if !strings.Contains(ov.Blocked[bo], "conflicting status rows 3, 4") {
		t.Errorf("bo blocked = %q", ov.Blocked[bo])
	}
	if _, ok := ov.Status[bo]; ok {
		t.Error("a blocked lead has no manual status")
	}
	if !strings.Contains(ov.Blocked[cy], "unknown Overrides value in row 5") {
		t.Errorf("cy blocked = %q: an unknown value must block, so a typo never drops an opt-out", ov.Blocked[cy])
	}
	var unmatched []int
	for _, o := range ov.Unmatched {
		unmatched = append(unmatched, o.Row)
	}
	if joinInts(unmatched) != "6, 9, 10" {
		t.Errorf("unmatched rows = %v, want 6, 9, 10", unmatched)
	}
	if ov.Rows[7].Lead != "" || !ov.Rows[7].Matched() {
		t.Error("a retry row for * matches every lead")
	}
	if ov.Rows[0].Hash == ov.Rows[1].Hash || ov.Rows[0].Hash != OverrideHash(w.m.Overrides[0]) {
		t.Error("each row has its own hash")
	}
}

// A status row under an absorbed lead's email counts for the survivor.
func TestOverridesFollowMerges(t *testing.T) {
	w := overridesWorld(t)
	w.override("bo@acme.example", "same_as", "ada@acme.example")
	w.apply()
	w.override("bo@acme.example", "status", "unsubscribed")
	w.override("ada@acme.example", "status", "replied_negative")
	ov := ParseOverrides(w.m)
	survivor := w.lead("ada@acme.example")
	if !strings.Contains(ov.Blocked[survivor], "conflicting") {
		t.Errorf("survivor blocked = %q: rows under both leads' keys count for it", ov.Blocked[survivor])
	}
}

func TestSetStatus(t *testing.T) {
	w := overridesWorld(t)
	ada := w.lead("ada@acme.example")
	w.override("linkedin.com/in/ada", "status", "replied_neutral")
	w.override("ADA@acme.example", "status", "blocked")
	w.override("ada@acme.example", "status", "resubscribe") // waiting; the newer status wins
	w.override("bo@acme.example", "status", "replied_neutral")

	key, err := SetStatus(w.m, "https://linkedin.com/in/ada", "Unsubscribed", t0)
	if err != nil || key != "ada@acme.example" {
		t.Fatalf("SetStatus = %q, %v; want the primary email", key, err)
	}
	ov := ParseOverrides(w.m)
	if ov.Status[ada] != "unsubscribed" || ov.Blocked[ada] != "" {
		t.Errorf("status %q blocked %q: one row replaces the rows under every key", ov.Status[ada], ov.Blocked[ada])
	}
	if got := countRows(w.m, "status"); got != 2 {
		t.Errorf("status rows = %d, want ada's new row and bo's", got)
	}
	for _, o := range w.m.Overrides {
		if o.Value == "resubscribe" {
			t.Error("an explicit status deletes the resubscribe row still waiting for the lead")
		}
	}

	if _, err := SetStatus(w.m, "ada@acme.example", "resubscribe", t0); err != nil {
		t.Fatal(err)
	}
	ov = ParseOverrides(w.m)
	if _, ok := ov.Status[ada]; ok {
		t.Error("resubscribe deletes the manual unsubscribed")
	}
	found := false
	for _, o := range w.m.Overrides {
		if o.Value == "resubscribe" && o.Note == model.FormatTime(t0) {
			found = true
		}
	}
	if !found {
		t.Error("resubscribe writes a row with the request time in note")
	}

	if _, err := SetStatus(w.m, "bo@acme.example", "none", t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := ParseOverrides(w.m).Status[w.lead("bo@acme.example")]; ok {
		t.Error("none deletes the status rows")
	}

	if _, err := SetStatus(w.m, "ada@acme.example", "deal", t0); err == nil {
		t.Error("deal is not a status Overrides accepts")
	}
	if _, err := SetStatus(w.m, "0190-unknown", "unsubscribed", t0); err == nil {
		t.Error("an unknown lead id is refused")
	}
	key, err = SetStatus(w.m, "Dee@Acme.example", "unsubscribed", t0)
	if err != nil || key != "dee@acme.example" {
		t.Errorf("an unknown person by email waits: %q, %v", key, err)
	}
	if key, _ := SetStatus(w.m, "linkedin.com/in/cy", "blocked", t0); key != "linkedin.com/in/cy" {
		t.Errorf("a lead with no email is named by its LinkedIn URL: %q", key)
	}
}

func countRows(m *model.Model, action string) int {
	n := 0
	for _, o := range m.Overrides {
		if o.Action == action {
			n++
		}
	}
	return n
}

func TestAddPairAndRetry(t *testing.T) {
	w := overridesWorld(t)
	if _, _, err := AddPair(w.m, ActionSameAs, "ada@acme.example", "linkedin.com/in/ada"); err == nil {
		t.Error("two keys of one lead are already the same lead")
	}
	a, b, err := AddPair(w.m, ActionDistinct, "linkedin.com/in/ada", "BO@acme.example")
	if err != nil || a != "ada@acme.example" || b != "bo@acme.example" {
		t.Errorf("AddPair = %q %q %v", a, b, err)
	}
	if k, err := AddRetry(w.m, "", "warm", t0); err != nil || k != "*" {
		t.Errorf("AddRetry every lead = %q %v", k, err)
	}
	if k, err := AddRetry(w.m, "bo@acme.example", "", t0.Add(1)); err != nil || k != "bo@acme.example" {
		t.Errorf("AddRetry one lead = %q %v", k, err)
	}
	last := w.m.Overrides[len(w.m.Overrides)-1]
	if last.Action != "retry" || last.Value != "" || last.Note == "" {
		t.Errorf("retry row = %+v", last)
	}
	ov := ParseOverrides(w.m)
	if len(ov.Unmatched) != 0 || len(ov.Blocked) != 0 {
		t.Errorf("unmatched %v blocked %v", ov.Unmatched, ov.Blocked)
	}
}
