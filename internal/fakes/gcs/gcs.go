// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package gcs is an in-memory fake of the Cloud Storage JSON API calls the
// Sheets store makes for its lease file: bucket get,
// object get (metadata or media), multipart and media upload, delete, and
// testIamPermissions on a bucket (the doctor's lease check). It enforces the
// generation preconditions (ifGenerationMatch, 0 meaning "does not exist yet")
// exactly, answering 412 when one does not hold, because that compare-and-swap
// is what makes the lease safe.
package gcs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	mu      sync.Mutex
	buckets map[string]map[string]*object
	gen     int64
	denied  map[string]map[string]bool // bucket -> permissions the caller lacks
}

type object struct {
	data        []byte
	gen         int64
	contentType string
}

// New returns a fake with no buckets.
func New() *Server { return &Server{buckets: map[string]map[string]*object{}} }

// CreateBucket adds an empty bucket.
func (s *Server) CreateBucket(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[name] == nil {
		s.buckets[name] = map[string]*object{}
	}
}

// DenyPermission makes testIamPermissions on bucket leave out perm (for
// example "storage.objects.create"), as for an account without that role.
func (s *Server) DenyPermission(bucket, perm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied == nil {
		s.denied = map[string]map[string]bool{}
	}
	if s.denied[bucket] == nil {
		s.denied[bucket] = map[string]bool{}
	}
	s.denied[bucket][perm] = true
}

// Object returns an object's content and generation; ok is false when missing.
func (s *Server) Object(bucket, name string) (data []byte, gen int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.buckets[bucket][name]
	if o == nil {
		return nil, 0, false
	}
	return append([]byte(nil), o.data...), o.gen, true
}

// SetObject writes an object directly, with a new generation.
func (s *Server) SetObject(bucket, name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[bucket] == nil {
		s.buckets[bucket] = map[string]*object{}
	}
	s.gen++
	s.buckets[bucket][name] = &object{data: append([]byte(nil), data...), gen: s.gen, contentType: "application/json"}
}

// Route serves Cloud Storage paths with gcs and every other path with other,
// so one test server can stand in for Sheets, Drive and Cloud Storage at one
// base URL.
func Route(gcs, other http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/storage/v1/") || strings.HasPrefix(p, "/upload/storage/v1/") {
			gcs.ServeHTTP(w, r)
			return
		}
		other.ServeHTTP(w, r)
	})
}

// ServeHTTP serves the JSON API paths.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	if rest, ok := strings.CutPrefix(path, "/upload/storage/v1/b/"); ok {
		bucket, tail, _ := strings.Cut(rest, "/")
		if tail != "o" || r.Method != http.MethodPost {
			apiError(w, http.StatusNotFound, "Not Found")
			return
		}
		bucket, _ = url.PathUnescape(bucket)
		s.upload(w, r, bucket, q)
		return
	}
	rest, ok := strings.CutPrefix(path, "/storage/v1/b/")
	if !ok {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	bucketEsc, tail, hasTail := strings.Cut(rest, "/")
	bucket, _ := url.PathUnescape(bucketEsc)
	objs := s.buckets[bucket]
	if objs == nil {
		apiError(w, http.StatusNotFound, "The specified bucket does not exist.")
		return
	}
	if !hasTail {
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, map[string]any{"kind": "storage#bucket", "name": bucket, "id": bucket})
		return
	}
	if tail == "iam/testPermissions" && r.Method == http.MethodGet {
		var held []string
		for _, p := range q["permissions"] {
			if !s.denied[bucket][p] {
				held = append(held, p)
			}
		}
		writeJSON(w, map[string]any{"kind": "storage#testIamPermissionsResponse", "permissions": held})
		return
	}
	nameEsc, ok := strings.CutPrefix(tail, "o/")
	if !ok {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	name, _ := url.PathUnescape(nameEsc)
	o := objs[name]
	if o == nil {
		apiError(w, http.StatusNotFound, "No such object: "+bucket+"/"+name)
		return
	}
	if !matches(q, o) {
		apiError(w, http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if q.Get("alt") == "media" {
			w.Header().Set("Content-Type", o.contentType)
			w.Header().Set("X-Goog-Generation", strconv.FormatInt(o.gen, 10))
			_, _ = w.Write(o.data)
			return
		}
		writeJSON(w, meta(bucket, name, o))
	case http.MethodDelete:
		delete(objs, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// matches checks ifGenerationMatch against an existing object.
func matches(q url.Values, o *object) bool {
	v := q.Get("ifGenerationMatch")
	if v == "" {
		return true
	}
	g, err := strconv.ParseInt(v, 10, 64)
	return err == nil && o != nil && g == o.gen
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	objs := s.buckets[bucket]
	if objs == nil {
		apiError(w, http.StatusNotFound, "The specified bucket does not exist.")
		return
	}
	name := q.Get("name")
	var data []byte
	contentType := "application/octet-stream"
	switch q.Get("uploadType") {
	case "media":
		b, err := io.ReadAll(r.Body)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		data = b
		if ct := r.Header.Get("Content-Type"); ct != "" {
			contentType = ct
		}
	case "multipart":
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mt, "multipart/") {
			apiError(w, http.StatusBadRequest, "a multipart upload needs a multipart body")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			apiError(w, http.StatusBadRequest, "no metadata part")
			return
		}
		var md struct {
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
		}
		if err := json.NewDecoder(part).Decode(&md); err != nil {
			apiError(w, http.StatusBadRequest, "bad metadata: "+err.Error())
			return
		}
		if md.Name != "" {
			name = md.Name
		}
		part, err = mr.NextPart()
		if err != nil {
			apiError(w, http.StatusBadRequest, "no media part")
			return
		}
		if data, err = io.ReadAll(part); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		contentType = part.Header.Get("Content-Type")
		if md.ContentType != "" {
			contentType = md.ContentType
		}
	default:
		apiError(w, http.StatusBadRequest, "the fake supports uploadType media and multipart only")
		return
	}
	if name == "" {
		apiError(w, http.StatusBadRequest, "Required object name")
		return
	}
	cur := objs[name]
	if v := q.Get("ifGenerationMatch"); v != "" {
		g, err := strconv.ParseInt(v, 10, 64)
		if err != nil || (g == 0 && cur != nil) || (g != 0 && (cur == nil || cur.gen != g)) {
			apiError(w, http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold.")
			return
		}
	}
	s.gen++
	o := &object{data: bytes.Clone(data), gen: s.gen, contentType: contentType}
	objs[name] = o
	writeJSON(w, meta(bucket, name, o))
}

func meta(bucket, name string, o *object) map[string]any {
	return map[string]any{
		"kind": "storage#object", "bucket": bucket, "name": name,
		"generation": strconv.FormatInt(o.gen, 10), "metageneration": "1",
		"size": strconv.Itoa(len(o.data)), "contentType": o.contentType,
		"id": fmt.Sprintf("%s/%s/%d", bucket, name, o.gen),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": msg, "errors": []map[string]any{{"message": msg}},
	}})
}
