// Package cli is the leadscore command line (RFC 6.13). Every command takes
// --config and --rubric, before or after the command name. A command whose slice
// has not landed prints "not built yet" and exits 2.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2 // bad usage, or a command not built yet
)

type flagKind int

const (
	boolFlag     flagKind = iota // --dry-run
	valueFlag                    // --config <path>
	optValueFlag                 // --every [interval]: takes the next word only when it is a duration
)

var globalFlags = map[string]flagKind{"config": valueFlag, "rubric": valueFlag}

type command struct {
	path  []string            // "config", "get"
	args  string              // positional usage, for help
	flags map[string]flagKind // command flags, besides the global ones
	help  string
	// minArgs and maxArgs bound the positional arguments; maxArgs < 0 is unbounded.
	minArgs, maxArgs int
	slice            string // the slice that builds it; empty once built
	run              func(inv *invocation) int
}

func (c *command) name() string { return strings.Join(c.path, " ") }

// commands is RFC 6.13's table, in its order. A slice that builds a command
// replaces its `slice:` field with `run:` on its own line only; do not reorder
// or reformat the table, so parallel slices touching it merge cleanly.
var commands = []*command{
	{
		path:  []string{"run"},
		flags: map[string]flagKind{"dry-run": boolFlag},
		args:  "[--dry-run]",
		help:  "one full run",
		slice: "S10a",
	},
	{
		path:    []string{"explain"},
		args:    "<person>",
		minArgs: 1,
		maxArgs: 1,
		help:    "a lead's verdict and reasons",
		slice:   "S10a",
	},
	{
		path:  []string{"doctor"},
		help:  "one pass/fail line per check",
		slice: "S16",
	},
	{
		path:  []string{"ranked"},
		flags: map[string]flagKind{"csv": boolFlag},
		args:  "[--csv]",
		help:  "print every lead's verdict, or write it as CSV",
		slice: "S10a",
	},
	{
		path:  []string{"serve"},
		flags: map[string]flagKind{"every": optValueFlag},
		args:  "[--every [interval]]",
		help:  "the receiver and /healthz; with --every, also runs the loop",
		slice: "S14a",
	},
	{
		path:  []string{"status"},
		help:  "print the Health table",
		slice: "S10a",
	},
	{
		path:    []string{"set-status"},
		args:    "<person> <status|none|resubscribe>",
		minArgs: 2,
		maxArgs: 2,
		help:    "replace, remove, or undo a manual status in Overrides",
		slice:   "S6",
	},
	{
		path:    []string{"merge"},
		args:    "<person> <person>",
		minArgs: 2,
		maxArgs: 2,
		help:    "mark two persons as the same lead in Overrides",
		slice:   "S6",
	},
	{
		path:    []string{"mark-distinct"},
		args:    "<person> <person>",
		minArgs: 2,
		maxArgs: 2,
		help:    "mark two namesakes as different leads in Overrides",
		slice:   "S6",
	},
	{
		path:    []string{"retry"},
		flags:   map[string]flagKind{"lane": valueFlag},
		args:    "[--lane X] [<person>]",
		maxArgs: 1,
		help:    "write a retry row in Overrides",
		slice:   "S6",
	},
	{
		path:  []string{"config", "push"},
		help:  "upload leadscore.yml and the rubric as one Secret Manager version",
		slice: "S14b",
	},
	{
		path:  []string{"setup", "hubspot"},
		help:  "create custom properties, resolve pipeline and stage",
		slice: "S11",
	},
	{
		path:    []string{"rules", "check"},
		args:    "<file>",
		minArgs: 1,
		maxArgs: 1,
		help:    "compile a rubric and report errors",
		slice:   "S2",
	},
	{
		path:    []string{"config", "get"},
		args:    "<key>",
		minArgs: 1,
		maxArgs: 1,
		help:    "print a configuration value",
		run:     runConfigGet,
	},
	{
		path:    []string{"config", "set-hosting"},
		args:    "<key>=<value>...",
		minArgs: 1,
		maxArgs: -1,
		help:    "write the hosting block, keeping comments",
		run:     runConfigSetHosting,
	},
	{
		path:  []string{"setup", "sheet"},
		flags: map[string]flagKind{"view": boolFlag, "repair": boolFlag},
		args:  "[--view] [--repair]",
		help:  "create the spreadsheet, the SQLite view, or repair its settings",
		slice: "S5",
	},
	{
		path:  []string{"healthz"},
		help:  "call the local /healthz",
		slice: "S14a",
	},
}

// invocation is one parsed command line.
type invocation struct {
	cmd            *command
	args           []string
	flags          map[string]string // name -> value ("" for a set bool flag)
	stdout, stderr io.Writer
}

func (inv *invocation) configOptions() config.Options {
	return config.Options{ConfigPath: inv.flags["config"], RubricPath: inv.flags["rubric"]}
}

// fail prints a local command's error as is. These errors name the operator's
// own files and keys, which they need to see to fix them; vendor response text
// is reduced with logredact.VendorErrorDetail where it is read, before it can
// reach an error. Logs (not this terminal output) go through logredact.Redact.
func (inv *invocation) fail(err error) int {
	fmt.Fprintf(inv.stderr, "leadscore %s: %s\n", inv.cmd.name(), err)
	return exitFail
}

var errHelp = errors.New("help requested")

// Main runs the command line and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	inv, err := parse(args)
	if errors.Is(err, errHelp) {
		usage(stdout)
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "leadscore: %s\n\n", err)
		usage(stderr)
		return exitUsage
	}
	inv.stdout, inv.stderr = stdout, stderr
	if inv.cmd.run == nil {
		fmt.Fprintf(stderr, "leadscore %s: not built yet (slice %s)\n", inv.cmd.name(), inv.cmd.slice)
		return exitUsage
	}
	return inv.cmd.run(inv)
}

// parse reads global flags, the command words, then the command's flags and
// arguments, in any order after the command. "--" ends flag parsing.
func parse(args []string) (*invocation, error) {
	inv := &invocation{flags: map[string]string{}}
	var words []string
	flagsDone := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !flagsDone && a == "--" {
			flagsDone = true
			continue
		}
		if !flagsDone && strings.HasPrefix(a, "-") && a != "-" {
			name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if name == "h" || name == "help" {
				return nil, errHelp
			}
			kind, ok := globalFlags[name]
			if !ok && inv.cmd != nil {
				kind, ok = inv.cmd.flags[name]
			}
			if !ok {
				if inv.cmd == nil {
					return nil, fmt.Errorf("unknown flag %s before the command", a)
				}
				return nil, fmt.Errorf("%s: unknown flag %s", inv.cmd.name(), a)
			}
			switch kind {
			case boolFlag:
				if hasValue {
					return nil, fmt.Errorf("flag --%s takes no value", name)
				}
			case valueFlag:
				if !hasValue {
					// A following "-x" is more likely a forgotten value than a
					// file name; such a value must be given as --flag=-x.
					if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
						return nil, fmt.Errorf("flag --%s needs a value", name)
					}
					i++
					value = args[i]
				}
				if value == "" {
					return nil, fmt.Errorf("flag --%s needs a non-empty value", name)
				}
			case optValueFlag:
				if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					value = args[i]
					hasValue = true
				}
				if hasValue {
					if d, err := config.ParseDuration(value); err != nil || d <= 0 {
						return nil, fmt.Errorf("flag --%s: %q is not a duration longer than zero", name, value)
					}
				}
			}
			inv.flags[name] = value
			continue
		}
		if inv.cmd == nil {
			if len(words) == 0 && a == "help" {
				return nil, errHelp
			}
			words = append(words, a)
			cmd, partial := match(words)
			if cmd != nil {
				inv.cmd = cmd
			} else if !partial {
				return nil, fmt.Errorf("unknown command %q", strings.Join(words, " "))
			}
			continue
		}
		inv.args = append(inv.args, a)
	}
	if inv.cmd == nil {
		if len(words) == 0 {
			return nil, errors.New("no command given")
		}
		return nil, fmt.Errorf("incomplete command %q", strings.Join(words, " "))
	}
	if n := len(inv.args); n < inv.cmd.minArgs || (inv.cmd.maxArgs >= 0 && n > inv.cmd.maxArgs) {
		return nil, fmt.Errorf("usage: leadscore %s %s", inv.cmd.name(), inv.cmd.args)
	}
	return inv, nil
}

// match returns the command named by words, or reports whether words is the
// start of some longer command name ("config" before "get").
func match(words []string) (cmd *command, partial bool) {
	for _, c := range commands {
		if len(c.path) < len(words) {
			continue
		}
		prefix := true
		for i, w := range words {
			if c.path[i] != w {
				prefix = false
				break
			}
		}
		if !prefix {
			continue
		}
		if len(c.path) == len(words) {
			return c, false
		}
		partial = true
	}
	return nil, partial
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: leadscore [--config <file>] [--rubric <file>] <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		line := c.name()
		if c.args != "" {
			line += " " + c.args
		}
		note := c.help
		if c.run == nil {
			note += " (not built yet: " + c.slice + ")"
		}
		fmt.Fprintf(w, "  %-52s %s\n", line, note)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Without --config, leadscore reads /config/bundle.yaml, else /config/leadscore.yml, else ./leadscore.yml.")
}
