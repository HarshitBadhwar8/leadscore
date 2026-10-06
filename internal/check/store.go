package check

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

func init() {
	Register(storeCheck{
		inContainer: func() bool { _, err := os.Stat("/.dockerenv"); return err == nil },
		hostname:    os.Hostname,
		mountType:   func(path string) (string, bool) { return mountType("/proc/self/mountinfo", path) },
	})
}

// storeCheck is the `store` check (contracts section 10). S4 owns the schema
// version and SQLite cases; S5 adds the Sheets cases and S10b the ledger case.
type storeCheck struct {
	inContainer func() bool
	hostname    func() (string, error)
	// mountType returns the filesystem type of the mount holding path, and
	// false when it cannot tell (any system without /proc/self/mountinfo).
	mountType func(path string) (string, bool)
}

func (storeCheck) Name() string { return "store" }
func (storeCheck) InRun() bool  { return true }

func (c storeCheck) Run(ctx context.Context, env Env) []Problem {
	// State is read from the store directly, so the version case works even
	// when the model could not load (a newer major refuses to load).
	state := map[string]string{}
	if env.Store != nil {
		// A store that cannot be read skips these cases; opening it is reported
		// where it is opened.
		rows, _ := env.Store.ReadTable(ctx, model.TableState)
		for _, r := range rows {
			state[r["key"]] = r["value"]
		}
	} else if env.Model != nil {
		for k, r := range env.Model.State {
			state[string(k)] = r.Value
		}
	}

	var out []Problem
	if v := state["schema_version"]; v != "" {
		if _, err := codec.CheckVersion(v); errors.Is(err, codec.ErrNewerSchema) {
			out = append(out, Problem{Key: "store:newer_schema",
				Message: "the store was written by a newer version (schema " + v + "; this binary writes " + model.SchemaVersion + ")",
				Fix:     "install a matching version"})
		}
	}
	if env.Config == nil || env.Config.Store.Type != "sqlite" {
		return out
	}
	path := env.Config.Store.Path
	if fs, ok := c.mountType(path); ok && (fs == "overlay" || fs == "tmpfs") {
		out = append(out, Problem{Key: "store:disk_not_kept",
			Message: "the SQLite file " + path + " is on a disk that is not kept (" + fs + "): it is lost when the container goes",
			Fix:     "put store.path on a named volume"})
	}
	// opened_by names the host of the `serve` that last opened the file. A
	// different name, seen from outside a container, means a container uses the
	// file; WAL locking is not safe across that boundary.
	if by := state["opened_by"]; by != "" && !c.inContainer() {
		if host, err := c.hostname(); err == nil && host != by {
			out = append(out, Problem{Key: "store:opened_outside_container",
				Message: "this command runs outside a container, but the SQLite file is used by the container " + by,
				Fix:     "run it inside the container: docker compose exec leadscore leadscore ..."})
		}
	}
	return out
}

// mountType reads a mountinfo file and returns the filesystem type of the
// longest mount point holding path (or its folder, when the file is missing).
func mountType(mountinfo, path string) (string, bool) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return "", false
	}
	defer f.Close()
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(real, filepath.Base(abs))
	}
	best, fs := -1, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, x := range fields {
			if x == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 5 || sep < 0 || sep+1 >= len(fields) {
			continue
		}
		mp := unescapeMount(fields[4])
		if mp == "/" || abs == mp || strings.HasPrefix(abs, mp+"/") {
			if len(mp) > best {
				best, fs = len(mp), fields[sep+1]
			}
		}
	}
	return fs, best >= 0
}

// unescapeMount decodes mountinfo's octal escapes (\040 is a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
