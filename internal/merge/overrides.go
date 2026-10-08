// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// Overrides actions.
const (
	ActionStatus   = "status"
	ActionSameAs   = "same_as"
	ActionDistinct = "distinct"
	ActionRetry    = "retry"
)

// Resubscribe is the `status` value that undoes a manual `unsubscribed`. It
// is not a status row for the fold's rules 2 and 3.
const Resubscribe = "resubscribe"

// ManualStatuses are the statuses an Overrides row may set: every status
// except new, contacted and deal.
var ManualStatuses = []string{
	"replied_positive", "replied_negative", "replied_neutral", "replied_unlabelled", "unsubscribed", "blocked",
}

func isManualStatus(v string) bool {
	for _, s := range ManualStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// OverrideRow is one Overrides row, read.
type OverrideRow struct {
	Row    int            // 1-based position among the Overrides rows (the header is not counted)
	Raw    model.Override // the row as stored
	Person string         // normalized; "*" means every lead (retry only)
	Action string         // lowercased and trimmed
	Value  string         // lowercased status or lane id; for same_as and distinct, the normalized other person
	Lead   api.LeadID     // the live lead Person names; empty when unknown or "*"
	Other  api.LeadID     // same_as and distinct: the live lead Value names
	// Hash identifies the row for Applied overrides (retry and resubscribe rows
	// apply once): the SHA-256 of its four raw columns.
	Hash string
	// Invalid says why the row has an unknown action or value; empty when valid.
	Invalid string
}

// Matched reports whether every person the row names is a known lead.
func (o OverrideRow) Matched() bool {
	if o.Person == "*" {
		return o.Action == ActionRetry && o.Invalid == ""
	}
	if o.Lead == "" {
		return false
	}
	if o.Action == ActionSameAs || o.Action == ActionDistinct {
		return o.Other != ""
	}
	return true
}

// Overrides is the Overrides tab read against the model.
type Overrides struct {
	Rows []OverrideRow
	// Status is each live lead's one manual status (rule 3), for leads not in
	// Blocked.
	Status map[api.LeadID]string
	// Blocked holds leads with two or more different status rows under any of
	// their identity keys, or with a row of unknown action or value (rule 2),
	// and why. The reason names row numbers, never a person.
	Blocked map[api.LeadID]string
	// Unmatched are rows naming a person not yet known; they wait until the
	// person appears.
	Unmatched []OverrideRow
}

// ParseOverrides reads the Overrides tab: each person normalized like
// Identities and resolved to its live lead (through merged_into, so a row
// under an absorbed lead's email counts for the survivor).
func ParseOverrides(m *model.Model) Overrides {
	out := Overrides{Status: map[api.LeadID]string{}, Blocked: map[api.LeadID]string{}}
	statusRows := map[api.LeadID]map[string][]int{}
	invalid := map[api.LeadID][]int{}
	for i, raw := range m.Overrides {
		o := OverrideRow{
			Row: i + 1, Raw: raw,
			Person: NormalizePerson(raw.Person),
			Action: strings.ToLower(strings.TrimSpace(raw.Action)),
			Value:  strings.TrimSpace(raw.Value),
			Hash:   OverrideHash(raw),
		}
		if o.Person != "*" {
			o.Lead, _ = Resolve(m, o.Person)
		}
		switch o.Action {
		case ActionStatus:
			o.Value = strings.ToLower(o.Value)
			if !isManualStatus(o.Value) && o.Value != Resubscribe {
				o.Invalid = "unknown status value"
			}
		case ActionSameAs, ActionDistinct:
			o.Value = NormalizePerson(o.Value)
			o.Other, _ = Resolve(m, o.Value)
			if o.Value == "" || o.Value == "*" {
				o.Invalid = "the value must name the other person"
			}
		case ActionRetry:
		default:
			o.Invalid = "unknown action"
		}
		if o.Person == "*" && o.Action != ActionRetry {
			o.Invalid = "person * is allowed only on a retry row"
		}
		out.Rows = append(out.Rows, o)
		if !o.Matched() {
			out.Unmatched = append(out.Unmatched, o)
		}
		if o.Lead == "" {
			continue
		}
		switch {
		case o.Invalid != "":
			invalid[o.Lead] = append(invalid[o.Lead], o.Row)
		case o.Action == ActionStatus && o.Value != Resubscribe:
			if statusRows[o.Lead] == nil {
				statusRows[o.Lead] = map[string][]int{}
			}
			statusRows[o.Lead][o.Value] = append(statusRows[o.Lead][o.Value], o.Row)
		}
	}
	for lead, rows := range invalid {
		out.Blocked[lead] = "unknown Overrides value in row " + joinInts(rows)
	}
	for lead, byValue := range statusRows {
		if len(byValue) > 1 {
			var rows []int
			for _, rs := range byValue {
				rows = append(rows, rs...)
			}
			sort.Ints(rows)
			if _, ok := out.Blocked[lead]; !ok {
				out.Blocked[lead] = "conflicting status rows " + joinInts(rows)
			}
			continue
		}
		if _, ok := out.Blocked[lead]; ok {
			continue
		}
		for v := range byValue {
			out.Status[lead] = v
		}
	}
	return out
}

func joinInts(xs []int) string {
	sort.Ints(xs)
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ", ")
}

// OverrideHash is a row's Applied overrides key: the SHA-256 of its four raw
// columns.
func OverrideHash(o model.Override) string {
	h := sha256.Sum256([]byte(strings.Join([]string{o.Person, o.Action, o.Value, o.Note}, "\x1f")))
	return hex.EncodeToString(h[:])
}

// Duplicates returns the leads every lane skips as unresolved duplicates:
// namesakes, two live leads with the same company domain and
// normalized full name not kept apart by an Overrides `distinct` row naming
// that pair (a third namesake stays blocked until paired with each of the
// others), and every lead in a hand-edited merged_into cycle (Cycles).
func Duplicates(m *model.Model) map[api.LeadID]bool {
	distinct := map[[2]api.LeadID]bool{}
	inPair := map[api.LeadID]bool{}
	for _, o := range ParseOverrides(m).Rows {
		if o.Action == ActionDistinct && o.Lead != "" && o.Other != "" {
			distinct[pair(o.Lead, o.Other)] = true
			inPair[o.Lead], inPair[o.Other] = true, true
		}
	}
	groups := map[string][]api.LeadID{}
	for _, p := range m.People {
		if p.MergedInto != "" {
			continue
		}
		d, n := p.Fields[FieldDomain].Value, NormalizeName(p.Fields[FieldFullName].Value)
		if d == "" || n == "" {
			continue
		}
		k := d + "\x00" + n
		groups[k] = append(groups[k], p.LeadID)
	}
	out := Cycles(m)
	for _, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		paired := false
		for _, id := range ids {
			paired = paired || inPair[id]
		}
		if !paired {
			// No distinct row touches the group: every member is blocked, with
			// no need to look at each pair.
			for _, id := range ids {
				out[id] = true
			}
			continue
		}
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				if !distinct[pair(ids[i], ids[j])] {
					out[ids[i]], out[ids[j]] = true, true
				}
			}
		}
	}
	return out
}

func pair(a, b api.LeadID) [2]api.LeadID {
	if b < a {
		a, b = b, a
	}
	return [2]api.LeadID{a, b}
}

// The CLI writers. Each changes only the model's
// Overrides; the caller encodes and commits that table. They write the same
// rows on every store.

// SetStatus writes `set-status <person> <value>`: value is a manual status,
// `none`, or `resubscribe`. It replaces the lead's status rows under every one
// of its identity keys with one row; `none` deletes them; `resubscribe` deletes
// them (a manual `unsubscribed` included) and writes a `resubscribe` row with
// the request time in `note`. An explicit status also deletes a `resubscribe`
// row still waiting for the lead: the newer instruction wins. It returns the
// person the row names.
func SetStatus(m *model.Model, person, value string, now time.Time) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if !isManualStatus(v) && v != "none" && v != Resubscribe {
		return "", fmt.Errorf("%q is not a status Overrides accepts: use one of %s, none or resubscribe",
			value, strings.Join(ManualStatuses, ", "))
	}
	key, lead, err := personKey(m, person)
	if err != nil {
		return "", err
	}
	want := model.Override{Person: key, Action: ActionStatus, Value: v}
	explicit := v != "none" && v != Resubscribe
	for _, o := range ParseOverrides(m).Rows {
		if o.Action != ActionStatus || (o.Value == Resubscribe && !explicit) {
			continue
		}
		if explicit && sameOverride(o.Raw, want) {
			continue // already says this; deleting and re-adding it would rewrite the tab
		}
		if (lead != "" && o.Lead == lead) || (lead == "" && o.Person == key) {
			deleteOverride(m, o.Raw)
		}
	}
	switch v {
	case "none":
	case Resubscribe:
		m.Put(model.TableOverrides, model.Override{Person: key, Action: ActionStatus, Value: Resubscribe, Note: model.FormatTime(now)})
	default:
		for _, o := range m.Overrides {
			if sameOverride(o, want) {
				return key, nil
			}
		}
		m.Put(model.TableOverrides, want)
	}
	return key, nil
}

// AddPair appends a `same_as` or `distinct` row for two persons. Two persons
// that are already one lead are refused, and so is a `same_as` naming a person
// no lead matches: a merge is permanent, so it must name two leads that exist
// now rather than wait for a typo to come true. A `distinct` row may wait.
func AddPair(m *model.Model, action, a, b string) (string, string, error) {
	if action != ActionSameAs && action != ActionDistinct {
		return "", "", fmt.Errorf("action %q is not same_as or distinct", action)
	}
	ka, la, err := personKey(m, a)
	if err != nil {
		return "", "", err
	}
	kb, lb, err := personKey(m, b)
	if err != nil {
		return "", "", err
	}
	if ka == kb || (la != "" && la == lb) {
		return "", "", errors.New("both persons are already the same lead")
	}
	if action == ActionSameAs && (la == "" || lb == "") {
		return "", "", errors.New("merge needs two known leads; a person no lead matches cannot be merged")
	}
	m.Put(model.TableOverrides, model.Override{Person: ka, Action: action, Value: kb})
	return ka, kb, nil
}

// AddRetry appends a `retry` row: for one person, or every lead when person is
// empty; for one lane, or every lane when lane is empty. The request time goes
// in `note`, so each request is a new row that applies once.
func AddRetry(m *model.Model, person, lane string, now time.Time) (string, error) {
	key := "*"
	if strings.TrimSpace(person) != "" {
		k, _, err := personKey(m, person)
		if err != nil {
			return "", err
		}
		key = k
	}
	m.Put(model.TableOverrides, model.Override{Person: key, Action: ActionRetry, Value: strings.TrimSpace(lane), Note: model.FormatTime(now)})
	return key, nil
}

// personKey names a person for a new Overrides row: a known lead by its
// primary email, else LinkedIn URL, else id; an unknown person by its
// normalized email or LinkedIn URL, so the row waits until the person appears.
func personKey(m *model.Model, person string) (key string, lead api.LeadID, err error) {
	if id, ok := Resolve(m, person); ok {
		return NewIndex(m, nil).PersonKey(id), id, nil
	}
	p := NormalizePerson(person)
	switch {
	case p == "" || p == "*":
		return "", "", errors.New("name a person: an email, a LinkedIn URL or a lead id")
	case strings.Contains(p, "@"):
		if err := ValidateEmailShape(p); err != nil {
			return "", "", fmt.Errorf("the person is not a usable email: %v", err)
		}
		return p, "", nil
	case strings.Contains(p, "linkedin."):
		return p, "", nil
	}
	return "", "", errors.New("no lead has that id; name an unknown person by email or LinkedIn URL")
}

func sameOverride(a, b model.Override) bool {
	return a.Person == b.Person && a.Action == b.Action && a.Value == b.Value && a.Note == b.Note
}

func deleteOverride(m *model.Model, o model.Override) {
	m.Delete(model.TableOverrides, []string{o.Person, o.Action, o.Value, o.Note})
}
