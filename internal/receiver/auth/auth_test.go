// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"strings"
	"testing"
)

const secret = "s3cr3t-from-the-env-file"

// TestVerifyRejectsEverythingItShould is the test that matters here. A verifier
// that is wrong in the permissive direction passes every test written against
// valid input, so the cases below are almost all rejections, including the ones
// a hand-rolled copy is most likely to get wrong.
func TestVerifyRejectsEverythingItShould(t *testing.T) {
	body := []byte(`{"event":"email_replied","contact_email":"ada@example.com"}`)
	valid := Sign(secret, body)

	tests := []struct {
		name      string
		mode      Mode
		secret    string
		body      []byte
		presented string
		wantOK    bool
	}{
		{"valid signature", ModeSignature, secret, body, valid, true},
		{"valid shared secret", ModeSharedSecret, secret, body, secret, true},

		// The case the whole signature mode exists for: the secret is right, the
		// body is not the one it was computed over.
		{"tampered body", ModeSignature, secret, []byte(`{"event":"email_replied","contact_email":"someone@example.org"}`), valid, false},
		// A signature from a different secret.
		{"signature under the wrong secret", ModeSignature, secret, body, Sign("not-the-secret", body), false},
		{"wrong shared secret", ModeSharedSecret, secret, body, "not-the-secret", false},
		{"shared secret prefix only", ModeSharedSecret, secret, body, secret[:5], false},
		// A shared secret presented where a signature is required must fail: the
		// weaker proof must not satisfy the stronger mode.
		{"raw secret does not satisfy signature mode", ModeSignature, secret, body, secret, false},
		// Missing or malformed proof.
		{"no proof", ModeSignature, secret, body, "", false},
		{"no proof, shared mode", ModeSharedSecret, secret, body, "", false},
		{"no prefix", ModeSignature, secret, body, strings.TrimPrefix(valid, SignaturePrefix), false},
		{"not hex", ModeSignature, secret, body, SignaturePrefix + "zzzz", false},
		// The configuration failure that would otherwise open the endpoint to
		// everyone: an optional secret left unset.
		{"no secret configured, signature mode", ModeSignature, "", body, valid, false},
		{"no secret configured, shared mode", ModeSharedSecret, "", body, "anything", false},
		{"no secret configured, empty proof", ModeSharedSecret, "", body, "", false},
		// An unrecognised mode must reject rather than fall through to the weaker
		// check.
		{"unknown mode", Mode(99), secret, body, secret, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.mode, tt.secret, tt.body, tt.presented)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("Verify = %v, want it to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Verify passed, want it rejected")
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("error %v does not wrap ErrUnauthorized, so callers cannot classify it", err)
			}
		})
	}
}

// TestModeSignatureIsTheZeroValue pins a deliberate choice: a caller that forgets
// to set the mode must get the strong check. If this ever flips, every sender
// that omits its mode silently drops to a comparable secret.
func TestModeSignatureIsTheZeroValue(t *testing.T) {
	var mode Mode
	if mode != ModeSignature {
		t.Fatalf("zero Mode = %d, want ModeSignature, the safe default", mode)
	}
}

// Rotation: the current and the previous secret both pass while both are set;
// nothing passes when neither is.
func TestVerifyAnyAcceptsEitherSecretDuringARotation(t *testing.T) {
	cases := []struct {
		name      string
		secrets   []string
		presented string
		wantOK    bool
	}{
		{"current", []string{"new-secret", "old-secret"}, "new-secret", true},
		{"previous", []string{"new-secret", "old-secret"}, "old-secret", true},
		{"neither", []string{"new-secret", "old-secret"}, "other-secret", false},
		{"previous removed", []string{"new-secret", ""}, "old-secret", false},
		{"none configured", []string{"", ""}, "", false},
		{"none configured, a proof", []string{"", ""}, "anything", false},
		{"empty proof", []string{"new-secret"}, "", false},
	}
	for _, c := range cases {
		err := VerifyAny(c.secrets, c.presented)
		if (err == nil) != c.wantOK {
			t.Errorf("%s: VerifyAny = %v, want ok %v", c.name, err, c.wantOK)
		}
		if err != nil && !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: error %v does not wrap ErrUnauthorized", c.name, err)
		}
	}
}
