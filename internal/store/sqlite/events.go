package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// The events table is one partition. A cursor is the last sequence number
// read, in decimal; empty means "from the start". AUTOINCREMENT never hands
// out a number again, even after DeleteProcessed removes rows, and SQLite
// assigns numbers in commit order (one writer at a time), so an event not yet
// returned always sorts after a saved cursor.
const createEvents = `CREATE TABLE IF NOT EXISTS "events" (
	"seq" INTEGER PRIMARY KEY AUTOINCREMENT,
	"received_at" TEXT NOT NULL DEFAULT '',
	"kind" TEXT NOT NULL DEFAULT '',
	"body" BLOB
)`

// AppendEvents stores a batch in one transaction, in order, and returns once it
// is durable (synchronous=FULL). It retries "database is locked" until ctx is done.
func (s *Store) AppendEvents(ctx context.Context, events []api.RawEvent) error {
	if len(events) == 0 {
		return nil
	}
	for i, e := range events {
		if e.ReceivedAt.IsZero() {
			return fmt.Errorf("appending events: event %d has no received time", i)
		}
	}
	wait := 20 * time.Millisecond
	for {
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, createEvents); err != nil {
				return err
			}
			for _, e := range events {
				body := e.Body
				if body == nil {
					body = []byte{}
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO "events" ("received_at", "kind", "body") VALUES (?, ?, ?)`,
					model.FormatTime(e.ReceivedAt), e.Kind, body)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil || !isBusy(err) {
			if err != nil {
				return fmt.Errorf("appending events: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("appending events: %w (gave up: %w)", err, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// ReadEvents returns the events after cursor, oldest first, and the cursor of
// the last one (cursor itself when none). It returns ErrEventsShrank when the
// cursor is past the highest number this file ever assigned: the file was
// replaced or restored from an older copy.
func (s *Store) ReadEvents(ctx context.Context, cursor api.Cursor) ([]api.RawEvent, api.Cursor, error) {
	after, err := parseCursor(cursor)
	if err != nil {
		return nil, cursor, err
	}
	high, exists, err := s.highestSeq(ctx)
	if err != nil {
		return nil, cursor, err
	}
	if after > high {
		return nil, cursor, fmt.Errorf("%w: cursor %d, highest event %d", api.ErrEventsShrank, after, high)
	}
	if !exists {
		return nil, cursor, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT "seq", "received_at", "kind", "body" FROM "events" WHERE "seq" > ? ORDER BY "seq"`, after)
	if err != nil {
		return nil, cursor, fmt.Errorf("reading events: %w", err)
	}
	defer rows.Close()
	var out []api.RawEvent
	next := cursor
	for rows.Next() {
		var (
			seq        int64
			at, kind   string
			body       []byte
			receivedAt time.Time
		)
		if err := rows.Scan(&seq, &at, &kind, &body); err != nil {
			return nil, cursor, fmt.Errorf("reading events: %w", err)
		}
		if receivedAt, err = model.ParseTime(at); err != nil {
			return nil, cursor, fmt.Errorf("event %d received_at: %w", seq, err)
		}
		next = api.Cursor(strconv.FormatInt(seq, 10))
		out = append(out, api.RawEvent{Seq: next, Kind: kind, ReceivedAt: receivedAt, Body: body})
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, fmt.Errorf("reading events: %w", err)
	}
	return out, next, nil
}

// DeleteProcessed removes events at or below committed that were received
// before olderThan. There is one partition, so the cursor comes back unchanged.
func (s *Store) DeleteProcessed(ctx context.Context, committed api.Cursor, olderThan time.Time) (api.Cursor, error) {
	upTo, err := parseCursor(committed)
	if err != nil {
		return committed, err
	}
	if _, exists, err := s.highestSeq(ctx); err != nil || !exists {
		return committed, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM "events" WHERE "seq" <= ? AND "received_at" < ?`,
			upTo, model.FormatTime(olderThan))
		return err
	})
	if err != nil {
		return committed, fmt.Errorf("deleting processed events: %w", err)
	}
	return committed, nil
}

// highestSeq is the highest sequence number ever assigned (0 when none), and
// whether the events table exists.
func (s *Store) highestSeq(ctx context.Context) (int64, bool, error) {
	cols, err := columns(ctx, s.db, "events")
	if err != nil || len(cols) == 0 {
		return 0, false, err
	}
	var high int64
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(max("seq"), 0) FROM sqlite_sequence WHERE "name" = 'events'`).Scan(&high)
	if err != nil {
		return 0, true, fmt.Errorf("reading the event sequence: %w", err)
	}
	return high, true, nil
}

func parseCursor(c api.Cursor) (int64, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(string(c), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("event cursor %q is not a SQLite sequence number", c)
	}
	return n, nil
}
