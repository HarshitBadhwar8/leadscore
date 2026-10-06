package engine

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

// score is step 6: it builds every live lead's LeadRef from the model (the
// merge index is built here, after Intake and Fold, right before Evaluate),
// evaluates under the deadline, logs tier and priority changes against the
// loaded Ranked, and puts the new Ranked rows in the model (written after
// phase 2).
func (x *exec) score(det rules.DetectorResults) error {
	r, m := x.run, x.run.Model
	idx := merge.NewIndex(m, x.cfg.Sources)
	refs := LeadRefs(m, idx)
	in := rules.Input{Leads: refs, Companies: Companies(m), LeadsSeen: idx.LeadsSeen(), Detectors: det}
	res, err := r.Rubric.EvaluateContext(x.dlCtx, in)
	if err != nil {
		if x.past("while scoring") {
			return nil
		}
		return fmt.Errorf("scoring: %w", err)
	}
	x.scored, x.result, x.refs = true, res, refs
	for _, w := range res.Warnings {
		x.log("warn", "rubric_warning", "", w)
	}

	x.oldRanked = make(map[model.Key]model.RankedRow, len(m.Ranked))
	for k, row := range m.Ranked {
		x.oldRanked[k] = row
	}
	derived := r.Rubric.DerivedNames()
	logged := map[string]string{}
	for _, n := range derived {
		if n == "tier" || n == "priority" {
			logged[n] = n + "_change"
		}
	}
	keep := map[model.Key]bool{}
	for _, ref := range refs {
		row := x.rankedRow(ref, derived)
		k := model.Key(ref.ID)
		keep[k] = true
		if old, ok := x.oldRanked[k]; ok {
			for _, n := range derived {
				kind, ok := logged[n]
				if !ok || old.Derived[n] == row.Derived[n] {
					continue
				}
				r.Model.Put(model.TableLog, model.LogEntry{At: r.Now(), RunID: r.ID, Level: "info", LeadID: ref.ID,
					Email: row.Email, Kind: kind, Message: fmt.Sprintf("%s %s -> %s", n, shown(old.Derived[n]), shown(row.Derived[n])),
					RubricVersion: r.Rubric.Version()})
			}
		}
		m.Put(model.TableRanked, row)
	}
	for k := range x.oldRanked {
		if !keep[k] {
			m.Delete(model.TableRanked, k.Parts())
		}
	}
	return nil
}

// rankedRow is one lead's Ranked row. The lane is the highest-priority lane
// whose `when` holds, empty when none does or a rubric conflict blocks the
// lead; S10b's lane checks refine it.
func (x *exec) rankedRow(ref api.LeadRef, derived []string) model.RankedRow {
	v := x.result.Verdicts[ref.ID]
	row := model.RankedRow{
		LeadID: ref.ID, FullName: ref.FullName, CompanyDomain: ref.Domain,
		Derived:      map[string]string{},
		AccountScore: v.AccountScore, ContactScore: v.ContactScore, Score: v.AccountScore + v.ContactScore,
		Status: ref.Status, Reasons: rules.ReasonsText(v), RubricVersion: v.RubricVersion,
	}
	if len(ref.Emails) > 0 {
		row.Email = ref.Emails[0]
	}
	if len(ref.LinkedInURLs) > 0 {
		row.LinkedInURL = ref.LinkedInURLs[0]
	}
	for _, n := range derived {
		row.Derived[n] = formatValue(v.Values[n])
	}
	if why, blocked := x.result.Blocked[ref.ID]; blocked {
		row.Reasons = joinReason(row.Reasons, "blocked: "+why)
	} else if lanes := x.result.Lanes[ref.ID]; len(lanes) > 0 {
		row.Lane = lanes[0]
	}
	return row
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// formatValue writes a derived value in the stored forms (contracts,
// "Formats used everywhere"): plain decimal numbers, booleans as yes or
// empty, times in the one time form, and no value as empty.
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return model.FormatFloat(t)
	case bool:
		if t {
			return "yes"
		}
		return ""
	case time.Time:
		return model.FormatTime(t)
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func shown(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// LeadRefs builds a LeadRef for every live lead, sorted by id, from the model
// and a merge index built over it. Status is the stored (folded) status;
// Verdict is nil.
func LeadRefs(m *model.Model, idx *merge.Index) []api.LeadRef {
	ids := idx.LiveLeads()
	out := make([]api.LeadRef, 0, len(ids))
	for _, id := range ids {
		p := m.People[model.Key(id)]
		fields := make(map[string]string, len(p.Fields))
		for k, f := range p.Fields {
			fields[k] = f.Value
		}
		conflicts := make([]string, 0, len(p.Conflicts))
		for f := range p.Conflicts {
			conflicts = append(conflicts, f)
		}
		sort.Strings(conflicts)
		out = append(out, api.LeadRef{
			ID:             id,
			Emails:         idx.Emails(id),
			LinkedInURLs:   idx.LinkedInURLs(id),
			FullName:       fields[merge.FieldFullName],
			Title:          fields["title"],
			Domain:         fields[merge.FieldDomain],
			Status:         m.Outcomes[model.Key(id)].Status,
			Fields:         fields,
			FirstSeenAt:    p.CreatedAt,
			SourcesSeen:    idx.SourcesSeen(id),
			ReceiverOnly:   idx.ReceiverOnly(id),
			ConflictFields: conflicts,
		})
	}
	return out
}

// Companies turns every Company facts row into the evaluator's CompanyFacts:
// the built-in facts typed, every other fact in Extra.
func Companies(m *model.Model) map[string]api.CompanyFacts {
	out := make(map[string]api.CompanyFacts, len(m.CompanyFacts))
	for _, cf := range m.CompanyFacts {
		f := api.CompanyFacts{Domain: cf.Domain, FetchedAt: cf.EnrichedAt, NotFound: !cf.NotFoundAt.IsZero(), Extra: map[string]string{}}
		for name, fact := range cf.Facts {
			switch name {
			case "name":
				f.Name = fact.Value
			case "region":
				f.Region = fact.Value
			case "funding_stage":
				f.FundingStage = fact.Value
			case "employees":
				if n, err := strconv.ParseFloat(fact.Value, 64); err == nil && n >= 0 && n <= math32 {
					e := int(n)
					f.Employees = &e
				}
			default:
				f.Extra[name] = fact.Value
			}
		}
		out[cf.Domain] = f
	}
	return out
}

// math32 bounds an employee count that converts to int on every platform.
const math32 = 1<<31 - 1
