package check

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
)

// stateStore is a Backend whose State table holds the given values.
type stateStore map[string]string

func (s stateStore) ReadTable(_ context.Context, name string) ([]api.Row, error) {
	if name != "State" {
		return nil, nil
	}
	var out []api.Row
	for k, v := range s {
		out = append(out, api.Row{"key": k, "value": v})
	}
	return out, nil
}
func (stateStore) Lease(context.Context, string, time.Duration) (api.RunLease, error) {
	return nil, nil
}
func (stateStore) Commit(context.Context, []api.TableWrite) error { return nil }

func TestStoreCheck(t *testing.T) {
	sqliteCfg := &config.Config{Store: config.Store{Type: "sqlite", Path: "/data/leadscore.db"}}
	sheetsCfg := &config.Config{Store: config.Store{Type: "sheets"}}
	for _, tt := range []struct {
		name        string
		cfg         *config.Config
		state       stateStore
		inContainer bool
		mount       string
		want        []string
	}{
		{"healthy sqlite in a container", sqliteCfg, stateStore{"schema_version": "1.0", "opened_by": "c0ffee"}, true, "ext4", nil},
		{"newer minor is fine", sqliteCfg, stateStore{"schema_version": "1.9"}, true, "ext4", nil},
		{"newer major", sheetsCfg, stateStore{"schema_version": "2.0"}, false, "", []string{"store:newer_schema"}},
		{"overlay disk", sqliteCfg, stateStore{}, true, "overlay", []string{"store:disk_not_kept"}},
		{"tmpfs disk", sqliteCfg, stateStore{}, true, "tmpfs", []string{"store:disk_not_kept"}},
		{"outside the container that opened it", sqliteCfg, stateStore{"opened_by": "c0ffee"}, false, "", []string{"store:opened_outside_container"}},
		{"outside, serve also ran outside (opened_by cleared)", sqliteCfg, stateStore{"opened_by": ""}, false, "", nil},
		{"outside, never opened by serve", sqliteCfg, stateStore{}, false, "", nil},
		{"malformed version", sheetsCfg, stateStore{"schema_version": "+1.0"}, false, "", []string{"store:bad_schema_version"}},
		{"version with no minor", sheetsCfg, stateStore{"schema_version": "1"}, false, "", []string{"store:bad_schema_version"}},
		{"sheets ignores the SQLite cases", sheetsCfg, stateStore{"opened_by": "c0ffee"}, false, "tmpfs", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := storeCheck{
				inContainer: func() bool { return tt.inContainer },
				mountType:   func(string) (string, bool) { return tt.mount, tt.mount != "" },
			}
			var got []string
			for _, p := range c.Run(context.Background(), Env{Config: tt.cfg, Store: tt.state}) {
				got = append(got, p.Key)
				if p.Message == "" || p.Fix == "" || p.Warning {
					t.Errorf("problem %+v needs a message and a fix, and fails (not a warning)", p)
				}
			}
			if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
				t.Errorf("problems = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoreCheckIsRegisteredInRun(t *testing.T) {
	for _, c := range InRun() {
		if c.Name() == "store" {
			return
		}
	}
	t.Error("the store check must be registered and run in every run")
}

func TestMountType(t *testing.T) {
	dir := t.TempDir()
	info := filepath.Join(dir, "mountinfo")
	data := "22 1 0:21 / / rw,relatime - overlay overlay rw\n" +
		"30 22 8:1 /vol /data rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"31 22 0:30 / /data/tmp\\040dir rw - tmpfs tmpfs rw\n"
	if err := os.WriteFile(info, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/data/leadscore.db":         "ext4",
		"/data/tmp dir/leadscore.db": "tmpfs",
		"/home/x/leadscore.db":       "overlay",
		"/database.db":               "overlay",
	} {
		if got, ok := mountType(info, path); !ok || got != want {
			t.Errorf("mountType(%s) = %q, %v; want %q", path, got, ok, want)
		}
	}
	if _, ok := mountType(filepath.Join(dir, "missing"), "/data/x.db"); ok {
		t.Error("no mountinfo must mean unknown")
	}
}
