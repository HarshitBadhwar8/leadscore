package cli

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"unicode"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/csvsafe"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// runRun: run [--dry-run]. SIGTERM (or Ctrl-C) closes Stop: the run stops new
// vendor calls and saves within its budget. It exits 1 when the run failed or
// finished unhealthy; a run skipped because another holds the lease is fine.
func runRun(inv *invocation) int {
	_, dry := inv.flags["dry-run"]
	stop := make(chan struct{})
	defer onSignal(func() { close(stop) })()
	res, err := engine.RunTo(context.Background(), api.RunOptions{
		ConfigPath: inv.flags["config"], RubricPath: inv.flags["rubric"], DryRun: dry, Stop: stop,
	}, inv.stdout)
	if err != nil {
		return inv.fail(err)
	}
	if !res.Healthy {
		return exitFail
	}
	return exitOK
}

// openStore loads leadscore.yml and opens its store, for the read-only
// commands. closeStore must be called.
func openStore(inv *invocation) (c *config.Config, b api.Backend, closeStore func(), err error) {
	c, err = config.Load(inv.configOptions())
	if err != nil {
		return nil, nil, nil, err
	}
	open, ok := api.BackendFactory(c.Store.Type)
	if !ok {
		return nil, nil, nil, fmt.Errorf("store type %q is not registered", c.Store.Type)
	}
	b, _, err = open(c.Store.Block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening the store: %w", err)
	}
	closeStore = func() {}
	if cl, ok := b.(io.Closer); ok {
		closeStore = func() { _ = cl.Close() }
	}
	return c, b, closeStore, nil
}

// healthResults is the order `status` prints the result rows in.
var healthResults = []string{"last_result", "last_run_at", "last_success_at", "run_id", "rubric_version", "schedule"}

// runStatus prints the Health table: the last result, then open problems.
func runStatus(inv *invocation) int {
	_, b, closeStore, err := openStore(inv)
	if err != nil {
		return inv.fail(err)
	}
	defer closeStore()
	rows, err := b.ReadTable(context.Background(), model.TableHealth)
	if err != nil {
		return inv.fail(fmt.Errorf("reading Health: %w", err))
	}
	results := map[string]string{}
	type prob struct{ key, value, since string }
	var probs []prob
	for _, r := range rows {
		switch r["kind"] {
		case "result":
			results[r["key"]] = r["value"]
		case "problem":
			probs = append(probs, prob{r["key"], r["value"], r["first_seen_at"]})
		}
	}
	if len(results) == 0 && len(probs) == 0 {
		fmt.Fprintln(inv.stdout, "no run has finished yet")
		return exitOK
	}
	tw := tabwriter.NewWriter(inv.stdout, 0, 2, 2, ' ', 0)
	for _, k := range healthResults {
		if v, ok := results[k]; ok {
			fmt.Fprintf(tw, "%s\t%s\n", k, printable(v))
		}
	}
	_ = tw.Flush()
	sort.Slice(probs, func(i, j int) bool { return probs[i].key < probs[j].key })
	if len(probs) == 0 {
		fmt.Fprintln(inv.stdout, "no open problems")
		return exitOK
	}
	fmt.Fprintf(inv.stdout, "%d open problem(s):\n", len(probs))
	for _, p := range probs {
		fmt.Fprintf(inv.stdout, "  %s (since %s)\n    %s\n", printable(p.key), printable(p.since), printable(p.value))
	}
	return exitOK
}

// rankedColumns returns Ranked's columns in their stored order: the fixed ones,
// with the derived names (the rubric's order when it loads, else sorted) after
// company_domain.
func rankedColumns(c *config.Config, rows []api.Row) []string {
	def, _ := model.Def(model.TableRanked)
	fixed := map[string]bool{}
	for _, col := range def.Columns {
		fixed[col] = true
	}
	var derived []string
	seen := map[string]bool{}
	if text, err := c.Rubric(); err == nil {
		if r, err := rules.Compile(text); err == nil {
			for _, n := range r.DerivedNames() {
				derived = append(derived, n)
				seen[n] = true
			}
		}
	}
	var extra []string
	for _, row := range rows {
		for col := range row {
			if !fixed[col] && !seen[col] {
				seen[col] = true
				extra = append(extra, col)
			}
		}
	}
	sort.Strings(extra)
	derived = append(derived, extra...)
	var out []string
	for _, col := range def.Columns {
		out = append(out, col)
		if col == def.DynamicAfter {
			out = append(out, derived...)
		}
	}
	return out
}

// runRanked prints every lead's verdict from Ranked, highest score first, or
// writes it as CSV with --csv.
func runRanked(inv *invocation) int {
	c, b, closeStore, err := openStore(inv)
	if err != nil {
		return inv.fail(err)
	}
	defer closeStore()
	rows, err := b.ReadTable(context.Background(), model.TableRanked)
	if err != nil {
		return inv.fail(fmt.Errorf("reading Ranked: %w", err))
	}
	score := func(r api.Row) float64 { f, _ := strconv.ParseFloat(r["score"], 64); return f }
	sort.SliceStable(rows, func(i, j int) bool {
		if si, sj := score(rows[i]), score(rows[j]); si != sj {
			return si > sj
		}
		return rows[i]["lead_id"] < rows[j]["lead_id"]
	})
	cols := rankedColumns(c, rows)
	if _, asCSV := inv.flags["csv"]; asCSV {
		if err := writeCSV(inv.stdout, cols, rows); err != nil {
			return inv.fail(err)
		}
		return exitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(inv.stdout, "Ranked is empty: no run has scored a lead yet")
		return exitOK
	}
	// The table leaves out the long reasons column; explain shows them.
	tw := tabwriter.NewWriter(inv.stdout, 0, 2, 2, ' ', 0)
	var shown []string
	for _, col := range cols {
		if col != "reasons" && col != "rubric_version" {
			shown = append(shown, col)
		}
	}
	fmt.Fprintln(tw, strings.Join(shown, "\t"))
	for _, r := range rows {
		vals := make([]string, len(shown))
		for i, col := range shown {
			vals[i] = printable(r[col])
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	_ = tw.Flush()
	fmt.Fprintf(inv.stdout, "%d lead(s); `leadscore explain <person>` shows a lead's reasons\n", len(rows))
	return exitOK
}

// writeCSV writes a header of cols, then each row's cells in that order,
// every cell through csvsafe (`ranked --csv`, `facts --csv`).
func writeCSV(out io.Writer, cols []string, rows []api.Row) error {
	w := csv.NewWriter(out)
	_ = w.Write(cols)
	for _, r := range rows {
		rec := make([]string, len(cols))
		for i, col := range cols {
			rec[i] = r[col]
		}
		_ = w.Write(csvsafe.Row(rec))
	}
	w.Flush()
	return w.Error()
}

// runExplain prints a lead's verdict as the last run stored it in Ranked, in
// rubric.Explain's layout: its row, every derived
// value, the score and its halves, and the reasons, with a note when the
// local rubric has changed since.
func runExplain(inv *invocation) int {
	c, b, closeStore, err := openStore(inv)
	if err != nil {
		return inv.fail(err)
	}
	defer closeStore()
	m, err := codec.Load(context.Background(), b)
	if err != nil {
		return inv.fail(fmt.Errorf("loading the store: %w", err))
	}
	id, ok := merge.Resolve(m, inv.args[0])
	if !ok {
		return inv.fail(errors.New("no lead matches " + inv.args[0]))
	}
	row, ok := m.Ranked[model.Key(id)]
	if !ok {
		return inv.fail(fmt.Errorf("lead %s has no verdict yet; it gets one at the next run", id))
	}
	w := inv.stdout
	fmt.Fprintf(w, "lead %s\n", id)
	for _, kv := range [][2]string{{"email", row.Email}, {"linkedin", row.LinkedInURL}, {"name", row.FullName},
		{"company", row.CompanyDomain}, {"status", row.Status}, {"lane", row.Lane}} {
		if kv[1] != "" {
			fmt.Fprintf(w, "%s: %s\n", kv[0], printable(kv[1]))
		}
	}
	// Derived values in the rubric's order when it compiles, then any others.
	var names []string
	seen := map[string]bool{}
	if text, err := c.Rubric(); err == nil {
		if r, err := rules.Compile(text); err == nil {
			if row.RubricVersion != r.Version() {
				fmt.Fprintf(w, "(scored with rubric %s; the local rubric is %s, so this changes at the next run)\n",
					printable(row.RubricVersion), r.Version())
			}
			for _, n := range r.DerivedNames() {
				if _, ok := row.Derived[n]; ok {
					names, seen[n] = append(names, n), true
				}
			}
		}
	}
	var rest []string
	for n := range row.Derived {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	for _, n := range append(names, rest...) {
		v := row.Derived[n]
		if v == "" {
			v = "no value"
		}
		fmt.Fprintf(w, "%s: %s\n", n, printable(v))
	}
	fmt.Fprintf(w, "score: %s (account %s, contact %s)\n", model.FormatFloat(row.Score),
		model.FormatFloat(row.AccountScore), model.FormatFloat(row.ContactScore))
	if row.Reasons != "" {
		fmt.Fprintln(w, "reasons:")
		for _, reason := range strings.Split(row.Reasons, "; ") {
			fmt.Fprintf(w, "  - %s\n", printable(reason))
		}
	}
	fmt.Fprintf(w, "rubric: %s\n", printable(row.RubricVersion))
	return exitOK
}

// printable replaces control characters in stored text with U+FFFD before it
// reaches a terminal, so a cell cannot move the cursor or recolour the screen.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\uFFFD'
		}
		return r
	}, s)
}

// onSignal calls f once on the first SIGTERM or Ctrl-C. A second one quits at
// once: Go's default handling returns once the first is taken. The returned
// function stops listening.
func onSignal(f func()) (release func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			signal.Stop(sig)
			f()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}
