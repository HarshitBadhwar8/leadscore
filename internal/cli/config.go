package cli

import (
	"fmt"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// runConfigGet prints one effective configuration value (contracts section 3).
func runConfigGet(inv *invocation) int {
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	v, err := c.Get(inv.args[0])
	if err != nil {
		return inv.fail(err)
	}
	fmt.Fprintln(inv.stdout, v)
	return exitOK
}

// runConfigSetHosting writes the hosting block of leadscore.yml, keeping comments.
func runConfigSetHosting(inv *invocation) int {
	path, err := config.Locate(inv.flags["config"])
	if err != nil {
		return inv.fail(err)
	}
	if err := config.SetHosting(path, inv.args); err != nil {
		return inv.fail(err)
	}
	fmt.Fprintf(inv.stdout, "updated hosting in %s\n", path)
	return exitOK
}
