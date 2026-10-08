// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
	"github.com/HarshitBadhwar8/leadscore/storetest"
)

// The SQLite store's proof: the public conformance suite, green on SQLite.
func TestStoretest(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (api.Backend, api.EventLog) {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "leadscore.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s, s
	})
}
