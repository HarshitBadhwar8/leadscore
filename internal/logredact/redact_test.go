package logredact

import "testing"

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"bearer token", "auth: Bearer abc.def", "auth: [REDACTED]"},
		{"github personal", "token=ghp_xyz123abc", "token=[REDACTED]"},
		{"github oauth", "gho_xxx", "[REDACTED]"},
		{"github server", "ghs_yyy", "[REDACTED]"},
		{"google access", "ya29.token-here", "[REDACTED]"},
		{"openai", "sk-secret-thing", "[REDACTED]"},
		{"slack bot", "xoxb-12345", "[REDACTED]"},
		{"slack user", "xoxp-67890", "[REDACTED]"},
		{"slack admin", "xoxa-abcdef", "[REDACTED]"},
		{"jwt", "eyJabc.eyJdef.signature123", "[REDACTED]"},
		{"email", "user@example.com", "u***@e***.com"},
		{"path with username", "/Users/alice/Documents/file.txt", "~/Documents/file.txt"},
		{"linux home", "/home/bob/code/main.go", "~/code/main.go"},
		{"multiple patterns", "Bearer xyz and email a@b.example", "[REDACTED] and email a***@b***.example"},
		{"hubspot private app", "token pat-na1-1a2b3c4d-1234-5678-9abc-def012345678 used", "token [REDACTED] used"},
		{"hubspot eu region", "pat-eu1-1A2B3C4D-1234-5678-9ABC-DEF012345678", "[REDACTED]"},
		{"google api key", "key=AIzaSyA1234567890abcdefghijklmnopqrstuv", "key=[REDACTED]"},
		{"pem private key", "cfg -----BEGIN PRIVATE KEY-----\nMIIEv\nabc\n-----END PRIVATE KEY----- tail", "cfg [REDACTED] tail"},
		{"pem rsa key", "-----BEGIN RSA PRIVATE KEY-----\nxyz\n-----END RSA PRIVATE KEY-----", "[REDACTED]"},
		{"pem cut off", "dump -----BEGIN EC PRIVATE KEY-----\nabc", "dump [REDACTED]"},
		{"sk inside a word is not a key", "ask-me task-list", "ask-me task-list"},
		{"sk after a separator", "key=sk-abc", "key=[REDACTED]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaskEnvSecrets(t *testing.T) {
	t.Cleanup(func() { MaskEnvSecrets(func(string) string { return "" }) })
	env := map[string]string{
		"APOLLO_API_KEY":                     "apolloKEY123456",
		"HUBSPOT_TOKEN":                      "  hubtoken-7890  ",
		"LEADSCORE_RECEIVER_SECRET":          "recv-secret-current",
		"LEADSCORE_RECEIVER_SECRET_PREVIOUS": "recv-secret",
		"UNRELATED":                          "not-a-secret-value",
	}
	MaskEnvSecrets(func(k string) string { return env[k] })
	tests := []struct{ in, want string }{
		{"apollo said 401 for key apolloKEY123456", "apollo said 401 for key [REDACTED]"},
		{"hub=hubtoken-7890;", "hub=[REDACTED];"},
		// The current secret contains the previous one; it must be masked whole.
		{"x recv-secret-current y", "x [REDACTED] y"},
		{"old recv-secret", "old [REDACTED]"},
		{"not-a-secret-value", "not-a-secret-value"},
	}
	for _, tt := range tests {
		if got := Redact(tt.in); got != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// A tiny value would mask ordinary words, so it is ignored.
	MaskEnvSecrets(func(k string) string {
		if k == "APOLLO_API_KEY" {
			return "a"
		}
		return ""
	})
	if got := Redact("a cat"); got != "a cat" {
		t.Errorf("a one-character secret masked text: %q", got)
	}
}

func TestContainsSecret(t *testing.T) {
	t.Cleanup(func() { MaskEnvSecrets(func(string) string { return "" }) })
	MaskEnvSecrets(func(k string) string {
		if k == "APOLLO_API_KEY" {
			return "apolloKEY123456"
		}
		return ""
	})
	for s, want := range map[string]bool{
		"key: apolloKEY123456":                                true,
		"token: pat-na1-12345678-1234-1234-1234-123456789012": true,
		"Authorization: Bearer abcdefghij.klmnopqrstuvwxyz":   true,
		"Authorization: Bearer abc.def":                       false, // too short to be a token
		"owner: ana@acme.example":                             false, // personal data, not a key
		"path: /Users/ana/leads.csv":                          false,
		"pipeline: Sales":                                     false,
	} {
		if got := ContainsSecret(s); got != want {
			t.Errorf("ContainsSecret(%q) = %v, want %v", s, got, want)
		}
	}
}
