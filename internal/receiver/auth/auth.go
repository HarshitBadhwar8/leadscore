// Package auth verifies that an inbound webhook came from the holder of the
// receiver's secret (contracts section 5.1).
//
// Adapted, with its tests, from a webhook verifier by its authors.
//
// Two modes, and which one a sender gets is a property of the sender rather
// than a preference:
//
//   - Signature. The sender HMACs the body it is about to send, so the secret
//     proves authenticity AND binds it to these bytes. A captured request cannot
//     be replayed with different contents.
//   - Shared secret. The sender presents the secret itself in a header or the
//     body. This proves only that the caller knows the secret: anyone who reads
//     it once (from a log, a screenshot, a vendor's config screen) can forge any
//     body they like, indefinitely, until it is rotated.
//
// The second is strictly weaker and is offered only because some senders cannot
// compute an HMAC. Apollo's workflows cannot, so the receiver uses it, and the
// README warns teams to keep the secret private.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Mode selects how a sender proves itself.
type Mode int

const (
	// ModeSignature requires an HMAC-SHA256 of the body. The zero value on
	// purpose: a caller that forgets to set the mode gets the strong check, not
	// the weak one.
	ModeSignature Mode = iota
	// ModeSharedSecret compares the presented value against the secret. Only for
	// senders that cannot sign.
	ModeSharedSecret
)

// ErrUnauthorized is returned for every rejection. The reason is deliberately not
// distinguished in the error: telling a caller whether the secret was absent, the
// wrong length, or merely wrong hands them a way to probe. Log the detail, return
// this.
var ErrUnauthorized = errors.New("webhook is not authenticated")

// SignaturePrefix is the encoding a signature header carries.
const SignaturePrefix = "sha256="

// Verify checks one delivery. Presented is the value the sender supplied: a
// signature under ModeSignature, the secret itself under ModeSharedSecret.
//
// An empty configured secret always fails. That matters more than it looks: with
// an optional setting, a missing secret would otherwise make the endpoint
// accept everything, which is the failure mode that looks like working software.
func Verify(mode Mode, secret string, body []byte, presented string) error {
	if secret == "" {
		return fmt.Errorf("%w: no secret is configured", ErrUnauthorized)
	}
	if presented == "" {
		return fmt.Errorf("%w: no proof presented", ErrUnauthorized)
	}

	switch mode {
	case ModeSharedSecret:
		// Constant-time even here: a byte-by-byte comparison leaks the secret's
		// prefix through timing, and the secret is the whole of the protection.
		if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
			return fmt.Errorf("%w: shared secret does not match", ErrUnauthorized)
		}
		return nil

	case ModeSignature:
		hexDigest, ok := strings.CutPrefix(presented, SignaturePrefix)
		if !ok {
			return fmt.Errorf("%w: signature is missing the %q prefix", ErrUnauthorized, SignaturePrefix)
		}
		presentedMAC, err := hex.DecodeString(hexDigest)
		if err != nil {
			// Both causes are carried: callers match on ErrUnauthorized, and the
			// hex error names the offending byte. Safe to surface: it describes
			// the presented signature, never the secret.
			return fmt.Errorf("%w: signature is not valid hex: %w", ErrUnauthorized, err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if !hmac.Equal(presentedMAC, mac.Sum(nil)) {
			return fmt.Errorf("%w: signature does not match the body", ErrUnauthorized)
		}
		return nil

	default:
		// An unknown mode is a programming error, and the safe reading of one is
		// to reject rather than to fall through to the weaker check.
		return fmt.Errorf("%w: unknown verification mode %d", ErrUnauthorized, mode)
	}
}

// VerifyAny checks a shared-secret delivery against each configured secret in
// turn (the current one, then the previous one while a rotation is under
// way). Every non-empty secret is compared, whichever matches, so the time
// taken does not tell which one did. Empty secrets are skipped; with none set
// every delivery fails.
func VerifyAny(secrets []string, presented string) error {
	ok := false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if Verify(ModeSharedSecret, s, nil, presented) == nil {
			ok = true
		}
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

// Sign produces the header value a sender would present under ModeSignature.
// Exported for tests.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}
