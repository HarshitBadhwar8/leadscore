package check

import (
	"context"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func init() { Register(rubric{}) }

// rubric fails when the rubric does not compile, or reads a field that is not
// built in, declared, or a loaded column. Every run also applies the field
// part before scoring.
type rubric struct{}

func (rubric) Name() string { return "rubric" }
func (rubric) InRun() bool  { return true }

func (rubric) Run(_ context.Context, env Env) []Problem {
	if env.Config == nil {
		return nil
	}
	where := env.Config.RubricPath
	if env.Config.Bundle {
		where = "the rubric in " + env.Config.Path
	}
	r := env.Rubric
	var err error
	if r == nil {
		var text []byte
		if text, err = env.Config.Rubric(); err == nil {
			r, err = rules.Compile(text)
		}
	}
	if err != nil {
		return []Problem{{
			Key:     "rubric_invalid:compile",
			Message: where + ": " + err.Error(),
			Fix:     "fix the file (leadscore rules check <file> lists every error)",
		}}
	}
	if env.Model == nil {
		return nil
	}
	out := UnknownFields(r, env.Model, env.Columns)
	if env.Doctor {
		// Doctor fetches no input rows, so a column whose every cell is
		// empty (stored nowhere) looks unknown here; the run judges it with
		// the headers it fetched.
		for i := range out {
			out[i].Warning = true
			out[i].Message += " (doctor reads no input headers, so this may be a column whose every cell is empty; the next run decides)"
		}
	}
	return out
}

// UnknownFieldKind is the Health key kind of a rubric field no input carries:
// rubric_unknown_field:<field>.
const UnknownFieldKind = "rubric_unknown_field"

// UnknownFields returns one problem per field the rubric reads (Fields())
// that is not built in, declared in the rubric, or a loaded column. Loaded
// columns are the fields of every People row, the facts of every Company facts
// row, the Companies tab's headers, and the given raw input headers (the
// run's fetched rows), each resolved as merge resolves headers.
func UnknownFields(r *rules.Rubric, m *model.Model, headers []string) []Problem {
	aliases := r.Aliases()
	known := map[string]bool{
		// Built-in fields with no header spelling.
		"sources_seen": true, "receiver_only": true, "company.domain": true, "company.leads_seen": true,
	}
	for _, f := range api.BuiltinAliases() {
		known[f] = true
	}
	for _, f := range aliases { // declared fields: each one's own name maps to it
		known[f] = true
	}
	for _, p := range m.People {
		for f := range p.Fields {
			known[f] = true
		}
	}
	for _, cf := range m.CompanyFacts {
		for f := range cf.Facts {
			known["company."+f] = true
		}
	}
	table := merge.AliasTable(aliases)
	for _, row := range m.Companies {
		for h := range row {
			// A Companies header naming company.<f> is fact f; any other header
			// is the fact named by its squashed form.
			if f := merge.ResolveHeader(table, h); strings.HasPrefix(f, "company.") {
				known[f] = true
			} else if sq := api.SquashHeader(h); sq != "" {
				known["company."+sq] = true
			}
		}
	}
	for _, h := range headers {
		if f := merge.ResolveHeader(table, h); f != "" {
			known[f] = true
		}
	}
	var out []Problem
	for _, f := range r.Fields() {
		if known[f] {
			continue
		}
		out = append(out, Problem{
			Key:     UnknownFieldKind + ":" + f,
			Message: "the rubric reads " + f + ", which is not built in, declared under fields, or a column of any loaded row",
			Fix:     "fix the name in the rubric, add the column to a source, or declare the field under fields",
		})
	}
	return out
}
