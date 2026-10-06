package check

import (
	"context"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func init() { Register(rubric{}) }

// rubric fails when the rubric does not compile, or reads a field that is not
// built in, declared, or a loaded column (section 10). S2 owns the compile
// part; S10a the field part, which every run also applies before scoring.
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
	text, err := env.Config.Rubric()
	var r *rules.Rubric
	if err == nil {
		r, err = rules.Compile(text)
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
	return UnknownFields(r, env.Model, env.Columns)
}

// UnknownFieldKind is the Health key kind of a rubric field no input carries:
// rubric_unknown_field:<field>.
const UnknownFieldKind = "rubric_unknown_field"

// UnknownFields returns one problem per field the rubric reads (Fields())
// that is not built in, declared in the rubric, or a loaded column. Loaded
// columns are the fields of every People row, the facts of every Company facts
// row, the Companies tab's headers, and the given raw input headers (the
// run's fetched rows), each resolved through the alias tables as merge does.
func UnknownFields(r *rules.Rubric, m *model.Model, headers []string) []Problem {
	aliases := r.Aliases()
	known := map[string]bool{
		// Built-in fields with no header spelling (RFC 6.4).
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
	table := api.BuiltinAliases()
	for k, v := range aliases {
		table[k] = v
	}
	for _, row := range m.Companies {
		for h := range row {
			// A Companies header naming company.<f> is fact f; any other header
			// is the fact named by its squashed form.
			if f := resolveHeader(h, table); strings.HasPrefix(f, "company.") {
				known[f] = true
			} else if sq := api.SquashHeader(h); sq != "" {
				known["company."+sq] = true
			}
		}
	}
	for _, h := range headers {
		if f := resolveHeader(h, table); f != "" {
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

// resolveHeader names the field a header carries, as merge resolves it: its
// entry in the alias table (the rubric's aliases over the built-in ones),
// else the squashed header.
func resolveHeader(h string, table map[string]string) string {
	sq := api.SquashHeader(h)
	if sq == "" {
		return ""
	}
	if f, ok := table[sq]; ok {
		return f
	}
	return sq
}
