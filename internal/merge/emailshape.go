package merge

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// maxEmailLen is RFC 5321's ceiling for a whole address: a 64-character local
// part, the @, and a 255-character domain.
const maxEmailLen = 320

// templateMarkers are the delimiters of the templating languages sending tools
// use. An address carrying one was never rendered: the workflow emitted the
// variable's source text where the person's address should have been.
// Checked before the parse so the error names the real fault.
var templateMarkers = []string{"{{", "}}", "${", "%%", "[[", "]]"}

// ValidateEmailShape rejects a string that cannot be a person's address. An
// address that cannot be pushed and cannot be dropped becomes a permanent
// failure on every run, so refusing it at the door is cheaper than admitting it.
//
// Shape only: whether the address accepts mail is not knowable here.
//
// No message quotes the value. These errors are logged on the rejection path,
// and a rejected address is still someone's address.
func ValidateEmailShape(email string) error {
	if email == "" {
		return errors.New("email is empty")
	}
	if len(email) > maxEmailLen {
		return fmt.Errorf("email is %d characters; the maximum is %d", len(email), maxEmailLen)
	}
	for _, marker := range templateMarkers {
		if strings.Contains(email, marker) {
			return fmt.Errorf("email carries the template marker %q: the sending workflow emitted the variable instead of its value", marker)
		}
	}
	// Round-tripped against the input so a display-name form ("Ada <a@b.example>")
	// is refused rather than stored whole: the same person arriving both ways
	// would otherwise become two leads.
	parsed, err := mail.ParseAddress(email)
	if err != nil {
		// Inspected for its class, never wrapped: the standard library's error
		// quotes its input, which on "ada@acme.example, evil@corp.example" would put a
		// second person's address in the log.
		if strings.Contains(err.Error(), "expected single address") {
			return errors.New("email carries more than one address")
		}
		return errors.New("email is not an address")
	}
	if parsed.Address != email {
		return errors.New("email is not a bare address")
	}
	// ParseAddress accepts a dotless domain ("ada@localhost"), which is valid
	// on a local network and never a prospect.
	at := strings.LastIndex(email, "@")
	if !strings.Contains(email[at+1:], ".") {
		return errors.New("email has no domain suffix")
	}
	return nil
}
