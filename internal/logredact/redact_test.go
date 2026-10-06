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
		{"multiple patterns", "Bearer xyz and email a@b.co", "[REDACTED] and email a***@b***.co"},
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
