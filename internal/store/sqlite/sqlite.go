// Package sqlite is the built-in SQLite store:
// a Backend and EventLog over one database file in WAL mode with a busy
// timeout, so `leadscore serve` can append events during a run. Every table
// column is text; the run lease is a pair of State rows; events use
// AUTOINCREMENT so a deleted sequence number is never reused.
//
// It uses modernc.org/sqlite, a pure-Go driver, so the binary stays static.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

func init() {
	api.RegisterBackend("sqlite", func(cfg api.Config) (api.Backend, api.EventLog, error) {
		path, _ := cfg["path"].(string)
		if path == "" {
			return nil, nil, errors.New("sqlite store: `store.path` is required")
		}
		s, err := Open(path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	})
}

// busyTimeout is how long one statement waits for another writer's lock;
// openBusyWait how long Open retries a busy file.
const (
	busyTimeout  = 10 * time.Second
	openBusyWait = 10 * time.Second
)

// Store is a SQLite Backend, EventLog and LeaseInspector.
type Store struct {
	db  *sql.DB
	now func() time.Time // the lease clock
}

var (
	_ api.Backend        = (*Store)(nil)
	_ api.EventLog       = (*Store)(nil)
	_ api.LeaseInspector = (*Store)(nil)
)

// Open opens (creating when missing) the database file at path. The folder
// must exist. Tables are created by the first write that names them.
func Open(path string) (*Store, error) {
	// The path goes into the driver's DSN, where "?" starts options and
	// "file:" makes a URI; refuse both rather than misread them.
	if path == "" || strings.Contains(path, "?") || strings.HasPrefix(strings.ToLower(path), "file:") {
		return nil, fmt.Errorf("sqlite store: %q is not a plain file path (no \"?\", no \"file:\")", path)
	}
	// Create the file owner-only before SQLite opens it; SQLite gives the -wal
	// and -shm files it creates the same mode.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		f.Close()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	// An existing file (and its -wal and -shm) is made owner-only too.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("sqlite store %s: %w", p, err)
		}
	}
	// _txlock=immediate takes the write lock when a transaction begins, so two
	// writers queue on the busy timeout instead of failing to upgrade a lock.
	// synchronous(FULL) makes a commit durable before it returns (AppendEvents).
	dsn := path + "?_txlock=immediate" +
		fmt.Sprintf("&_pragma=busy_timeout(%d)", busyTimeout.Milliseconds()) +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	// Two processes opening a new file at once can meet SQLITE_BUSY while the
	// first sets WAL mode, before the busy timeout applies; wait a little.
	err = db.Ping()
	for wait := time.Now().Add(openBusyWait); isBusy(err) && time.Now().Before(wait); err = db.Ping() {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// OpenReadOnly opens an existing database file for reading only, as doctor
// does: SQLite's read-only mode, no chmod and no journal pragma, so it never
// creates the file, changes its mode, switches its journal, folds a leftover
// WAL into it or rolls back a hot journal. A file that needs one of those
// fails to open (or to read) instead of being changed.
func OpenReadOnly(path string) (*Store, error) {
	if path == "" || strings.Contains(path, "?") || strings.HasPrefix(strings.ToLower(path), "file:") {
		return nil, fmt.Errorf("sqlite store: %q is not a plain file path (no \"?\", no \"file:\")", path)
	}
	// A file: URI needs an absolute path: a relative one would read as the
	// URI's authority. Path escaping handles "#" and spaces.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sqlite store: %w", err)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	u := url.URL{Scheme: "file", Path: path}
	dsn := u.String() + "?mode=ro" + fmt.Sprintf("&_pragma=busy_timeout(%d)", busyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// TableName maps a store table name to its SQLite name: lower snake case
// ("Company facts" is company_facts, "Export warm" is export_warm).
func TableName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "_")
}

func quote(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// ReadTable returns every row of a table in insertion order; a missing table
// returns no rows and no error.
func (s *Store) ReadTable(ctx context.Context, name string) ([]api.Row, error) {
	cols, err := columns(ctx, s.db, TableName(name))
	if err != nil || len(cols) == 0 {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+quote(TableName(name))+" ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []api.Row
	vals := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		r := make(api.Row, len(names))
		for i, n := range names {
			r[n] = text(vals[i])
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// querier is what columns needs: a *sql.DB or a *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// columns returns a table's columns in order; none when the table is missing.
func columns(ctx context.Context, q querier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Commit applies every write in one transaction, all-or-nothing. It checks
// every write before applying any, creates a missing table and appends a
// missing column the first time a write names it. SQLite has no request size
// limit, so it never returns ErrTooLarge.
func (s *Store) Commit(ctx context.Context, writes []api.TableWrite) error {
	for _, w := range writes {
		if err := validate(w); err != nil {
			return err
		}
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, w := range writes {
			if err := apply(ctx, tx, w); err != nil {
				return fmt.Errorf("%s: %w", w.Table, err)
			}
		}
		return nil
	})
}

func validate(w api.TableWrite) error {
	if w.Table == "" {
		return errors.New("a table write names no table")
	}
	if strings.ContainsRune(w.Table, 0) {
		return errors.New("a table name holds a NUL character")
	}
	if TableName(w.Table) == "events" {
		return errors.New("Events is written only through AppendEvents")
	}
	// Column names: SQLite ignores case, so one write may not name two columns
	// that differ only by case; rowid and its aliases are SQLite's own.
	seen := map[string]string{}
	check := func(c string) error {
		switch {
		case c == "" || strings.ContainsRune(c, 0):
			return fmt.Errorf("%s: a column name is empty or holds a NUL character", w.Table)
		case reservedColumn(c):
			return fmt.Errorf("%s: %q is SQLite's own column name", w.Table, c)
		}
		if prev, ok := seen[strings.ToLower(c)]; ok && prev != c {
			return fmt.Errorf("%s: columns %q and %q differ only by case", w.Table, prev, c)
		}
		seen[strings.ToLower(c)] = c
		return nil
	}
	for _, r := range w.Rows {
		for c := range r {
			if err := check(c); err != nil {
				return err
			}
		}
	}
	for _, c := range w.Key {
		if err := check(c); err != nil {
			return err
		}
	}
	if w.Column != "" {
		if err := check(w.Column); err != nil {
			return err
		}
	}
	switch w.Op {
	case api.OpReplace, api.OpAppend:
	case api.OpUpsert, api.OpDelete:
		if len(w.Key) == 0 {
			return fmt.Errorf("%s: an upsert or delete needs Key columns", w.Table)
		}
	case api.OpTrim:
		if w.Column == "" || w.Before.IsZero() {
			return fmt.Errorf("%s: a trim needs Column and Before", w.Table)
		}
	default:
		return fmt.Errorf("%s: unknown write op %d", w.Table, w.Op)
	}
	return nil
}

// inTx runs f in a write transaction and commits it.
func (s *Store) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func reservedColumn(c string) bool {
	switch strings.ToLower(c) {
	case "rowid", "oid", "_rowid_":
		return true
	}
	return false
}

func apply(ctx context.Context, tx *sql.Tx, w api.TableWrite) error {
	table := TableName(w.Table)
	existing, err := columns(ctx, tx, table)
	if err != nil {
		return err
	}
	w = canonical(w, existing)
	named := map[string]bool{}
	for _, r := range w.Rows {
		for c := range r {
			named[c] = true
		}
	}
	for _, c := range w.Key {
		named[c] = true
	}
	if w.Column != "" {
		named[w.Column] = true
	}
	if len(existing) == 0 && (w.Op == api.OpDelete || w.Op == api.OpTrim) {
		return nil // nothing to delete from a table that does not exist
	}
	if err := ensureTable(ctx, tx, w.Table, existing, named); err != nil {
		return err
	}
	q := quote(table)
	switch w.Op {
	case api.OpReplace:
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+q); err != nil {
			return err
		}
		return insert(ctx, tx, q, w.Rows)
	case api.OpAppend:
		return insert(ctx, tx, q, w.Rows)
	case api.OpUpsert:
		return upsert(ctx, tx, q, w.Key, w.Rows)
	case api.OpDelete:
		for _, r := range w.Rows {
			where, args := keyWhere(w.Key, r)
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+q+" WHERE "+where, args...); err != nil {
				return err
			}
		}
		return nil
	case api.OpTrim:
		c := quote(w.Column)
		_, err := tx.ExecContext(ctx, "DELETE FROM "+q+" WHERE "+c+" <> '' AND "+c+" < ?", model.FormatTime(w.Before))
		return err
	}
	return nil
}

// canonical respells a write's columns as the table already spells them (or,
// for a table not yet created, as the store tables do), since SQLite column
// names ignore case: "Score" in a row writes the existing "score" column.
func canonical(w api.TableWrite, existing []string) api.TableWrite {
	spell := map[string]string{}
	if def, ok := model.Def(w.Table); ok && len(existing) == 0 {
		for _, c := range def.Columns {
			spell[strings.ToLower(c)] = c
		}
	}
	for _, c := range existing {
		spell[strings.ToLower(c)] = c
	}
	name := func(c string) string {
		if s, ok := spell[strings.ToLower(c)]; ok {
			return s
		}
		return c
	}
	out := w
	out.Rows = make([]api.Row, len(w.Rows))
	for i, r := range w.Rows {
		nr := make(api.Row, len(r))
		for c, v := range r {
			nr[name(c)] = v
		}
		out.Rows[i] = nr
	}
	out.Key = make([]string, len(w.Key))
	for i, c := range w.Key {
		out.Key[i] = name(c)
	}
	if w.Column != "" {
		out.Column = name(w.Column)
	}
	return out
}

// ensureTable creates the table when missing, with the fixed columns of a
// known table first (Ranked's derived columns after company_domain), and
// appends any named column it lacks. A known keyed table gets a unique index
// on its key.
func ensureTable(ctx context.Context, tx *sql.Tx, name string, existing []string, named map[string]bool) error {
	table := TableName(name)
	def, known := model.Def(name)
	if len(existing) == 0 {
		var cols []string
		if known {
			cols = append(cols, def.Columns...)
		}
		var extra []string
		for c := range named {
			if !contains(cols, c) {
				extra = append(extra, c)
			}
		}
		sort.Strings(extra)
		if known && def.DynamicAfter != "" {
			at := indexOf(cols, def.DynamicAfter) + 1
			cols = append(cols[:at:at], append(extra, cols[at:]...)...)
		} else {
			cols = append(cols, extra...)
		}
		if len(cols) == 0 {
			return nil // no column named yet: create it on a write that names one
		}
		defs := make([]string, len(cols))
		for i, c := range cols {
			defs[i] = quote(c) + " TEXT NOT NULL DEFAULT ''"
		}
		if _, err := tx.ExecContext(ctx, "CREATE TABLE "+quote(table)+" ("+strings.Join(defs, ", ")+")"); err != nil {
			return fmt.Errorf("creating %s: %w", table, err)
		}
		if known && len(def.Key) > 0 {
			key := make([]string, len(def.Key))
			for i, c := range def.Key {
				key[i] = quote(c)
			}
			_, err := tx.ExecContext(ctx, "CREATE UNIQUE INDEX "+quote(table+"_key")+" ON "+quote(table)+
				" ("+strings.Join(key, ", ")+")")
			if err != nil {
				return fmt.Errorf("indexing %s: %w", table, err)
			}
		}
		if name == model.TableWindowEvents {
			if _, err := tx.ExecContext(ctx, `CREATE INDEX "window_events_at" ON "window_events" ("at")`); err != nil {
				return fmt.Errorf("indexing %s: %w", table, err)
			}
		}
		return nil
	}
	var missing []string
	for c := range named {
		if !contains(existing, c) {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	for _, c := range missing {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quote(table)+" ADD COLUMN "+quote(c)+" TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("adding column %s to %s: %w", c, table, err)
		}
	}
	return nil
}

func insert(ctx context.Context, tx *sql.Tx, q string, rows []api.Row) error {
	for _, r := range rows {
		if len(r) == 0 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+q+" DEFAULT VALUES"); err != nil {
				return err
			}
			continue
		}
		cols := sortedCols(r)
		names := make([]string, len(cols))
		args := make([]any, len(cols))
		for i, c := range cols {
			names[i], args[i] = quote(c), r[c]
		}
		stmt := "INSERT INTO " + q + " (" + strings.Join(names, ", ") + ") VALUES (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// upsert updates the rows matching each row's key columns, or inserts the row
// when none matches. Only the columns a row names are written, so a column the
// writer does not know keeps its value.
func upsert(ctx context.Context, tx *sql.Tx, q string, key []string, rows []api.Row) error {
	for _, r := range rows {
		where, wargs := keyWhere(key, r)
		var sets []string
		var args []any
		for _, c := range sortedCols(r) {
			if !contains(key, c) {
				sets = append(sets, quote(c)+" = ?")
				args = append(args, r[c])
			}
		}
		var n int64
		if len(sets) > 0 {
			res, err := tx.ExecContext(ctx, "UPDATE "+q+" SET "+strings.Join(sets, ", ")+" WHERE "+where, append(args, wargs...)...)
			if err != nil {
				return err
			}
			if n, err = res.RowsAffected(); err != nil {
				return err
			}
		} else if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+q+" WHERE "+where, wargs...).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			full := api.Row{}
			for k, v := range r {
				full[k] = v
			}
			for _, k := range key {
				full[k] = r[k]
			}
			if err := insert(ctx, tx, q, []api.Row{full}); err != nil {
				return err
			}
		}
	}
	return nil
}

func keyWhere(key []string, r api.Row) (string, []any) {
	parts := make([]string, len(key))
	args := make([]any, len(key))
	for i, c := range key {
		parts[i], args[i] = quote(c)+" = ?", r[c]
	}
	return strings.Join(parts, " AND "), args
}

func sortedCols(r api.Row) []string {
	out := make([]string, 0, len(r))
	for c := range r {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// isBusy reports SQLite's "database is locked" answers, its form of "slow down".
func isBusy(err error) bool {
	var e *sqlite.Error
	if errors.As(err, &e) {
		switch e.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// InContainer reports whether this process runs in a container (/.dockerenv
// exists). A variable, so tests can replace it.
var InContainer = func() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// MarkOpenedBy records in State.opened_by the hostname of a `serve` running in
// a container, and clears it when `serve` runs outside one. `serve` calls it
// when it opens the store, so the `store` check can tell when a binary outside
// the container opens a file the container uses. Other stores are left alone.
func MarkOpenedBy(ctx context.Context, b api.Backend) error {
	host := ""
	if InContainer() {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("reading the hostname: %w", err)
		}
		host = h
	}
	return markOpenedBy(ctx, b, host)
}

func markOpenedBy(ctx context.Context, b api.Backend, host string) error {
	if _, ok := b.(*Store); !ok {
		return nil
	}
	return b.Commit(ctx, []api.TableWrite{{
		Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{{"key": "opened_by", "value": host}},
	}})
}
