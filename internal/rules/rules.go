// Package rules is the rubric compiler and evaluator (RFC 6.4, contracts
// sections 2 and 12.3). A rubric is one YAML file; Compile checks it and turns
// every condition into one CEL program, and Evaluate runs it over a run's leads:
// company rollups and company derive blocks, then each lead's derive blocks,
// then both score halves. It reads only what Input carries and computes nothing
// merge produces.
package rules

import (
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Input is what one evaluation reads. The engine builds it from the model; S3
// builds it straight from fixture rows.
type Input struct {
	// Leads carry their merged fields, folded Status, SourcesSeen, ReceiverOnly,
	// ConflictFields and FirstSeenAt (rollups take leads oldest first).
	Leads []api.LeadRef
	// Companies holds each company's resolved facts, by domain. A lead whose
	// Domain has no entry still has a company: one with only its domain.
	Companies map[string]api.CompanyFacts
	// LeadsSeen is company.leads_seen, by domain; a missing domain leaves it absent.
	LeadsSeen map[string]int
	Detectors DetectorResults
}

// DetectorResults says which detectors fired (S9 computes them): lead-subject
// detectors per lead, company-subject detectors per company domain. A detector
// missing from a map did not fire.
type DetectorResults struct {
	Leads     map[api.LeadID]map[string]bool
	Companies map[string]map[string]bool
}

// DetectorSpec is one entry of the rubric's `detectors` block, parsed and
// checked; S9 evaluates it. Fields a kind does not use are zero.
type DetectorSpec struct {
	Name    string
	Kind    string // count_in_window, first_seen, change, or a registered kind
	Subject string // lead or company
	Event   string // count_in_window, first_seen; may end in * to match a prefix
	Window  time.Duration
	Min     int
	Within  time.Duration // first_seen, change
	Field   string        // change: the company fact, without the company. prefix
	From    *string       // change: optional
	To      *string       // change: optional
	Params  api.Config    // a registered kind's `params:`
}

// Lane is one entry of the rubric's `lanes` block. Its `when` is compiled with
// the rest of the rubric; MatchLanes evaluates it.
type Lane struct {
	ID       string
	Name     string
	Kind     string // cold, non-cold or export
	Priority int
	Push     string // as written: <sink>:<destination>
	Sink     string // the part before the first colon
	Dest     string // the part after it
	When     string // the condition, rendered for people; empty means always
	when     *condition
}

// Limits is the rubric's `limits` block with defaults applied.
type Limits struct {
	MaxPushesPerRun int
	MaxPushesPerDay int
	Timezone        string
	Location        *time.Location
}

// Default limits (contracts section 2).
const (
	DefaultMaxPushesPerRun = 100
	DefaultMaxPushesPerDay = 200
	DefaultTimezone        = "UTC"
)

// Rubric is a compiled rubric. It is safe for concurrent use.
type Rubric struct {
	version       string
	settings      map[string]any
	leadFields    map[string]*fieldDef
	companyFields map[string]*fieldDef
	aliases       map[string]string
	rollups       []*rollup
	detectors     []DetectorSpec
	derive        []*deriveBlock
	conflicts     []string
	account       []*scoreRule
	contact       []*scoreRule
	limits        Limits
	lanes         []Lane
	reads         []string
	warnings      []string
}

// Version is `r-` plus the first 16 hex characters of the SHA-256 of the
// rubric re-marshalled with sorted keys and no comments.
func (r *Rubric) Version() string { return r.version }

// Fields returns every input field the rubric reads, sorted: lead fields by
// name, company facts as `company.<name>`. Derived names, rollups, `status` and
// detectors are not input fields and are left out. S10a fails a run when one is
// not built in, declared, or a loaded column.
func (r *Rubric) Fields() []string { return append([]string(nil), r.reads...) }

// Aliases returns the header spellings the rubric's declared fields add, as
// squashed header to field name (`company.<name>` for a company field): each
// declared field's own squashed name and its aliases. They win over the
// built-in table on a clash (contracts section 2).
func (r *Rubric) Aliases() map[string]string {
	out := make(map[string]string, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// ConflictFields returns the fields named in `conflicts`, in file order.
func (r *Rubric) ConflictFields() []string { return append([]string(nil), r.conflicts...) }

// Lanes returns the lanes in file order.
func (r *Rubric) Lanes() []Lane { return append([]Lane(nil), r.lanes...) }

// Limits returns the push limits.
func (r *Rubric) Limits() Limits { return r.limits }

// Detectors returns the detectors in file order.
func (r *Rubric) Detectors() []DetectorSpec {
	out := make([]DetectorSpec, len(r.detectors))
	copy(out, r.detectors)
	return out
}

// DerivedNames returns the derived names in file order: the `Ranked` columns.
func (r *Rubric) DerivedNames() []string {
	out := make([]string, len(r.derive))
	for i, d := range r.derive {
		out[i] = d.name
	}
	return out
}

// Warnings returns what Compile noticed but accepted, such as a derived name
// shadowing an input field. `rules check` prints them.
func (r *Rubric) Warnings() []string { return append([]string(nil), r.warnings...) }
