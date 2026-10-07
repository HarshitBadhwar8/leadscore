package engine

import (
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// maxErrText bounds vendor text the run stores (a ledger row's last_error, a
// Log line, a Health problem): a vendor can answer with a whole page.
const maxErrText = 500

// errText is an error's text redacted (no emails or secrets), then cut to
// maxErrText.
func errText(err error) string { return clip(logredact.Redact(err.Error())) }

// clip cuts text to maxErrText bytes on a character boundary, marking the cut.
func clip(s string) string {
	if len(s) <= maxErrText {
		return s
	}
	cut := maxErrText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " [cut]"
}
