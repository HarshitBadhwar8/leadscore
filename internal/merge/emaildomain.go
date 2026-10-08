// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package merge

import "strings"

// personalProviders are personal mailbox providers: an address at one of them
// names no company. Exact match only; a subdomain such as mail.gmail.com is
// not one of these, and treating it as one would need a suffix rule this list
// does not claim to be.
var personalProviders = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"yahoo.com":      true,
	"yahoo.co.in":    true,
	"icloud.com":     true,
	"me.com":         true,
	"aol.com":        true,
	"protonmail.com": true,
	"proton.me":      true,
	"zoho.com":       true,
	"yandex.com":     true,
	"mail.com":       true,
	"gmx.com":        true,
	"rediffmail.com": true,
	"126.com":        true,
	"163.com":        true,
	"fastmail.com":   true,
	"gmx.net":        true,
	"hey.com":        true,
	"hotmail.co.uk":  true,
	"mac.com":        true,
	"msn.com":        true,
	"pm.me":          true,
	"qq.com":         true,
	"rocketmail.com": true,
	"tutanota.com":   true,
	"yahoo.co.uk":    true,
	"yandex.ru":      true,
	"ymail.com":      true,
}

// IsPersonalProvider reports whether a lowercase domain is a personal mailbox
// provider, so an address there names no company.
func IsPersonalProvider(domain string) bool { return personalProviders[domain] }

// DeriveCompanyDomain takes the domain part of a work email (the
// company domain is derived from a work email when absent). It reports false
// for a personal provider and for anything that is not a single well-formed
// address: a false positive manufactures a junk company from a personal
// address, so the rule errs towards no company.
func DeriveCompanyDomain(email string) (string, bool) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", false
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	// Any whitespace at all, not just a space: a tab or newline smuggled through
	// would otherwise pass as a domain and become a company.
	if domain == "" || strings.ContainsAny(domain, " \t\r\n,;@") || !strings.Contains(domain, ".") {
		return "", false
	}
	if IsPersonalProvider(domain) {
		return "", false
	}
	return domain, true
}
