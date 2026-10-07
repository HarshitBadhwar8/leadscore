package check

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

func init() {
	Register(storeCheck{
		getenv:      os.Getenv,
		inContainer: func() bool { _, err := os.Stat("/.dockerenv"); return err == nil },
		mountType:   func(path string) (string, bool) { return mountType("/proc/self/mountinfo", path) },
	})
}

// storeCheck is the `store` check (contracts section 10). S4 owns the schema
// version and SQLite cases; S10a the Cloud Run case; S5 adds the Sheets cases
// and S10b the ledger case (ledger_shrank).
type storeCheck struct {
	getenv      func(string) string
	inContainer func() bool
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
		_, err := codec.CheckVersion(v)
		switch {
		case errors.Is(err, codec.ErrNewerSchema):
			out = append(out, Problem{Key: "store:newer_schema",
				Message: "the store was written by a newer version (schema " + v + "; this binary writes " + model.SchemaVersion + ")",
				Fix:     "install a matching version"})
		case errors.Is(err, codec.ErrBadVersion):
			out = append(out, Problem{Key: "store:bad_schema_version",
				Message: "State.schema_version is " + strconv.Quote(v) + ", not a major.minor version",
				Fix:     "restore the State row from a backup, or set it to the version that wrote the store"})
		}
	}
	// The ledger case (S10b): a ledger with fewer rows than the highest
	// count ever committed lost rows, so it cannot say who was already
	// contacted; the run pushes nothing until they are restored.
	if env.Model != nil {
		if saved, err := strconv.Atoi(state["ledger_rows"]); err == nil && len(env.Model.Pushes) < saved {
			out = append(out, Problem{Key: "ledger_shrank",
				Message: "the ledger (Pushes) has " + strconv.Itoa(len(env.Model.Pushes)) + " rows, but " + strconv.Itoa(saved) +
					" were saved before: rows were deleted, so pushing is blocked",
				Fix: "restore the deleted Pushes rows from a backup or the spreadsheet's version history"})
		}
	}
	if env.Config != nil && c.getenv != nil {
		out = append(out, CloudRunRefusal(env.Config, c.getenv)...)
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
	// serve writes opened_by only when it runs in a container (and clears it
	// otherwise), so a set value seen from outside a container means a
	// container uses the file; WAL locking is not safe across that boundary.
	if by := state["opened_by"]; by != "" && !c.inContainer() {
		out = append(out, Problem{Key: "store:opened_outside_container",
			Message: "this command runs outside a container, but the SQLite file is used by the container " + by,
			Fix:     "run it inside the container: docker compose exec leadscore leadscore ..."})
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

// CloudRunRefusal is the store check's Cloud Run case: Cloud Run keeps no
// files, so a SQLite store or a CSV source path is refused while
// CLOUD_RUN_JOB or K_SERVICE is set (RFC 6.6, "Store and setup pairs"). Every
// run refuses to start on it, as doctor reports it.
func CloudRunRefusal(c *config.Config, getenv func(string) string) []Problem {
	if getenv == nil || (getenv("CLOUD_RUN_JOB") == "" && getenv("K_SERVICE") == "") {
		return nil
	}
	var files []string
	if c.Store.Type == "sqlite" {
		files = append(files, "a SQLite store")
	}
	for _, src := range c.Sources {
		if _, hasPath := src.Block["path"]; hasPath || src.Type == "csv" {
			files = append(files, "the CSV path of source "+src.ID)
		}
	}
	if len(files) == 0 {
		return nil
	}
	return []Problem{{
		Key:     "store:cloud_run_files",
		Message: "Cloud Run keeps no files, but this install uses " + strings.Join(files, " and "),
		Fix:     "on Google Cloud use the Sheets store and Sheet-tab sources",
	}}
}
