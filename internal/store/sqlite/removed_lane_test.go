// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// A lane is removed from the rubric; its lead later unsubscribes. The old
// export table must still load (from State's export_lane record) so its row
// can be updated to do_not_contact (the export-row rules).
func TestExportTableOfRemovedLaneStillLoads(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "leadscore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	commit := func(m *model.Model) {
		t.Helper()
		w := codec.Encode(m)
		if err := s.Commit(ctx, w); err != nil {
			t.Fatal(err)
		}
		m.Committed(w)
	}

	// Run 1: lane "warm" lists L1.
	m, err := codec.Load(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	m.Put(model.ExportTable("warm"), model.ExportRow{LeadID: "L1", Status: "new", FirstListedAt: at, UpdatedAt: at})
	commit(m)

	// Run 2: "warm" is gone from the rubric, so nothing names it; L1 unsubscribed.
	m, err = codec.Load(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := m.Exports["warm"]["L1"]
	if !ok {
		t.Fatalf("the removed lane's export table was not loaded: %v", m.Exports)
	}
	row.Status, row.DoNotContact, row.UpdatedAt = "unsubscribed", true, at.Add(time.Hour)
	m.Put(model.ExportTable("warm"), row)
	commit(m)

	rows, err := s.ReadTable(ctx, "Export warm")
	if err != nil || len(rows) != 1 || rows[0]["do_not_contact"] != "yes" || rows[0]["status"] != "unsubscribed" {
		t.Errorf("Export warm = %v, %v", rows, err)
	}
}
