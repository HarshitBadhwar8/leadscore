// Package codec maps the in-memory model to store tables (contracts section
// 12.2): Encode turns recorded changes into table writes, Load reads a store
// into a model and checks its schema version. Backends only move rows; this is
// the one place that knows which op each change becomes.
package codec

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// ErrNewerSchema means the store was written by a newer major version; this
// binary refuses it (RFC 6.6, "Schema version").
var ErrNewerSchema = errors.New("store is from a newer major schema version")

// Encode turns the model's recorded changes for the named tables (all when none
// is named) into writes:
//
//   - OpAppend for new rows of Seen events, Window events, Log and Identities,
//     and for new Overrides rows;
//   - OpUpsert for new or changed keyed rows (Applied rows included);
//   - OpDelete for deleted rows, by key (Overrides: by all four columns);
//   - OpReplace for Ranked, the whole table (Chunk splits it);
//   - OpTrim for retention trims, first in the table's writes.
//
// A State name may carry a key prefix ("State:cursor:") to encode only those
// keys. A row put back unchanged produces no write.
func Encode(m *model.Model, tables ...string) []api.TableWrite {
	return m.Writes(tables...)
}

// Chunk splits a write into writes of at most size rows. An OpReplace becomes
// an OpReplace of the first chunk followed by OpAppends (how Ranked is written
// in chunks after phase 2); other ops keep their op. size <= 0 returns w alone.
func Chunk(w api.TableWrite, size int) []api.TableWrite {
	if size <= 0 || len(w.Rows) <= size {
		return []api.TableWrite{w}
	}
	var out []api.TableWrite
	for i := 0; i < len(w.Rows); i += size {
		c := w
		c.Rows = w.Rows[i:min(i+size, len(w.Rows))]
		if i > 0 && w.Op == api.OpReplace {
			c.Op = api.OpAppend
		}
		out = append(out, c)
	}
	return out
}

// loaded is every table a run loads: every section 4 table except Log (only
// appended and trimmed) and Events (read through the EventLog), plus the
// people-owned Companies tab.
func loaded() []string {
	var out []string
	for _, d := range model.Tables {
		if d.Pattern || d.Name == model.TableLog {
			continue
		}
		out = append(out, d.Name)
	}
	return append(out, model.TableCompanies)
}

// Load reads the store into a new model: every table except Log and Events,
// plus the export table of each lane id given. It refuses a store from a newer
// major schema version (ErrNewerSchema), and records a schema_version raise
// when the store's is missing or older; a newer minor is kept, never lowered.
func Load(ctx context.Context, b api.Backend, exportLanes ...string) (*model.Model, error) {
	m := model.New()
	names := loaded()
	for _, lane := range exportLanes {
		names = append(names, model.ExportTable(lane))
	}
	for _, name := range names {
		rows, err := b.ReadTable(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		if err := m.Load(name, rows); err != nil {
			return nil, err
		}
	}
	stored := m.StateValue("schema_version")
	raise, err := CheckVersion(stored)
	if err != nil {
		return nil, err
	}
	if raise {
		m.SetState("schema_version", model.SchemaVersion)
	}
	return m, nil
}

// CheckVersion compares a stored schema_version with this binary's. It returns
// ErrNewerSchema for a newer major, and raise=true when the stored version is
// missing or lower (so the caller writes this binary's). A newer minor of the
// same major is fine and is kept.
func CheckVersion(stored string) (raise bool, err error) {
	if stored == "" {
		return true, nil
	}
	sMaj, sMin, err := ParseVersion(stored)
	if err != nil {
		return false, fmt.Errorf("State.schema_version: %w", err)
	}
	oMaj, oMin, _ := ParseVersion(model.SchemaVersion)
	switch {
	case sMaj > oMaj:
		return false, fmt.Errorf("%w: the store is %s, this binary writes %s", ErrNewerSchema, stored, model.SchemaVersion)
	case sMaj < oMaj, sMin < oMin:
		return true, nil
	}
	return false, nil
}

// ParseVersion reads a `major.minor` version.
func ParseVersion(v string) (major, minor int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(v), ".")
	if ok {
		major, err = strconv.Atoi(a)
		if err == nil {
			minor, err = strconv.Atoi(b)
		}
	}
	if !ok || err != nil || major < 0 || minor < 0 {
		return 0, 0, fmt.Errorf("%q is not a major.minor version", v)
	}
	return major, minor, nil
}
