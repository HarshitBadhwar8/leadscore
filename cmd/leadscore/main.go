// Command leadscore is the release binary: the built-in adapters plus the CLI.
// A custom build copies this file and adds an import for its own adapter package.
package main

import (
	"github.com/HarshitBadhwar8/leadscore"

	// Built-in adapters register here with blank imports as their slices land.
	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
)

func main() { leadscore.Main() }
