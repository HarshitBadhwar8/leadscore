// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"fmt"

	"github.com/HarshitBadhwar8/leadscore/internal/merge"
)

// normDomain normalizes a domain as merge does for leads (lowercase,
// trimmed, no scheme, port, path or www.), so a HubSpot value and a lead's
// domain compare equal whenever they name the same company.
func normDomain(d string) string { return merge.NormalizeDomain(d) }

// dealCompanyDomains reads, for each deal, the domains of the company
// records it is linked to (normalized, empty ones left out).
func dealCompanyDomains(ctx context.Context, c *client, dealIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(dealIDs) == 0 {
		return out, nil
	}
	links, err := c.associations(ctx, "deals", "companies", dealIDs)
	if err != nil {
		return nil, fmt.Errorf("reading deals' companies: %w", err)
	}
	var ids []string
	for _, cs := range links {
		ids = append(ids, cs...)
	}
	ids = sortedIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	comps, err := c.batchRead(ctx, "companies", "", ids, []string{"domain"})
	if err != nil {
		return nil, fmt.Errorf("reading companies: %w", err)
	}
	dom := map[string]string{}
	for _, o := range comps {
		dom[o.ID] = normDomain(o.prop("domain"))
	}
	for deal, cs := range links {
		for _, id := range cs {
			if d := dom[id]; d != "" {
				out[deal] = append(out[deal], d)
			}
		}
	}
	return out, nil
}

// elsewhere reports evidence that a deal linked to a contact at the company
// belongs to another company: its domain property names another domain, or
// it is linked to company records none of which has this domain while one
// has another. A deal with no such evidence (a salesperson's deal with no
// domain property and no company) counts for the company: wrongly holding
// a company is safe, wrongly releasing it is not.
func elsewhere(domain, prop string, companyDomains []string) bool {
	if p := normDomain(prop); p != "" && p != domain {
		return true
	}
	if len(companyDomains) == 0 {
		return false
	}
	for _, d := range companyDomains {
		if d == domain {
			return false
		}
	}
	return true
}
