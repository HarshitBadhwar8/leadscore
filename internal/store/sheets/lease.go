// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/storage/v1"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
)

// LeaseObject is the lease file in the lease bucket.
// Sheets has no compare-and-swap, so the lease lives in Cloud Storage: every
// write and delete is conditioned on the generation read just before
// (ifGenerationMatch, 0 when the file is absent), which Cloud Storage refuses
// if another run changed the file first. Two callers can never both hold it.
const LeaseObject = "leadscore-lease.json"

type leaseFile struct {
	Owner     string `json:"owner"`
	ExpiresAt string `json:"expires_at"`
}

// Lease takes the run lease, or returns ErrLeaseHeld while another owner's
// lease has not expired. An expired lease is taken over; the same owner
// taking it again extends it.
func (s *Store) Lease(ctx context.Context, owner string, ttl time.Duration) (api.RunLease, error) {
	if owner == "" {
		return nil, errors.New("a lease needs an owner")
	}
	if ttl <= 0 {
		return nil, errors.New("a lease needs a positive length")
	}
	cur, gen, err := s.readLease(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if cur.Owner != "" && cur.Owner != owner {
		exp, err := model.ParseTime(cur.ExpiresAt)
		if err == nil && exp.After(now) {
			return nil, api.ErrLeaseHeld
		}
	}
	data, err := json.Marshal(leaseFile{Owner: owner, ExpiresAt: model.FormatTime(now.Add(ttl))})
	if err != nil {
		return nil, err
	}
	_, err = s.svc.Storage.Objects.Insert(s.bucket, &storage.Object{Name: LeaseObject, ContentType: "application/json"}).
		Media(bytes.NewReader(data), googleapi.ContentType("application/json")).
		IfGenerationMatch(gen).Context(ctx).Do()
	if precondition(err) {
		return nil, api.ErrLeaseHeld // another caller wrote the file first
	}
	if err != nil {
		return nil, fmt.Errorf("taking the lease in gs://%s/%s: %w", s.bucket, LeaseObject, err)
	}
	return &lease{s: s, owner: owner}, nil
}

// LeaseInfo shows the lease without taking it; an empty owner means none.
func (s *Store) LeaseInfo(ctx context.Context) (string, time.Time, error) {
	cur, _, err := s.readLease(ctx)
	if err != nil || cur.Owner == "" {
		return "", time.Time{}, err
	}
	exp, err := model.ParseTime(cur.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the lease file's expires_at: %w", err)
	}
	return cur.Owner, exp, nil
}

// LeaseBucket is the lease's Cloud Storage bucket; empty when none is set.
func (s *Store) LeaseBucket() string { return s.bucket }

// leasePermissions are what taking, checking and releasing the lease need.
var leasePermissions = []string{"storage.objects.create", "storage.objects.delete", "storage.objects.get"}

// LeaseBucketAccess reports, without writing anything, whether the lease
// bucket exists and whether this account may write, read and delete the
// lease file in it (the doctor's `lease` check). It asks Cloud Storage's
// testIamPermissions, which needs no permission of its own, so an account
// holding only Storage Object Admin on the bucket can still ask.
func (s *Store) LeaseBucketAccess(ctx context.Context) (exists, writable bool, err error) {
	if s.bucket == "" {
		return false, false, errors.New("the Sheets store needs `store.lease_bucket` to take the run lease (setup/gcp.sh bucket creates it)")
	}
	resp, err := s.svc.Storage.Buckets.TestIamPermissions(s.bucket, leasePermissions).Context(ctx).Do()
	if notFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("asking Cloud Storage about gs://%s: %w", s.bucket, err)
	}
	for _, p := range leasePermissions {
		if !slices.Contains(resp.Permissions, p) {
			return true, false, nil
		}
	}
	return true, true, nil
}

// readLease reads the lease file and its generation; a missing file is an
// empty lease at generation 0.
func (s *Store) readLease(ctx context.Context) (leaseFile, int64, error) {
	if s.bucket == "" {
		return leaseFile{}, 0, errors.New("the Sheets store needs `store.lease_bucket` to take the run lease (setup/gcp.sh bucket creates it)")
	}
	resp, err := s.svc.Storage.Objects.Get(s.bucket, LeaseObject).Context(ctx).Download()
	if notFound(err) {
		return leaseFile{}, 0, nil
	}
	if err != nil {
		return leaseFile{}, 0, fmt.Errorf("reading the lease gs://%s/%s: %w", s.bucket, LeaseObject, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return leaseFile{}, 0, fmt.Errorf("reading the lease: %w", err)
	}
	gen, err := strconv.ParseInt(resp.Header.Get("X-Goog-Generation"), 10, 64)
	if err != nil || gen <= 0 {
		return leaseFile{}, 0, errors.New("reading the lease: Cloud Storage sent no object generation")
	}
	var lf leaseFile
	if err := json.Unmarshal(data, &lf); err != nil {
		// A damaged file is treated as expired, so it is taken over by
		// generation and not left blocking every run.
		return leaseFile{ExpiresAt: ""}, gen, nil
	}
	return lf, gen, nil
}

func notFound(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}

func precondition(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusPreconditionFailed
}

type lease struct {
	s     *Store
	owner string
}

// Check returns ErrLeaseLost if the lease expired or another owner took it.
func (l *lease) Check(ctx context.Context) error {
	cur, _, err := l.s.readLease(ctx)
	if err != nil {
		return err
	}
	exp, perr := model.ParseTime(cur.ExpiresAt)
	if cur.Owner != l.owner || perr != nil || !exp.After(l.s.now()) {
		return api.ErrLeaseLost
	}
	return nil
}

// Release deletes the lease file only if this owner still holds it, by the
// generation just read; otherwise it returns ErrLeaseLost and leaves the
// lease alone.
func (l *lease) Release(ctx context.Context) error {
	cur, gen, err := l.s.readLease(ctx)
	if err != nil {
		return err
	}
	if cur.Owner != l.owner {
		return api.ErrLeaseLost
	}
	err = l.s.svc.Storage.Objects.Delete(l.s.bucket, LeaseObject).IfGenerationMatch(gen).Context(ctx).Do()
	if precondition(err) || notFound(err) {
		return api.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("releasing the lease: %w", err)
	}
	return nil
}
