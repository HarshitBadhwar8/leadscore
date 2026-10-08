// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// The lease is two State rows. Each read-check-write runs
// in one immediate transaction, which holds SQLite's write lock: that is the
// compare-and-swap, so two callers can never both hold the lease.
const (
	leaseOwnerKey   = "lease_owner"
	leaseExpiresKey = "lease_expires_at"
)

// Lease takes the run lease, or returns ErrLeaseHeld while another owner's
// lease has not expired. An expired lease is taken over; the same owner taking
// it again extends it.
func (s *Store) Lease(ctx context.Context, owner string, ttl time.Duration) (api.RunLease, error) {
	if owner == "" {
		return nil, errors.New("a lease needs an owner")
	}
	if ttl <= 0 {
		return nil, errors.New("a lease needs a positive length")
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, expires, err := readLease(ctx, tx)
		if err != nil {
			return err
		}
		now := s.now()
		if cur != "" && cur != owner && expires.After(now) {
			return api.ErrLeaseHeld
		}
		return writeLease(ctx, tx, owner, now.Add(ttl))
	})
	if err != nil {
		return nil, err
	}
	return &lease{s: s, owner: owner}, nil
}

// LeaseInfo shows the lease without taking it; an empty owner means none.
func (s *Store) LeaseInfo(ctx context.Context) (string, time.Time, error) {
	return readLease(ctx, s.db)
}

type lease struct {
	s     *Store
	owner string
}

// Check returns ErrLeaseLost if the lease expired or another owner took it.
func (l *lease) Check(ctx context.Context) error {
	cur, expires, err := readLease(ctx, l.s.db)
	if err != nil {
		return err
	}
	if cur != l.owner || !expires.After(l.s.now()) {
		return api.ErrLeaseLost
	}
	return nil
}

// Release gives the lease up only if this owner still holds it; otherwise it
// returns ErrLeaseLost and leaves the lease alone.
func (l *lease) Release(ctx context.Context) error {
	return l.s.inTx(ctx, func(tx *sql.Tx) error {
		cur, _, err := readLease(ctx, tx)
		if err != nil {
			return err
		}
		if cur != l.owner {
			return api.ErrLeaseLost
		}
		return writeLease(ctx, tx, "", time.Time{})
	})
}

type rowQuerier interface {
	querier
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readLease(ctx context.Context, q rowQuerier) (owner string, expires time.Time, err error) {
	cols, err := columns(ctx, q, TableName(model.TableState))
	if err != nil || len(cols) == 0 {
		return "", time.Time{}, err
	}
	get := func(key string) (string, error) {
		var v string
		err := q.QueryRowContext(ctx, `SELECT "value" FROM "state" WHERE "key" = ?`, key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	if owner, err = get(leaseOwnerKey); err != nil {
		return "", time.Time{}, fmt.Errorf("reading the lease: %w", err)
	}
	at, err := get(leaseExpiresKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading the lease: %w", err)
	}
	if expires, err = model.ParseTime(at); err != nil {
		return "", time.Time{}, fmt.Errorf("State.%s: %w", leaseExpiresKey, err)
	}
	return owner, expires, nil
}

func writeLease(ctx context.Context, tx *sql.Tx, owner string, expires time.Time) error {
	return apply(ctx, tx, api.TableWrite{
		Table: model.TableState, Op: api.OpUpsert, Key: []string{"key"},
		Rows: []api.Row{
			{"key": leaseOwnerKey, "value": owner},
			{"key": leaseExpiresKey, "value": model.FormatTime(expires)},
		},
	})
}
