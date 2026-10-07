package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/csvsafe"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// runFacts prints the Company facts table, one row per domain sorted by
// domain, or writes it as CSV with --csv: the table's own columns (section 4)
// with their stored text, so the CSV matches a `SELECT *` of the SQLite table
// or a download of the Sheets tab. Like doctor, it never writes the store and
// never takes the lease: SQLite is opened read-only and a missing file is not
// created.
func runFacts(inv *invocation) int {
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	addTestClients(c)
	open, ok := api.BackendFactory(c.Store.Type)
	if !ok {
		return inv.fail(fmt.Errorf("store type %q is not registered", c.Store.Type))
	}
	if c.Store.Type == "sqlite" && !fileExists(c.Store.Path) {
		return inv.fail(fmt.Errorf("there is no SQLite file at %s yet: no run has saved", c.Store.Path))
	}
	b, _, err := openReadOnly(c, open)
	if err != nil {
		return inv.fail(fmt.Errorf("opening the store: %w", err))
	}
	if cl, ok := b.(io.Closer); ok {
		defer cl.Close()
	}
	rows, err := b.ReadTable(context.Background(), model.TableCompanyFacts)
	if err != nil {
		return inv.fail(fmt.Errorf("reading Company facts: %w", err))
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i]["domain"] < rows[j]["domain"] })

	if _, asCSV := inv.flags["csv"]; asCSV {
		cols := factsColumns(rows)
		w := csv.NewWriter(inv.stdout)
		w.Write(cols)
		for _, r := range rows {
			rec := make([]string, len(cols))
			for i, col := range cols {
				rec[i] = r[col]
			}
			w.Write(csvsafe.Row(rec))
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return inv.fail(err)
		}
		return exitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(inv.stdout, "Company facts is empty: no run has stored a company yet")
		return exitOK
	}
	tw := tabwriter.NewWriter(inv.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "domain\tenriched_at\tnot_found_at\tenrich_failed_at\tfacts")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", printable(r["domain"]), printable(r["enriched_at"]),
			printable(r["not_found_at"]), printable(r["enrich_failed_at"]), printable(factsSummary(r)))
	}
	tw.Flush()
	fmt.Fprintf(inv.stdout, "%d domain(s); `leadscore facts --csv` writes every column\n", len(rows))
	return exitOK
}

// factsColumns is Company facts' section 4 columns, then any other column a
// newer version kept, sorted.
func factsColumns(rows []api.Row) []string {
	def, _ := model.Def(model.TableCompanyFacts)
	cols := append([]string{}, def.Columns...)
	known := map[string]bool{}
	for _, col := range cols {
		known[col] = true
	}
	var extra []string
	for _, r := range rows {
		for col := range r {
			if !known[col] {
				known[col] = true
				extra = append(extra, col)
			}
		}
	}
	sort.Strings(extra)
	return append(cols, extra...)
}

type shownFact struct {
	Value  string `json:"value"`
	Origin string `json:"origin"`
}

// factsSummary is a row's facts on one line, by name: value (origin), with
// the value a change replaced, if any. A facts cell that is not section 4's
// JSON is shown as stored.
func factsSummary(r api.Row) string {
	var facts, prev map[string]shownFact
	if s := strings.TrimSpace(r["facts"]); s != "" {
		if err := json.Unmarshal([]byte(s), &facts); err != nil {
			return "facts as stored: " + s
		}
	}
	if s := strings.TrimSpace(r["previous"]); s != "" {
		json.Unmarshal([]byte(s), &prev) // a bad previous only loses the "was" notes
	}
	// A fact only in previous was removed (its Companies cell emptied).
	var names []string
	for n := range facts {
		names = append(names, n)
	}
	for n := range prev {
		if _, ok := facts[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		part := n + "=none ("
		if f, ok := facts[n]; ok {
			part = fmt.Sprintf("%s=%s (%s; ", n, f.Value, f.Origin)
		}
		if p, ok := prev[n]; ok {
			part += fmt.Sprintf("was %s from %s; ", p.Value, p.Origin)
		}
		parts = append(parts, strings.TrimSuffix(part, "; ")+")")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
