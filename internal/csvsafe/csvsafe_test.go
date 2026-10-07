package csvsafe

import "testing"

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"Ana":                            "Ana",
		`=HYPERLINK("http://x","click")`: `'=HYPERLINK("http://x","click")`,
		"+cmd":                           "'+cmd",
		"-2+3":                           "'-2+3",
		"@SUM(A1)":                       "'@SUM(A1)",
		"\tx":                            "'\tx",
		"\rx":                            "'\rx",
		"-5":                             "-5",
		"+1.5":                           "+1.5",
		"a=b":                            "a=b",
	} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}
