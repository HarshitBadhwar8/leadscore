// Command leadscore is the release binary: the built-in adapters plus the CLI.
// A custom build copies this file and adds an import for its own adapter package.
package main

import (
	"github.com/HarshitBadhwar8/leadscore"

	// Built-in adapters in public packages (adapters/...) register here with
	// blank imports as their slices land. The built-in stores are internal, so
	// they register through the root package instead.
	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
	_ "github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	_ "github.com/HarshitBadhwar8/leadscore/adapters/sheetsource"
)

func main() { leadscore.Main() }
