// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package cli is the leadscore command line. Every command takes
// --config and --rubric, before or after the command name. A command whose slice
// has not landed prints "not built yet" and exits 2.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
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

// commands is the command table, in its design order. A slice that builds a command
// replaces its `slice:` field with `run:` on its own line only; do not reorder
// or reformat the table, so parallel slices touching it merge cleanly.
var commands = []*command{
	{
		path:  []string{"run"},
		flags: map[string]flagKind{"dry-run": boolFlag},
		args:  "[--dry-run]",
		help:  "one full run",
		run:   runRun,
	},
	{
		path:    []string{"explain"},
		args:    "<person>",
		minArgs: 1,
		maxArgs: 1,
		help:    "a lead's verdict and reasons",
		run:     runExplain,
	},
	{
		path: []string{"doctor"},
		help: "one pass/fail line per check",
		run:  runDoctor,
	},
	{
		path:  []string{"ranked"},
		flags: map[string]flagKind{"csv": boolFlag},
		args:  "[--csv]",
		help:  "print every lead's verdict, or write it as CSV",
		run:   runRanked,
	},
	{
		path:  []string{"facts"},
		flags: map[string]flagKind{"csv": boolFlag},
		args:  "[--csv]",
		help:  "print every company's stored facts, or write them as CSV",
		run:   runFacts,
	},
	{
		path:  []string{"serve"},
		flags: map[string]flagKind{"every": optValueFlag},
		args:  "[--every [interval]]",
		help:  "the receiver and /healthz; with --every, also runs the loop",
		run:   runServe,
	},
	{
		path: []string{"status"},
		help: "print the Health table",
		run:  runStatus,
	},
	{
		path:    []string{"set-status"},
		args:    "<person> <status|none|resubscribe>",
		minArgs: 2,
		maxArgs: 2,
		help:    "replace, remove, or undo a manual status in Overrides",
		run:     runSetStatus,
	},
	{
		path:    []string{"merge"},
		args:    "<person> <person>",
		minArgs: 2,
		maxArgs: 2,
		help:    "mark two persons as the same lead in Overrides",
		run:     runMerge,
	},
	{
		path:    []string{"mark-distinct"},
		args:    "<person> <person>",
		minArgs: 2,
		maxArgs: 2,
		help:    "mark two namesakes as different leads in Overrides",
		run:     runMarkDistinct,
	},
	{
		path:    []string{"retry"},
		flags:   map[string]flagKind{"lane": valueFlag},
		args:    "[--lane X] [<person>]",
		maxArgs: 1,
		help:    "write a retry row in Overrides",
		run:     runRetry,
	},
	{
		path: []string{"config", "push"},
		help: "upload leadscore.yml and the rubric as one Secret Manager version",
		run:  runConfigPush,
	},
	{
		path: []string{"setup", "hubspot"},
		help: "create custom properties, resolve pipeline and stage",
		run:  runSetupHubSpot,
	},
	{
		path:    []string{"rules", "check"},
		args:    "<file>",
		minArgs: 1,
		maxArgs: 1,
		help:    "compile a rubric and report errors",
		run:     runRulesCheck,
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
		run:   runSetupSheet,
	},
	{
		path: []string{"healthz"},
		help: "call the local /healthz",
		run:  runHealthz,
	},
	{
		path: []string{"version"},
		help: "print the version",
		run:  runVersion,
	},
}

// groups is how `leadscore help` lists the commands: by what a person is
// doing. Every command is in exactly one group (a test holds this).
var groups = []struct {
	name     string
	commands []string
}{
	{"Set up", []string{"setup sheet", "setup hubspot", "config get", "config set-hosting", "config push", "rules check"}},
	{"Run", []string{"run", "serve"}},
	{"Inspect", []string{"status", "ranked", "explain", "facts", "doctor", "healthz", "version"}},
	{"Fix", []string{"set-status", "merge", "mark-distinct", "retry"}},
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

// fail prints a command's error. Locally the error goes to the operator's own
// terminal unredacted: it names their files and keys, which they need to see
// to fix them. Inside Cloud Run (K_SERVICE or CLOUD_RUN_JOB set) stderr is
// Cloud Logging, so it is redacted like every other log line. Vendor response
// text is reduced with logredact.VendorErrorDetail where it is read.
func (inv *invocation) fail(err error) int {
	msg := err.Error()
	if inCloudRun() {
		msg = logredact.Redact(msg)
	}
	fmt.Fprintf(inv.stderr, "leadscore %s: %s\n", inv.cmd.name(), msg)
	return exitFail
}

func inCloudRun() bool { return os.Getenv("K_SERVICE") != "" || os.Getenv("CLOUD_RUN_JOB") != "" }

var (
	errHelp    = errors.New("help requested")
	errVersion = errors.New("version requested")
)

// usageError is a known command used wrongly: Main prints the message and
// that command's usage, not the whole list.
type usageError struct {
	cmd *command
	msg string
}

func (e *usageError) Error() string { return e.msg }

// Main runs the command line and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	// Before anything can log: mask the exact key values from the environment.
	logredact.MaskEnvSecrets(os.Getenv)
	if cmd, asked, unknown := helpTarget(args); asked {
		switch {
		case cmd != nil:
			commandUsage(stdout, cmd)
		case unknown != "":
			fmt.Fprintf(stderr, "leadscore: unknown command %q\n\n", unknown)
			usage(stderr)
			return exitUsage
		default:
			usage(stdout)
		}
		return exitOK
	}
	inv, err := parse(args)
	if errors.Is(err, errHelp) {
		usage(stdout)
		return exitOK
	}
	if errors.Is(err, errVersion) {
		fmt.Fprintln(stdout, versionLine())
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(stderr, "leadscore %s: %s\n", ue.cmd.name(), ue.msg)
		commandUsage(stderr, ue.cmd)
		return exitUsage
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

// helpTarget reports whether args ask for help (-h, --help, or a first word
// "help"), and for which command: the longest run of the words that names
// one. unknown is set for `help <words>` when the words name no command.
func helpTarget(args []string) (cmd *command, asked bool, unknown string) {
	var words []string
	helpWord := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if name == "h" || name == "help" {
				asked = true
			}
			if kind, ok := globalFlags[name]; ok && kind == valueFlag && !hasValue {
				i++ // its value is not a command word
			}
			continue
		}
		if len(words) == 0 && a == "help" && !helpWord {
			asked, helpWord = true, true
			continue
		}
		words = append(words, a)
	}
	if !asked {
		return nil, false, ""
	}
	for n := len(words); n > 0; n-- {
		if c, _ := match(words[:n]); c != nil {
			return c, true, ""
		}
	}
	if helpWord && len(words) > 0 {
		return nil, true, strings.Join(words, " ")
	}
	return nil, true, ""
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
			if name == "version" && inv.cmd == nil {
				return nil, errVersion
			}
			kind, ok := globalFlags[name]
			if !ok && inv.cmd != nil {
				kind, ok = inv.cmd.flags[name]
			}
			if !ok {
				if inv.cmd == nil {
					return nil, fmt.Errorf("unknown flag %s before the command", a)
				}
				return nil, &usageError{inv.cmd, "unknown flag " + a}
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
		want := "takes no arguments"
		switch {
		case inv.cmd.maxArgs < 0:
			want = fmt.Sprintf("needs at least %d argument(s)", inv.cmd.minArgs)
		case inv.cmd.minArgs == inv.cmd.maxArgs && inv.cmd.minArgs > 0:
			want = fmt.Sprintf("needs %d argument(s)", inv.cmd.minArgs)
		case inv.cmd.maxArgs > 0:
			want = fmt.Sprintf("takes at most %d argument(s)", inv.cmd.maxArgs)
		}
		return nil, &usageError{inv.cmd, fmt.Sprintf("%s, got %d", want, n)}
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

// usageLine is a command's name and arguments.
func (c *command) usageLine() string {
	line := c.name()
	if c.args != "" {
		line += " " + c.args
	}
	return line
}

// commandUsage prints one command's usage line and what it does.
func commandUsage(w io.Writer, c *command) {
	fmt.Fprintf(w, "Usage: leadscore %s [--config <file>] [--rubric <file>]\n", c.usageLine())
	note := c.help
	if c.run == nil {
		note += " (not built yet: " + c.slice + ")"
	}
	fmt.Fprintf(w, "%s.\n", strings.ToUpper(note[:1])+note[1:])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: leadscore [--config <file>] [--rubric <file>] <command> [arguments]")
	byName := map[string]*command{}
	for _, c := range commands {
		byName[c.name()] = c
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s:\n", g.name)
		for _, n := range g.commands {
			c := byName[n]
			note := c.help
			if c.run == nil {
				note += " (not built yet: " + c.slice + ")"
			}
			fmt.Fprintf(w, "  %-47s %s\n", c.usageLine(), note)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Overrides: the table where people correct leads (statuses, merges, retries); runs read it, never write it.")
	fmt.Fprintln(w, "Health: the table where each run records its result and any open problems.")
	fmt.Fprintln(w, "One command's usage: leadscore help <command>, or leadscore <command> --help.")
	fmt.Fprintln(w, "Without --config, leadscore reads /config/bundle.yaml, else /config/leadscore.yml, else ./leadscore.yml.")
}
