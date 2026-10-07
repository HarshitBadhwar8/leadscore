package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// Test adapters: a stub source whose output each test sets, a sink so a lane
// can name a registered one, and a SQLite store that can refuse commits.
func init() {
	api.RegisterSource("stub", func(cfg api.Config) (api.Source, error) {
		id, _ := cfg["id"].(string)
		return stubSource{id: id}, nil
	})
	api.RegisterSink("testsink", func(api.Config) (api.Sink, error) { return nil, errors.New("not used") })
	api.RegisterBackend("flaky", func(cfg api.Config) (api.Backend, api.EventLog, error) {
		path, _ := cfg["path"].(string)
		s, err := sqlite.Open(path)
		if err != nil {
			return nil, nil, err
		}
		return &flakyStore{Store: s}, s, nil
	})
}

// stubOut is what each stub source returns, by source id.
type stubOut struct {
	rows   []api.InputRow
	events []api.Event
	next   api.Cursor
	err    error
	delay  time.Duration // Fetch sleeps this long first
	seen   []api.Cursor  // the cursors Fetch was called with
}

var (
	stubMu   sync.Mutex
	stubData = map[string]*stubOut{}
)

func setStub(t *testing.T, id string, out *stubOut) {
	stubMu.Lock()
	stubData[id] = out
	stubMu.Unlock()
	t.Cleanup(func() { stubMu.Lock(); delete(stubData, id); stubMu.Unlock() })
}

type stubSource struct{ id string }

func (s stubSource) ID() string { return s.id }
func (s stubSource) Fetch(_ context.Context, c api.Cursor) ([]api.InputRow, []api.Event, api.Cursor, error) {
	stubMu.Lock()
	d := time.Duration(0)
	if o := stubData[s.id]; o != nil {
		d = o.delay
	}
	stubMu.Unlock()
	time.Sleep(d)
	stubMu.Lock()
	defer stubMu.Unlock()
	out := stubData[s.id]
	if out == nil {
		return nil, nil, "", nil
	}
	out.seen = append(out.seen, c)
	return out.rows, out.events, out.next, out.err
}

// flakyStore returns ErrTooLarge for the next tooLarge commits that write
// People, and fails the next failRanked commits that write Ranked.
var flaky struct {
	sync.Mutex
	tooLarge, failRanked int
	// savedWithProblems: the next commits that write People are saved, then
	// answered with ErrCommittedWithProblems, as the Sheets store does when a
	// people tab moved under it; the run must not send those writes again.
	savedWithProblems int
	// failPushes: the next commits that write Pushes fail.
	failPushes int
}

type flakyStore struct{ *sqlite.Store }

func (f *flakyStore) Commit(ctx context.Context, writes []api.TableWrite) error {
	flaky.Lock()
	refuse := false
	if flaky.tooLarge > 0 {
		for _, w := range writes {
			if w.Table == model.TablePeople {
				refuse = true
			}
		}
		if refuse {
			flaky.tooLarge--
		}
	}
	failRanked := false
	if flaky.failRanked > 0 {
		for _, w := range writes {
			if w.Table == model.TableRanked {
				failRanked = true
			}
		}
		if failRanked {
			flaky.failRanked--
		}
	}
	problems := false
	if flaky.savedWithProblems > 0 && slices.ContainsFunc(writes, func(w api.TableWrite) bool { return w.Table == model.TablePeople }) {
		flaky.savedWithProblems--
		problems = true
	}
	failPushes := false
	if flaky.failPushes > 0 && slices.ContainsFunc(writes, func(w api.TableWrite) bool { return w.Table == model.TablePushes }) {
		flaky.failPushes--
		failPushes = true
	}
	flaky.Unlock()
	if failPushes {
		return errors.New("injected Pushes failure")
	}
	if refuse {
		return api.ErrTooLarge
	}
	if problems {
		if err := f.Store.Commit(ctx, writes); err != nil {
			return err
		}
		return fmt.Errorf("%w: rows of Overrides moved while saving", api.ErrCommittedWithProblems)
	}
	if failRanked {
		return errors.New("injected Ranked failure")
	}
	return f.Store.Commit(ctx, writes)
}

// testRubric scores a head-of title as tier 1, pays for a second channel,
// and lists every tier 1 or 2 lead on an export lane.
const testRubric = `version: 1
derive:
  tier:
    - when: { field: title, contains: head }
      then: 1
    - else: 2
score:
  contact:
    - { when: { field: title, contains: head }, points: 5 }
    - { band: sources_seen, points: { 2: 10 } }
lanes:
  - { id: list, kind: export, when: { field: tier, lte: 2 }, push: "export:list" }
`

// install is a folder with leadscore.yml, rubric.yml and a SQLite store.
type install struct {
	t   *testing.T
	dir string
}

// newInstall writes leadscore.yml (the store and version are added) and the rubric.
func newInstall(t *testing.T, body, rubric string) *install {
	t.Helper()
	in := &install{t: t, dir: t.TempDir()}
	in.config(body)
	in.write("rubric.yml", rubric)
	return in
}

func (in *install) config(body string) {
	in.t.Helper()
	if !strings.Contains(body, "store:") {
		body = "store: { type: sqlite, path: leadscore.db }\n" + body
	}
	in.write("leadscore.yml", "version: 1\n"+body)
}

func (in *install) write(name, text string) {
	in.t.Helper()
	if err := os.WriteFile(filepath.Join(in.dir, name), []byte(text), 0o600); err != nil {
		in.t.Fatal(err)
	}
}

func (in *install) opts() api.RunOptions {
	return api.RunOptions{ConfigPath: filepath.Join(in.dir, "leadscore.yml")}
}

// run executes one run with the given hooks, returning its result, output and error.
func (in *install) run(hooks Hooks, mod ...func(*api.RunOptions, *settings)) (api.RunResult, string, error) {
	in.t.Helper()
	var out bytes.Buffer
	opts := in.opts()
	s := settings{hooks: hooks, now: time.Now, out: &out, getenv: func(string) string { return "" }}
	for _, f := range mod {
		f(&opts, &s)
	}
	res, err := execute(context.Background(), opts, s)
	return res, out.String(), err
}

func dry(o *api.RunOptions, _ *settings) { o.DryRun = true }

// store opens the install's SQLite file.
func (in *install) store() *sqlite.Store {
	in.t.Helper()
	s, err := sqlite.Open(filepath.Join(in.dir, "leadscore.db"))
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() { s.Close() })
	return s
}

func (in *install) rows(table string) []api.Row {
	in.t.Helper()
	rows, err := in.store().ReadTable(context.Background(), table)
	if err != nil {
		in.t.Fatal(err)
	}
	return rows
}

// health returns Health as kind:key -> value.
func (in *install) health() map[string]string {
	out := map[string]string{}
	for _, r := range in.rows(model.TableHealth) {
		out[r["kind"]+":"+r["key"]] = r["value"]
	}
	return out
}

func (in *install) state(key string) string {
	for _, r := range in.rows(model.TableState) {
		if r["key"] == key {
			return r["value"]
		}
	}
	return ""
}

// setLease writes the SQLite lease rows directly, as another run would.
func (in *install) setLease(owner string, expires time.Time) {
	in.t.Helper()
	err := in.store().Commit(context.Background(), []api.TableWrite{{
		Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "lease_owner", "value": owner}, {"key": "lease_expires_at", "value": model.FormatTime(expires)}},
	}})
	if err != nil {
		in.t.Fatal(err)
	}
}

// csvRows is a CSV file's text from header and data lines.
func csvText(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// shrink makes the run's fixed times small for the duration of a test.
func shrink(t *testing.T, budget time.Duration) {
	old := saveBudget
	saveBudget = budget
	t.Cleanup(func() { saveBudget = old })
}

func hasKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}
