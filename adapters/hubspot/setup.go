// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// property is one custom property setup creates.
type property struct {
	object, name, label string
	number, unique      bool
}

// properties are the custom properties, named without the prefix.
func properties() []property {
	return []property{
		{object: "contacts", name: "lead_id", label: "leadscore lead id", unique: true},
		{object: "contacts", name: "lane", label: "leadscore lane"},
		{object: "contacts", name: "tier", label: "leadscore tier"},
		{object: "contacts", name: "priority", label: "leadscore priority"},
		{object: "contacts", name: "reasons", label: "leadscore reasons"},
		{object: "contacts", name: "score", label: "leadscore score", number: true},
		{object: "deals", name: "company_domain", label: "leadscore company domain"},
		{object: "deals", name: "lane", label: "leadscore lane"},
	}
}

func (p property) typ() (typ, field string) {
	if p.number {
		return "number", "number"
	}
	return "string", "text"
}

// existing is a property as the portal lists it.
type existing struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	HasUniqueValue bool   `json:"hasUniqueValue"`
}

// listProperties reads an object's properties by name.
func listProperties(ctx context.Context, c *client, object string) (map[string]existing, error) {
	var out struct {
		Results []existing `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, "/crm/v3/properties/"+object, nil, &out); err != nil {
		return nil, fmt.Errorf("listing %s properties: %w", object, err)
	}
	m := map[string]existing{}
	for _, p := range out.Results {
		m[p.Name] = p
	}
	return m, nil
}

// mismatch says how an existing property differs from what the sink needs,
// or "" when it fits.
func (p property) mismatch(e existing) string {
	typ, _ := p.typ()
	switch {
	case e.Type != typ:
		return fmt.Sprintf("is of type %s, not %s", e.Type, typ)
	case p.unique && !e.HasUniqueValue:
		return "does not require unique values"
	case !p.unique && e.HasUniqueValue:
		return "requires unique values, which it must not (one company can have several deals over time)"
	}
	return ""
}

// Setup is `leadscore setup hubspot`: it creates the `leadscore`
// property group and the custom properties on contacts and deals (keeping
// any that already exist and fit), and resolves the configured pipeline and
// stage by name. It is safe to run again. The private app needs the schema
// write scopes for contacts and deals.
func Setup(ctx context.Context, cfg api.Config, out io.Writer) error {
	s, err := parse(cfg)
	if err != nil {
		return err
	}
	c := s.c
	have := map[string]map[string]existing{}
	for _, object := range []string{"contacts", "deals"} {
		if have[object], err = listProperties(ctx, c, object); err != nil {
			return err
		}
	}
	for _, object := range []string{"contacts", "deals"} {
		err := c.call(ctx, http.MethodPost, "/crm/v3/properties/"+object+"/groups",
			map[string]any{"name": groupName, "label": "leadscore", "displayOrder": -1}, nil)
		if err != nil && statusOf(err) != http.StatusConflict {
			return fmt.Errorf("creating the %s property group on %s: %w", groupName, object, err)
		}
	}
	var bad []string
	for _, p := range properties() {
		name := s.prop(p.name)
		if e, ok := have[p.object][name]; ok {
			if why := p.mismatch(e); why != "" {
				bad = append(bad, fmt.Sprintf("%s property %s %s", strings.TrimSuffix(p.object, "s"), name, why))
				continue
			}
			fmt.Fprintf(out, "kept %s property %s\n", strings.TrimSuffix(p.object, "s"), name)
			continue
		}
		typ, field := p.typ()
		body := map[string]any{"name": name, "label": p.label, "type": typ, "fieldType": field, "groupName": groupName}
		if p.unique {
			body["hasUniqueValue"] = true
		}
		err := c.call(ctx, http.MethodPost, "/crm/v3/properties/"+p.object, body, nil)
		switch {
		case statusOf(err) == http.StatusConflict:
			// Created by someone else since the list was read.
			fmt.Fprintf(out, "kept %s property %s\n", strings.TrimSuffix(p.object, "s"), name)
		case err != nil:
			return fmt.Errorf("creating %s property %s: %w", strings.TrimSuffix(p.object, "s"), name, err)
		default:
			fmt.Fprintf(out, "created %s property %s\n", strings.TrimSuffix(p.object, "s"), name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("these properties exist but do not fit: %s; delete or rename them in HubSpot, or set sinks.hubspot.property_prefix", strings.Join(bad, "; "))
	}
	if s.pipeline == "" {
		fmt.Fprintln(out, "sinks.hubspot.pipeline and stage are not set: a hubspot:deals lane needs them")
		return nil
	}
	p, err := readPipelines(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "deals go to pipeline %q (id %s), stage %q (id %s)\n", s.pipeline, p.pipelineID, s.stage, p.stageID)
	return nil
}
