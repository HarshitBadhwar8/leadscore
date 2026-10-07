package apollo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// visitEventPrefix starts every visit workflow's event literal: one workflow
// per watched site, `website_visited_<name>`, read as event kind
// `visit_<name>` (contracts section 5.1).
const visitEventPrefix = "website_visited_"

// websiteVisit is one body from a visitor-identification workflow.
//
// The trigger fires per identified person, so the contact block is normally
// filled. A body whose contact block is empty still names a company, and that
// becomes a company-only event.
type websiteVisit struct {
	Event string `json:"event"`
	// Domain is the site the trigger watches: the team's own site, not the
	// visitor's employer. Reading it as the employer would attribute every
	// visit to the team itself. The employer is in the account block.
	Domain  string `json:"domain"`
	Contact struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		FirstName   string `json:"first_name"`
		LastName    string `json:"last_name"`
		Title       string `json:"title"`
		LinkedinURL string `json:"linkedin_url"`
		Company     string `json:"company"`
	} `json:"contact"`
	// Account is the visiting company, resolved by the vendor from the
	// visitor's network.
	Account struct {
		Domain     string `json:"domain"`
		WebsiteURL string `json:"website_url"`
		Name       string `json:"name"`
	} `json:"account"`
	VisitedAt string `json:"visited_at"`
}

func parseVisit(body []byte) (websiteVisit, error) {
	var v websiteVisit
	if err := json.Unmarshal(body, &v); err != nil {
		return websiteVisit{}, fmt.Errorf("decoding a visit body: %w", err)
	}
	return v, nil
}

// kind is the event kind: `visit_` plus the workflow's site name, lowercased,
// with any character outside a-z, 0-9 and _ made _. Empty when the event
// literal is not a visit workflow's.
func (v websiteVisit) kind() string {
	ev := strings.ToLower(strings.TrimSpace(v.Event))
	name, ok := strings.CutPrefix(ev, visitEventPrefix)
	if !ok || name == "" {
		return ""
	}
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	return "visit_" + string(b)
}

// employerDomain is the visiting company's domain: the account block's domain,
// else the host of its website URL. Empty rather than a guess; merge then
// derives the domain from a work email.
func (v websiteVisit) employerDomain() string {
	if d := strings.ToLower(strings.TrimSpace(v.Account.Domain)); d != "" {
		return d
	}
	host, err := HostOf(v.Account.WebsiteURL)
	if err != nil {
		return ""
	}
	return host
}

// HostOf reduces a bare host or a full URL to its lowercase host with no
// `www.`. The scheme test is anchored on "://": a bare host with a doubled
// path separator ("acme.io/careers//apply") must not be read as a URL with a
// scheme, or its host comes back empty.
func HostOf(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the account website is not a URL")
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	if host == "" {
		return "", fmt.Errorf("the account website names no host")
	}
	return host, nil
}

func (v websiteVisit) email() string {
	return strings.ToLower(strings.TrimSpace(v.Contact.Email))
}

func (v websiteVisit) fullName() string {
	return strings.TrimSpace(strings.TrimSpace(v.Contact.FirstName) + " " + strings.TrimSpace(v.Contact.LastName))
}

// identifiable reports whether the body names a person: an email or a
// LinkedIn URL. A name and a company are not enough: a namesake false-merge
// would hand one person another's opt-out.
func (v websiteVisit) identifiable() bool {
	return v.email() != "" || strings.TrimSpace(v.Contact.LinkedinURL) != ""
}

// visitedAt is the vendor's visit time, and whether it was usable. With no
// usable time the visit is keyed by a hash of its body (RFC 6.7) and recorded
// at its received time.
func (v websiteVisit) visitedAt() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v.VisitedAt))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// bodyHash is the hex SHA-256 of a stored body: the de-duplication key of a
// visit with no usable visit time, so a redelivered body keys the same.
func bodyHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}
