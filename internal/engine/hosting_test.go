// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// A run reads the hosted keys right after loading leadscore.yml, before any
// adapter is built. A key that cannot be read does not stop the run: the
// secrets check reports it.
func TestRunLoadsHostedKeysFirst(t *testing.T) {
	in := basicInstall(t)
	in.config(leadsCSV + "hosting: { project: p, region: r }\n")
	var seen *config.Config
	hooks := DefaultHooks()
	intake := hooks.Intake
	hooks.Intake = func(r *Run) error {
		if seen == nil {
			t.Error("Intake ran before the keys were read")
		}
		return intake(r)
	}
	_, out, err := in.run(hooks, func(_ *api.RunOptions, s *settings) {
		s.loadKeys = func(_ context.Context, c *config.Config) error {
			seen = c
			return errors.New("HUBSPOT_TOKEN: reading secret hubspot-token: HTTP 403")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil || !seen.Hosted() {
		t.Fatalf("loadKeys got %+v", seen)
	}
	if !strings.Contains(out, "keys from Secret Manager: HUBSPOT_TOKEN") {
		t.Errorf("the failed read is not reported: %q", out)
	}
	if len(in.rows("Ranked")) != 2 {
		t.Errorf("the run did not go on to score: Ranked has %d rows", len(in.rows("Ranked")))
	}
}
