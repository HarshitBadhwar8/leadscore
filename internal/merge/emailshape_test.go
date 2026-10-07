package merge

import (
	"strings"
	"testing"
)

func TestValidateEmailShape(t *testing.T) {
	tests := []struct {
		name    string
		email   string
		wantErr bool
	}{
		// A sending workflow emitted its own variable: the case the guard exists
		// for, and it carries no @ at all.
		{name: "unrendered handlebars placeholder", email: "{{contact.email}}", wantErr: true},
		{name: "unrendered shell-style placeholder", email: "${lead.email}", wantErr: true},
		{name: "unrendered bracket placeholder", email: "[[email]]", wantErr: true},
		{name: "placeholder spliced into a real domain", email: "{{first}}@acme.example", wantErr: true},

		{name: "no at sign", email: "ada.acme.example", wantErr: true},
		{name: "two at signs", email: "ada@@acme.example", wantErr: true},
		{name: "empty", email: "", wantErr: true},
		{name: "whitespace inside", email: "ada lovelace@acme.example", wantErr: true},
		// Stored whole, this would become a second lead for a person we already have.
		{name: "display-name form", email: "Ada Lovelace <ada@acme.example>", wantErr: true},
		{name: "dotless domain", email: "ada@localhost", wantErr: true},
		{name: "over the length ceiling", email: strings.Repeat("a", 320) + "@acme.example", wantErr: true},

		// Addresses a real prospect can have: rejecting any would drop leads silently.
		{name: "plain", email: "ada@acme.example"},
		{name: "plus addressing", email: "ada+gtm@acme.example"},
		{name: "subdomain", email: "ada@mail.eng.acme.example"},
		{name: "dots and hyphens in the local part", email: "ada.b-lovelace@acme.example"},
		{name: "long tld", email: "ada@acme.technology"},
		{name: "digits", email: "ada2@acme4.example"},
		{name: "apostrophe in the local part", email: "o'hara@acme.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEmailShape(tt.email)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateEmailShape(%q) error = %v, wantErr %v", tt.email, err, tt.wantErr)
			}
		})
	}
}

// The errors travel into a log line on the rejection path, and a rejected
// address is still someone's address.
func TestValidateEmailShapeNeverEchoesTheAddress(t *testing.T) {
	for _, email := range []string{
		"ada@localhost",
		"Ada Lovelace <ada@acme.example>",
		"ada lovelace@acme.example",
		"ada.acme.example",
		strings.Repeat("a", 320) + "@acme.example",
		// The standard library answers a trailing address by quoting it, so
		// wrapping its error would put a second person's address in the log.
		"ada@acme.example, evil@corp.example",
	} {
		err := ValidateEmailShape(email)
		if err == nil {
			t.Fatalf("ValidateEmailShape(%q) accepted an address this test assumes it rejects", email)
		}
		for _, fragment := range strings.FieldsFunc(email, func(r rune) bool {
			return r == '@' || r == ' ' || r == ',' || r == '<' || r == '>'
		}) {
			if len(fragment) >= 4 && strings.Contains(err.Error(), fragment) {
				t.Errorf("error for %q quotes %q from the input: %v", email, fragment, err)
			}
		}
	}
}

// DeriveCompanyDomain decides whether an email-only person gets a company at
// all. A false positive manufactures a junk company from a personal address; a
// false negative leaves a real prospect outside every company rule.
func TestDeriveCompanyDomain(t *testing.T) {
	tests := []struct {
		name, email, want string
		ok                bool
	}{
		{name: "work address", email: "ada@acme.example", want: "acme.example", ok: true},
		{name: "subdomain is kept", email: "ada@eng.acme.example", want: "eng.acme.example", ok: true},
		{name: "uppercase is normalised", email: "Ada@ACME.example", want: "acme.example", ok: true},
		{name: "sub-addressed local part is irrelevant", email: "ada+gtm@acme.example", want: "acme.example", ok: true},
		{name: "free provider rejected", email: "ada@gmail.com"},
		{name: "free provider, uppercase", email: "ada@GMAIL.COM"},
		{name: "another free provider", email: "ada@proton.me"},
		{name: "broader provider: qq.com", email: "ada@qq.com"},
		{name: "broader provider: ymail.com", email: "ada@ymail.com"},
		{name: "broader provider: hey.com", email: "ada@hey.com"},
		{name: "broader provider: yahoo.co.uk", email: "ada@yahoo.co.uk"},
		// Exact match, not a suffix rule: pinned so the behaviour is a decision.
		{name: "free-provider subdomain is not matched", email: "ada@mail.gmail.com", want: "mail.gmail.com", ok: true},
		{name: "no at sign", email: "ada.acme.example"},
		{name: "trailing at", email: "ada@"},
		{name: "leading at", email: "@acme.example"},
		{name: "empty", email: ""},
		{name: "no dot in domain", email: "ada@localhost"},
		{name: "embedded space", email: "ada@acme io.com"},
		{name: "embedded tab", email: "ada@acme\tio.com"},
		{name: "embedded newline", email: "ada@acme\nio.com"},
		{name: "two at signs takes the last", email: "a@b@acme.example", want: "acme.example", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DeriveCompanyDomain(tt.email)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("DeriveCompanyDomain(%q) = %q, %v; want %q, %v", tt.email, got, ok, tt.want, tt.ok)
			}
		})
	}
}
