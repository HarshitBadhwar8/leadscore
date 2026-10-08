package apollo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

func init() { api.RegisterEnricher("apollo", NewEnricher) }

// organizationEnrichPath is Apollo's company lookup by domain: one domain per
// call, one credit per call (the credit cost is unconfirmed).
const organizationEnrichPath = "/api/v1/organizations/enrich"

// FactLatestFundingAt is the Extra fact the enricher writes: the date of the
// latest funding round, as YYYY-MM-DD.
const FactLatestFundingAt = "latest_funding_at"

// FactFundingStage is the funding stage fact's name.
const FactFundingStage = "funding_stage"

// ErrNotFound means Apollo has no organization for the domain: a 200 whose
// organization is null or missing. It is not a failure: plenty of small
// companies are not in its index. The enricher reports it as
// CompanyFacts.NotFound, so the domain waits for the max age.
var ErrNotFound = errors.New("apollo: no organization for that domain")

// Organization is the part of Apollo's company record the facts come from.
type Organization struct {
	Name string
	// Employees is nil when Apollo gave no headcount: zero would read as a
	// company with no staff.
	Employees *int
	// FundingStage is Apollo's own label, unmapped.
	FundingStage string
	// FundingDate is the latest round's date as sent; empty when absent.
	FundingDate string
	Country     string
}

// EnrichOrganization looks one company up by domain, with the retrying call.
// A 200 carrying no organization is ErrNotFound, which must not overwrite good
// facts with blanks. Unconfirmed: does Apollo also answer 404 for an unknown
// domain? Until then a 404 is an ordinary failure (the path could be wrong),
// so it never marks a company not-found for a whole max age.
//
// The field names below are the ones enrichment code running in production
// reads.
func (c *Client) EnrichOrganization(ctx context.Context, domain string) (*Organization, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, errors.New("apollo: enrichment needs a domain")
	}
	var reply struct {
		Organization *struct {
			Name                   string `json:"name"`
			EstimatedNumEmployees  *int   `json:"estimated_num_employees"`
			LatestFundingStage     string `json:"latest_funding_stage"`
			LatestFundingRoundDate string `json:"latest_funding_round_date"`
			Country                string `json:"country"`
		} `json:"organization"`
	}
	err := c.DoRetrying(ctx, Request{Method: http.MethodGet, Path: organizationEnrichPath,
		Query: url.Values{"domain": {domain}}}, &reply)
	switch {
	case err != nil:
		return nil, err
	case reply.Organization == nil:
		return nil, ErrNotFound
	}
	o := reply.Organization
	return &Organization{
		Name:         o.Name,
		Employees:    o.EstimatedNumEmployees,
		FundingStage: o.LatestFundingStage,
		FundingDate:  o.LatestFundingRoundDate,
		Country:      o.Country,
	}, nil
}

// Enricher is the `apollo` enricher.
type Enricher struct {
	c   *Client
	now func() time.Time
}

// NewEnricher builds the enricher from the `enrich` block: the key from
// APOLLO_API_KEY, and the test keys base_url and _http_client.
func NewEnricher(cfg api.Config) (api.Enricher, error) {
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Enricher{c: c, now: time.Now}, nil
}

// Enrich looks up domains in order, at most budget of them. A domain Apollo
// does not know comes back with NotFound set. A failure on one domain (a 5xx,
// a 404, a timeout, a reply it cannot read) is skipped: that company keeps
// its stored facts. A rate limit that outlasts the retries stops the call and
// returns the facts so far with ErrRateLimited.
//
// Three more stop it, since every later domain would likely fail the same
// way: the key refused (401 or 403), ctx done, and MaxFailuresInARow
// failures in a row. ErrWaitStopped (the run stopped new calls during a
// retry wait) also ends it, returned as is. The engine calls it one domain at a time.
func (e *Enricher) Enrich(ctx context.Context, domains []string, budget int) ([]api.CompanyFacts, error) {
	var out []api.CompanyFacts
	failures := 0
	for i, d := range domains {
		if i >= budget {
			break
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		org, err := e.c.EnrichOrganization(ctx, d)
		switch {
		case errors.Is(err, ErrNotFound):
			out = append(out, api.CompanyFacts{Domain: d, FetchedAt: e.now().UTC(), NotFound: true})
		case errors.Is(err, api.ErrRateLimited), errors.Is(err, ErrWaitStopped), KeyRefused(err):
			return out, err
		case ctx.Err() != nil:
			return out, fmt.Errorf("apollo: enrichment stopped: %w", err)
		case err != nil:
			if failures++; failures >= MaxFailuresInARow {
				return out, fmt.Errorf("apollo: enrichment stopped after %d failures in a row: %w", failures, err)
			}
			continue
		default:
			out = append(out, CompanyFromOrganization(d, org, e.now().UTC()))
		}
		failures = 0
	}
	return out, nil
}

// MaxFailuresInARow is how many lookups in a row may fail (no answer at all)
// before enrichment stops for the run.
const MaxFailuresInARow = 3

// CompanyFromOrganization maps Apollo's record onto company facts. Both
// funding mappings fail soft rather than losing the whole company over one
// field. A field Apollo left out stays empty, which keeps the stored fact. A
// funding label or date it sent that cannot be mapped is put in Extra with an
// empty value, which clears the stored fact, so a stale
// stage does not outlive the vendor's change.
func CompanyFromOrganization(domain string, o *Organization, fetched time.Time) api.CompanyFacts {
	f := api.CompanyFacts{
		Domain:       strings.ToLower(strings.TrimSpace(domain)),
		Name:         strings.TrimSpace(o.Name),
		Region:       strings.TrimSpace(o.Country),
		FundingStage: FundingStage(o.FundingStage),
		FetchedAt:    fetched,
		Extra:        map[string]string{},
	}
	if o.Employees != nil && *o.Employees >= 0 {
		n := *o.Employees
		f.Employees = &n
	}
	if strings.TrimSpace(o.FundingStage) != "" && f.FundingStage == "" {
		f.Extra[FactFundingStage] = "" // a label we do not map: clear the stored stage
	}
	if d := fundingDate(o.FundingDate); d != "" {
		f.Extra[FactLatestFundingAt] = d
	} else if strings.TrimSpace(o.FundingDate) != "" {
		f.Extra[FactLatestFundingAt] = "" // a date we cannot read: clear the stored one
	}
	return f
}

// fundingStages maps Apollo's labels, lowercased and trimmed, onto the fixed
// values. Only the rounds listed have a value; an angel
// round, a grant or a post-IPO label is empty.
var fundingStages = map[string]string{
	"pre-seed": "pre_seed",
	"pre seed": "pre_seed",
	"seed":     "seed",
	"series a": "series_a",
	"series b": "series_b",
	"series c": "series_c",
	"series d": "series_d_plus",
	"series e": "series_d_plus",
	"series f": "series_d_plus",
	"series g": "series_d_plus",
	"series h": "series_d_plus",
	"series i": "series_d_plus",
	"series j": "series_d_plus",
}

// FundingStage maps one Apollo funding label to its fixed value, or "".
func FundingStage(label string) string {
	return fundingStages[strings.ToLower(strings.TrimSpace(label))]
}

// fundingDate reads the round date as YYYY-MM-DD. Apollo sends a date, as
// production enrichment code reads it; a full timestamp is cut to its UTC
// date. Anything else is "".
func fundingDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.Format(time.DateOnly)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	return ""
}
