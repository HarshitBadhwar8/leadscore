// Package csvsafe makes text safe to write into a CSV a person opens in a
// spreadsheet (contracts section 12.1): a cell a spreadsheet would read as a
// formula is prefixed with a quote, so stored text from a lead sheet can never
// run as one. `ranked --csv` and the export CSVs (S13) use it.
package csvsafe

import (
	"strconv"
	"strings"
)

// Cell returns s, or s with a leading ' when it starts with =, +, -, @, a tab
// or a carriage return. A cell that parses as a number (-5, +1.5) is left
// alone: it cannot be a formula, and the quote would turn it into text.
func Cell(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}

// Row applies Cell to every cell, in place, and returns it.
func Row(cells []string) []string {
	for i, c := range cells {
		cells[i] = Cell(c)
	}
	return cells
}
