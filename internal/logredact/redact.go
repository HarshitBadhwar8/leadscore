// Package logredact masks emails, secrets and home-directory paths in log text,
// and summarises vendor error bodies without echoing the personal data a vendor
// sends back. Every stdout and Cloud Logging line passes through it, so logs
// carry ids, never emails.
//
// Adapted from the logredact module by its authors (Redact and
// VendorErrorDetail).
package logredact

import (
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// redactedPlaceholder is the replacement string written wherever a value is
// fully masked (token regex match, malformed email/path, etc.).
const redactedPlaceholder = "[REDACTED]"

// tokenPatterns match well-known secret formats anywhere inside a string.
// Each match is replaced wholesale with redactedPlaceholder.
var tokenPatterns = []*regexp.Regexp{
	// A PEM private key, through its END line (or to the end of the text when
	// cut off), first so no shorter pattern splits it.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?i)Bearer\s+\S+`),
	// HubSpot private-app token: pat-<region>-<uuid>.
	regexp.MustCompile(`(?i)\bpat-[a-z]{2,4}\d*-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),
	// Google API key.
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`ya29\.\S+`),
	regexp.MustCompile(`gho_\S+`),
	regexp.MustCompile(`ghp_\S+`),
	regexp.MustCompile(`ghs_\S+`),
	regexp.MustCompile(`\bsk-\S+`),
	regexp.MustCompile(`xoxb-\S+`),
	regexp.MustCompile(`xoxp-\S+`),
	regexp.MustCompile(`xoxa-\S+`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), // JWT
}

var emailRegex = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// pathUsernameRegex masks "/Users/<name>/" or "/home/<name>/" → "~/", for any
// single segment after /Users or /home, since the username is not known here.
var pathUsernameRegex = regexp.MustCompile(`/(Users|home)/[^/]+/`)

// Redact returns s with all known token patterns, emails, and home-dir paths
// replaced. Used for free-form fields like log messages and event names.
func Redact(s string) string {
	if vals := secretValues.Load(); vals != nil {
		for _, v := range *vals {
			s = strings.ReplaceAll(s, v, redactedPlaceholder)
		}
	}
	for _, p := range tokenPatterns {
		s = p.ReplaceAllString(s, redactedPlaceholder)
	}
	s = emailRegex.ReplaceAllStringFunc(s, maskEmail)
	s = pathUsernameRegex.ReplaceAllString(s, "~/")
	return s
}

// keyPatterns are the token formats ContainsSecret looks for: Redact's,
// anchored and with a minimum length, so ordinary words ("bearer bond", an id
// like sk-leads) do not read as keys. Redact keeps its broader patterns: in a
// log line, masking too much is safe; refusing a team's file is not.
var keyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	regexp.MustCompile(`(?i)\bpat-[a-z]{2,4}\d*-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`\bya29\.[A-Za-z0-9._-]{20,}`),
	regexp.MustCompile(`\bgh[ops]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bxox[abp]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWT
}

// ContainsSecret reports whether s holds a known key or token format, or a
// value MaskEnvSecrets registered. Unlike comparing Redact(s) with s, it
// ignores emails and home paths, which are personal data but not keys.
func ContainsSecret(s string) bool {
	if vals := secretValues.Load(); vals != nil {
		for _, v := range *vals {
			if strings.Contains(s, v) {
				return true
			}
		}
	}
	for _, p := range keyPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// SecretVariables are the environment variables whose exact values Redact
// masks once MaskEnvSecrets has run (the documented key variables).
var SecretVariables = []string{
	"APOLLO_API_KEY",
	"HUBSPOT_TOKEN",
	"LEADSCORE_RECEIVER_SECRET",
	"LEADSCORE_RECEIVER_SECRET_PREVIOUS",
}

// minSecretLen keeps a tiny or placeholder value from masking ordinary text.
const minSecretLen = 6

var secretValues atomic.Pointer[[]string]

// MaskEnvSecrets makes Redact mask the exact values of SecretVariables, read
// through getenv (os.Getenv at startup). Patterns cannot recognise an Apollo
// key or a receiver secret, so their values are masked literally. Calling it
// again replaces the set.
func MaskEnvSecrets(getenv func(string) string) {
	var vals []string
	for _, name := range SecretVariables {
		if v := strings.TrimSpace(getenv(name)); len(v) >= minSecretLen {
			vals = append(vals, v)
		}
	}
	// Longest first, so a secret that contains another is masked whole.
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	secretValues.Store(&vals)
}

func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return redactedPlaceholder
	}
	local := email[:at]
	domain := email[at+1:]
	dot := strings.IndexByte(domain, '.')
	if dot < 0 {
		return redactedPlaceholder
	}
	domainName := domain[:dot]
	rest := domain[dot:]
	if local == "" || domainName == "" {
		return redactedPlaceholder
	}
	return string(local[0]) + "***@" + string(domainName[0]) + "***" + rest
}

// vendorDiagnosticKeys are response-body fields whose values are machine-issued
// identifiers rather than prose: safe to carry into an error or a log line. The
// list is deliberately short. Anything absent from it — including a vendor's
// human-readable message — is dropped, because a validation message routinely
// quotes the value that failed validation, and that value is our own submitted
// payload.
var vendorDiagnosticKeys = map[string]struct{}{
	"category":       {},
	"code":           {},
	"correlationId":  {},
	"correlation_id": {},
	"errorType":      {},
	"error_code":     {},
	"requestId":      {},
	"status":         {},
}

// maxVendorDiagnosticValue bounds a value admitted as an identifier. A machine id
// is short and has no spaces; anything longer or containing whitespace is prose
// wearing an allowlisted key, and is dropped.
const maxVendorDiagnosticValue = 64

// VendorErrorDetail summarises an external API's error body for an error message
// or a log line, admitting ONLY machine-issued identifiers.
//
// This is an allowlist, not a redaction pass, and that is the point: Redact masks
// what it recognises (emails, tokens, home paths) and cannot mask what it does
// not — a name, a job title, a company, a URL. A vendor rejecting a contact or a
// deal echoes exactly those fields back, so passing its body through Redact
// leaves most of a person's identity intact. Naming what may leave is the only
// version of this that holds when the vendor changes its error shape.
//
// It scans for the allowlisted keys rather than parsing the body. Parsing would
// buy nothing here — nesting and types are irrelevant to a fixed set of scalar
// fields — while a scan cannot fail, so there is no parse outcome to interpret
// and no branch where a malformed body could fall through to its own raw text.
//
// Returns "" when no admitted key is present. The caller should pair it with the
// HTTP status, which is always safe.
func VendorErrorDetail(body []byte) string {
	s := string(body)
	var kept []string
	for _, key := range sortedVendorDiagnosticKeys() {
		m := vendorDiagnosticPattern(key).FindStringSubmatch(s)
		if m == nil {
			continue
		}
		v := m[1]
		// Re-validated after matching, not trusted because the key was allowlisted:
		// a machine id is short and has no whitespace, so prose wearing an
		// allowlisted key is dropped here.
		if v == "" || len(v) > maxVendorDiagnosticValue || strings.ContainsAny(v, " \t\n\r") {
			continue
		}
		kept = append(kept, key+"="+v)
	}
	// Redact over the allowlist's own output: two independent layers, so an
	// email-shaped value that ever reached an allowlisted key is still masked.
	// The allowlist is the control; this is the seatbelt on it.
	return Redact(strings.Join(kept, " "))
}

func sortedVendorDiagnosticKeys() []string {
	keys := make([]string, 0, len(vendorDiagnosticKeys))
	for k := range vendorDiagnosticKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// vendorDiagnosticPattern matches one allowlisted key's string value. Case
// -insensitive on the key because vendors differ (correlationId, correlation_id),
// and the value class is deliberately narrow: no escapes, no quotes, so a value
// containing either simply does not match and is dropped rather than half-read.
func vendorDiagnosticPattern(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)"` + regexp.QuoteMeta(key) + `"\s*:\s*"([^"\\]*)"`)
}
