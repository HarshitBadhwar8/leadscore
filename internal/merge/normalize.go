// Package merge turns input rows from many sources into one person per human
// (RFC 6.5, contracts section 12.5). It owns header aliasing, the per-row id
// and hash, the identity rules (which rows become one lead), the event-key
// normalizer, `same_as` merges, and reading the Overrides tab.
//
// A wrong merge transfers one person's opt-out to a stranger, so every rule
// here errs towards keeping two people apart: a duplicate is recoverable (a
// person resolves it in Overrides), a false merge is not.
package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// ReceiverSource is the source id (and channel) of rows the receiver derives
// from Apollo webhooks, and of leads created for an event's person.
const ReceiverSource = "receiver"

// Built-in field names merge reads.
const (
	FieldEmail     = "email"
	FieldLinkedIn  = "linkedin_url"
	FieldFullName  = "full_name"
	FieldContactID = "contact_id"
	FieldDomain    = model.CompanyDomainField // "company.domain"
)

// Normalized is one input row with its headers resolved to field names.
type Normalized struct {
	SourceID string
	// RowID is the row's identity within its source (RFC 6.5): the lowercased
	// email, else the canonical LinkedIn URL, else the company domain and the
	// normalized full name. For source `receiver` it is the contact id first.
	// A rejected row with no key at all gets "row:" plus its hash.
	RowID string
	// RowHash changes when any raw cell or the alias table changes, so an
	// edited row, or a rubric alias change, re-applies the row.
	RowHash string
	// Fields holds every non-empty column by resolved name, trimmed. The email
	// is lowercased, the LinkedIn URL canonical, and the company domain a bare
	// lowercase host.
	Fields map[string]string
	// Reject is the reason the row cannot be applied, or empty. It never
	// quotes a cell value, so it can be logged. A rejected row is still passed
	// to Apply, which records it once in Applied rows.
	Reject string
}

// Normalize resolves a row's headers with the built-in alias table plus the
// rubric's aliases (the rubric wins on a clash; the first header in
// row.Headers order owns a field), computes the row id and hash, and checks the
// email's shape and that the row has a key. It never fails: a rejected row
// comes back with Reject set, and goes to Apply like any other row, so no
// caller can drop a reject before it is recorded.
func Normalize(row api.InputRow, aliases map[string]string) Normalized {
	table := aliasTable(aliases)
	n := Normalized{SourceID: row.SourceID, Fields: map[string]string{}, RowHash: rowHash(row, table)}
	owned := map[string]bool{}
	for _, h := range headersOf(row) {
		name := resolveHeader(table, h)
		if name == "" || owned[name] {
			continue
		}
		owned[name] = true
		v := strings.TrimSpace(row.Columns[h])
		switch name {
		case FieldEmail:
			v = strings.ToLower(v)
		case FieldLinkedIn:
			v = CanonicalLinkedIn(v)
		case FieldDomain:
			v = NormalizeDomain(v)
		}
		if v != "" {
			n.Fields[name] = v
		}
	}

	email, li := n.Fields[FieldEmail], n.Fields[FieldLinkedIn]
	domain, name := n.Fields[FieldDomain], NormalizeName(n.Fields[FieldFullName])
	switch {
	case row.SourceID == ReceiverSource && n.Fields[FieldContactID] != "":
		n.RowID = n.Fields[FieldContactID]
	case email != "":
		n.RowID = email
	case li != "":
		n.RowID = li
	case domain != "" && name != "":
		n.RowID = domain + "|" + name
	}

	switch {
	case email != "":
		if err := ValidateEmailShape(email); err != nil {
			n.Reject = err.Error()
		}
	case li == "" && (domain == "" || name == ""):
		n.Reject = "no email, LinkedIn URL, or company domain and full name"
	}
	if n.RowID == "" {
		n.RowID = "row:" + n.RowHash[:16]
	}
	return n
}

// RowGroup is every row of one source that shares a row id (a CSV with the
// same email on two lines, say). A group applies as one unit: its rows in
// order, under one hash, so a repeated row id settles instead of re-applying
// every run.
type RowGroup struct {
	SourceID, RowID string
	Hash            string // the row's hash for a one-row group; else the SHA-256 of the sorted row hashes
	Rows            []Normalized
}

// Group splits rows into row groups, in order of each group's first row. The
// run's chunking must never split a group across chunks.
func Group(rows []Normalized) []RowGroup {
	var out []RowGroup
	at := map[model.Key]int{}
	for _, r := range rows {
		k := model.K(r.SourceID, r.RowID)
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, RowGroup{SourceID: r.SourceID, RowID: r.RowID})
		}
		out[i].Rows = append(out[i].Rows, r)
	}
	for i := range out {
		g := &out[i]
		if len(g.Rows) == 1 {
			g.Hash = g.Rows[0].RowHash
			continue
		}
		hs := make([]string, len(g.Rows))
		for j, r := range g.Rows {
			hs[j] = r.RowHash
		}
		sort.Strings(hs)
		sum := sha256.Sum256([]byte(strings.Join(hs, "\x00")))
		g.Hash = hex.EncodeToString(sum[:])
	}
	return out
}

// Pending returns the groups not yet applied at their current hash: the
// run's backlog.
func Pending(m *model.Model, rows []Normalized) []RowGroup {
	var out []RowGroup
	for _, g := range Group(rows) {
		if ar, ok := m.AppliedRows[model.K(g.SourceID, g.RowID)]; !ok || ar.RowHash != g.Hash {
			out = append(out, g)
		}
	}
	return out
}

// headersOf returns the row's headers in order, followed by any column the
// headers do not name (sorted), so no cell is silently skipped.
func headersOf(row api.InputRow) []string {
	out := append([]string(nil), row.Headers...)
	seen := map[string]bool{}
	for _, h := range row.Headers {
		seen[h] = true
	}
	var extra []string
	for h := range row.Columns {
		if !seen[h] {
			extra = append(extra, h)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// aliasTable is the built-in table with the rubric's aliases laid over it.
func aliasTable(rubric map[string]string) map[string]string {
	t := api.BuiltinAliases()
	for k, v := range rubric {
		t[k] = v
	}
	return t
}

// resolveHeader names the field a header carries: its alias, else its squashed
// form. A header that squashes to nothing names no field.
func resolveHeader(table map[string]string, h string) string {
	sq := api.SquashHeader(h)
	if sq == "" {
		return ""
	}
	if f, ok := table[sq]; ok {
		return f
	}
	return sq
}

// rowHash is the SHA-256 over the row's sorted raw header=value pairs, plus a
// hash of the alias table, hex encoded.
func rowHash(row api.InputRow, table map[string]string) string {
	pairs := make([]string, 0, len(row.Columns))
	for h, v := range row.Columns {
		pairs = append(pairs, h+"="+v)
	}
	sort.Strings(pairs)
	keys := make([]string, 0, len(table))
	for k, v := range table {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	at := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	h := sha256.New()
	for _, p := range pairs {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	h.Write(at[:])
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalLinkedIn reduces a LinkedIn profile URL to the one form
// identities compare, so the same person entered many ways is one key:
// `linkedin.com/in/<slug>` (or `linkedin.com/pub/<name>/<a>/<b>/<c>`),
// lowercased. Any *.linkedin.com host counts (in., uk., m., www.), with or
// without a scheme; the query, fragment, empty segments, and anything after
// the slug (a locale such as /en) are dropped. Anything else, including a
// placeholder such as "N/A" or "-", is not a LinkedIn key and returns "".
func CanonicalLinkedIn(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
		return ""
	}
	var segs []string
	for _, p := range strings.Split(u.Path, "/") {
		if p = strings.TrimSpace(p); p != "" {
			segs = append(segs, p)
		}
	}
	switch {
	case len(segs) >= 2 && segs[0] == "in":
		return "linkedin.com/in/" + segs[1]
	case len(segs) >= 2 && segs[0] == "pub":
		if len(segs) > 5 {
			segs = segs[:5] // pub, name and the three id parts; a locale may follow
		}
		return "linkedin.com/" + strings.Join(segs, "/")
	}
	return ""
}

// NormalizeDomain reduces a domain or URL to its lowercase host: no scheme,
// user, `www.`, port, path, query or trailing dot.
func NormalizeDomain(raw string) string {
	d := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.LastIndex(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	if i := strings.LastIndex(d, ":"); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	return strings.TrimPrefix(d, "www.")
}

// NormalizeEmail is an email as identities compare it: trimmed and lowercased.
func NormalizeEmail(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// NormalizeName is a full name as namesake matching compares it: Unicode NFC,
// lowercased, trimmed, with runs of spaces collapsed.
func NormalizeName(raw string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFC.String(raw))), " ")
}

// NormalizeEventKeys is the one normalizer for event keys (contracts 12.5):
// sources pass keys trimmed only. The run's intake applies it once to every
// event before keying, resolving persons, or writing windows and companies,
// so nothing downstream reads a raw key.
func NormalizeEventKeys(e api.Event) api.Event {
	e.Email = NormalizeEmail(e.Email)
	e.LinkedInURL = CanonicalLinkedIn(e.LinkedInURL)
	e.Domain = NormalizeDomain(e.Domain)
	return e
}

// NormalizePerson normalizes an Overrides `person` (or a CLI argument) the way
// Identities are keyed: an email lowercased, a LinkedIn URL canonical, anything
// else (a lead id, or `*`) trimmed.
func NormalizePerson(raw string) string {
	s := strings.TrimSpace(raw)
	switch {
	case strings.Contains(s, "@"):
		return NormalizeEmail(s)
	case strings.Contains(strings.ToLower(s), "linkedin."):
		return CanonicalLinkedIn(s) // "" when it is not a profile URL: it names nobody
	}
	return s
}
