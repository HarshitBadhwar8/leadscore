// Package codec maps the in-memory model to store tables (contracts section
// 12.2): Encode turns recorded changes into table writes, Load reads a store
// into a model and checks its schema version. Backends only move rows; which
// op each change becomes is decided by model.Model.Writes, behind Encode.
package codec

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// ErrNewerSchema means the store was written by a newer major version; this
// binary refuses it (RFC 6.6, "Schema version").
var ErrNewerSchema = errors.New("store is from a newer major schema version")

// Encode turns the model's recorded changes for the named tables (all when none
// is named) into writes. It is the public name for model.Model.Writes, which
// holds the rules for which op each change becomes. A State name may carry a
// key prefix ("State:cursor:").
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

// loaded is every table a run loads besides State: every section 4 table
// except Log (only appended and trimmed) and Events (read through the
// EventLog), plus the people-owned Companies tab.
func loaded() []string {
	var out []string
	for _, d := range model.Tables {
		if d.Pattern || d.Name == model.TableLog || d.Name == model.TableState {
			continue
		}
		out = append(out, d.Name)
	}
	return append(out, model.TableCompanies)
}

// Load reads the store into a new model. It reads State first and refuses a
// store from a newer major schema version (ErrNewerSchema) before decoding
// anything else. It then reads every table except Log and Events, plus the
// export table of every lane recorded in State (export_lane:<lane id>), so a
// lane removed from the rubric keeps its table loaded. It records a
// schema_version raise when the store's is missing or older; a newer minor is
// kept, never lowered.
func Load(ctx context.Context, b api.Backend) (*model.Model, error) {
	m := model.New()
	read := func(name string) error {
		rows, err := b.ReadTable(ctx, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		return m.Load(name, rows)
	}
	if err := read(model.TableState); err != nil {
		return nil, err
	}
	raise, err := CheckVersion(m.StateValue("schema_version"))
	if err != nil {
		return nil, err
	}
	names := loaded()
	for k := range m.State {
		if lane, ok := strings.CutPrefix(string(k), model.ExportLaneKey); ok && lane != "" {
			names = append(names, model.ExportTable(lane))
		}
	}
	for _, name := range names {
		if err := read(name); err != nil {
			return nil, err
		}
	}
	if raise {
		m.SetState("schema_version", model.SchemaVersion)
	}
	return m, nil
}

// CheckVersion compares a stored schema_version with this binary's. It returns
// ErrNewerSchema for a newer major, ErrBadVersion for a value that is not
// major.minor, and raise=true when the stored version is missing or lower (so
// the caller writes this binary's). A newer minor of the same major is kept.
func CheckVersion(stored string) (raise bool, err error) {
	if stored == "" {
		return true, nil
	}
	sMaj, sMin, err := parseVersion(stored)
	if err != nil {
		return false, err
	}
	oMaj, oMin, _ := parseVersion(model.SchemaVersion)
	switch {
	case sMaj > oMaj:
		return false, fmt.Errorf("%w: the store is %s, this binary writes %s", ErrNewerSchema, stored, model.SchemaVersion)
	case sMaj < oMaj, sMin < oMin:
		return true, nil
	}
	return false, nil
}

// ErrBadVersion means State.schema_version is not major.minor.
var ErrBadVersion = errors.New("State.schema_version is not a major.minor version")

var versionForm = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// parseVersion reads a `major.minor` version: digits only.
func parseVersion(v string) (major, minor int, err error) {
	if !versionForm.MatchString(v) {
		return 0, 0, fmt.Errorf("%w: %q", ErrBadVersion, v)
	}
	a, b, _ := strings.Cut(v, ".")
	major, err = strconv.Atoi(a)
	if err == nil {
		minor, err = strconv.Atoi(b)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %q", ErrBadVersion, v)
	}
	return major, minor, nil
}
