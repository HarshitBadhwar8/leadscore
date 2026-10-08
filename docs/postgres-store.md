# A Postgres store, as a plug-in

leadscore ships two stores: SQLite and Google Sheets. A team that already runs
Postgres can add its own store without forking: a store is any type that
implements `leadscore.Backend` and `leadscore.EventLog`, registered under a
`store.type` in a small program of your own. This page shows one way to
write it.

**The snippets below are untested.** They show the shape and the parts that
are easy to get wrong; your store is correct when the conformance suite,
`storetest.Run`, passes against it (last section). Nothing here is part of
leadscore's own tests.

## Your own binary

A plug-in is registered in `init` and the CLI is `leadscore.Main`. Build this
instead of the released binary (and your own image from it, if you use
Docker):

```go
package main

import (
	"github.com/HarshitBadhwar8/leadscore"
	"example.com/yourteam/pgstore"
)

func init() { leadscore.RegisterBackend("postgres", pgstore.Open) }

func main() { leadscore.Main() }
```

`leadscore.yml` then names it. The connection string is a secret, so it comes
from an environment variable, never from the file:

```yaml
store: { type: postgres, dsn_env: LEADSCORE_PG_DSN }
```

The `store` block reaches `Open` as a `leadscore.Config` (a map), with YAML's
types.

## What the engine needs from a store

Read [`reference.md`](reference.md), "Plug-ins" (`Backend`, `EventLog`) and
"Store tables". In short:

- **Tables are text.** Every value is a string; keep it exactly (a value
  starting with `=`, leading zeros, newlines).
- **Unknown columns are kept.** A newer version may add columns; a write
  never drops a column it does not name. The simplest way to honour this is to
  keep each row as one `jsonb` object.
- **`Commit` is all-or-nothing:** one transaction for every write in the
  call. It creates a missing table and column the first time a write names
  it, and an `OpAppend` of a key a keyed table already holds fails the whole
  commit.
- **The lease is a real compare-and-swap.** Two runs can never both hold it.
- **Event appends are ordered and durable.** A reader that has seen event
  `n` must never later find an event below `n` appear.

## Tables

One table holds every leadscore table's rows; another holds the receiver's
events; a third holds the lease.

```sql
CREATE TABLE leadscore_rows (
  tbl  text   NOT NULL,          -- the table name, e.g. 'Ranked', 'Export call-list'
  pk   text,                     -- the key columns' values joined; NULL on keyless tables (Overrides, Log)
  seq  bigserial,                -- keeps keyless tables in insertion order
  row  jsonb  NOT NULL,
  PRIMARY KEY (seq)
);
CREATE UNIQUE INDEX leadscore_rows_key ON leadscore_rows (tbl, pk) WHERE pk IS NOT NULL;
CREATE INDEX leadscore_rows_tbl ON leadscore_rows (tbl, seq);

CREATE TABLE leadscore_events (
  seq         bigserial PRIMARY KEY,
  received_at timestamptz NOT NULL,
  kind        text NOT NULL,
  body        bytea NOT NULL
);

CREATE TABLE leadscore_lease (
  id         int PRIMARY KEY CHECK (id = 1),
  owner      text NOT NULL,
  expires_at timestamptz NOT NULL
);
```

The key columns of each keyed table are in the reference's "Store tables" (`People` by `lead_id`,
`Pushes` by `lead_id, lane_id, step`, `Health` by `kind, key`, an
`Export <lane id>` table by `lead_id`, and so on). Keep them in a map in your
store; `OpUpsert` and `OpDelete` writes also carry their `Key`.

## Commit

```go
func (s *Store) Commit(ctx context.Context, writes []leadscore.TableWrite) error {
	for _, w := range writes {
		if (w.Op == leadscore.OpUpsert || w.Op == leadscore.OpDelete) && len(w.Key) == 0 {
			return fmt.Errorf("%s: %v needs a Key", w.Table, w.Op) // before touching anything
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, w := range writes {
		if err := s.apply(ctx, tx, w); err != nil {
			return fmt.Errorf("%s: %w", w.Table, err) // the deferred Rollback undoes every write
		}
	}
	return tx.Commit()
}
```

Each op in `apply`, with `pk` built from the table's key columns:

- `OpReplace`: `DELETE FROM leadscore_rows WHERE tbl = $1`, then insert every
  row.
- `OpAppend`: `INSERT`; on a keyed table the unique index refuses a key that
  exists, which fails the commit as `Backend` requires.
- `OpUpsert`: merge, so columns the writer does not know survive:
  `INSERT ... ON CONFLICT (tbl, pk) WHERE pk IS NOT NULL DO UPDATE SET row = leadscore_rows.row || EXCLUDED.row`.
- `OpDelete`: on a keyed table by `pk`; on `Overrides` (keyed on all four
  columns) by containment: `DELETE ... WHERE tbl = $1 AND row @> $2::jsonb`.
- `OpTrim`: `DELETE ... WHERE tbl = $1 AND row->>$2 <> '' AND row->>$2 < $3`,
  with `Before` formatted as `2006-01-02T15:04:05.000Z`. Stored times are UTC
  in that fixed-width form, so text order is time order.

`ReadTable` is `SELECT row FROM leadscore_rows WHERE tbl = $1 ORDER BY seq`;
a table never written returns no rows and no error.

`ErrTooLarge` exists for stores with a request cap (Sheets). Postgres has
none worth hitting at leadscore's sizes, so never return it.

## The lease

```go
func (s *Store) Lease(ctx context.Context, owner string, ttl time.Duration) (leadscore.RunLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Create the row once, then lock it: concurrent callers queue here.
	if _, err := tx.ExecContext(ctx, `INSERT INTO leadscore_lease VALUES (1, '', 'epoch') ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	var cur string
	var exp time.Time
	if err := tx.QueryRowContext(ctx, `SELECT owner, expires_at FROM leadscore_lease WHERE id = 1 FOR UPDATE`).Scan(&cur, &exp); err != nil {
		return nil, err
	}
	now := time.Now()
	if cur != "" && cur != owner && exp.After(now) {
		return nil, leadscore.ErrLeaseHeld
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leadscore_lease SET owner = $1, expires_at = $2 WHERE id = 1`, owner, now.Add(ttl)); err != nil {
		return nil, err
	}
	return &lease{s: s, owner: owner}, tx.Commit()
}
```

The returned lease's `Check` reads the row and returns `ErrLeaseLost` when
another owner holds it or it expired; `Release` clears it only when this
owner still holds it (`UPDATE ... WHERE id = 1 AND owner = $1`, and
`ErrLeaseLost` when no row changed). Also implement
`LeaseInfo(ctx) (owner string, expires time.Time, err error)`
(`leadscore.LeaseInspector`) so `leadscore doctor` can show a held lease.

## Events: appends serialized with `pg_advisory_xact_lock`

The receiver appends events while a run reads them, and a run reads from a
saved cursor (the last `seq` it saw). A `bigserial` alone is not enough: two
appends in flight take `seq` 7 and 8, the one holding 8 commits first, a run
reads up to 8 and saves that cursor, and event 7, committed a moment later,
is never read. So every append takes one transaction-scoped advisory lock
first: appends commit one at a time, in `seq` order.

```go
const appendLock = 0x6c656164 // any fixed number, the same in every process

func (s *Store) AppendEvents(ctx context.Context, events []leadscore.RawEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Held until this transaction ends: the next append waits for our commit.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, appendLock); err != nil {
		return err
	}
	for _, e := range events {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO leadscore_events (received_at, kind, body) VALUES ($1, $2, $3)`,
			e.ReceivedAt, e.Kind, e.Body); err != nil {
			return err
		}
	}
	return tx.Commit() // durable once it returns (synchronous_commit on, the default)
}
```

Retry Postgres' "slow down" answers (serialization failures, too many
connections) until `ctx` is done, as `EventLog` requires; never answer the
receiver before the commit returned.

- `ReadEvents(ctx, cursor)`: `SELECT ... WHERE seq > $1 ORDER BY seq LIMIT 5000`,
  setting each event's `Seq` to its `seq` as text and returning the last one
  as the next cursor (an empty cursor reads from the start). Return `ErrEventsShrank`
  when the cursor is beyond the highest `seq` ever issued
  (`SELECT last_value FROM leadscore_events_seq_seq`): the database was
  restored from an older copy, and the run must not skip ahead.
- `DeleteProcessed(ctx, committed, olderThan)`:
  `DELETE FROM leadscore_events WHERE seq <= $1 AND received_at < $2`, and
  return `committed` unchanged (one partition, never dropped).

## Export lists

The engine writes the `Export <lane id>` tables through `Commit` on every
store, so your lists are rows of `leadscore_rows`. The CSV files in
`export.dir` are written only for the built-in SQLite store; a plug-in store
gets none. Read a list from the database instead, and **filter on
`do_not_contact` before every send**:

```sql
SELECT row->>'email' AS email, row->>'full_name' AS full_name, row->>'score' AS score
FROM leadscore_rows
WHERE tbl = 'Export call-list' AND row->>'do_not_contact' = 'no'
ORDER BY (row->>'score')::numeric DESC;
```

## Prove it with the conformance suite

```go
package pgstore_test

func TestConformance(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN not set")
	}
	storetest.Run(t, func(t *testing.T) (leadscore.Backend, leadscore.EventLog) {
		s := openFreshSchema(t, dsn) // a new, empty schema per call
		return s, s
	})
}
```

`storetest.Run` checks every table's round trip, all-or-nothing commits for
every op, unknown columns kept, the lease races, event ordering with a slow
append and a read interleaved, `DeleteProcessed` and `ErrEventsShrank`. When
it passes, run `leadscore doctor` against the new store, then a
`leadscore run --dry-run`.

`leadscore doctor`'s `store` check covers the schema version on any store;
its SQLite and Sheets cases do not apply here.
