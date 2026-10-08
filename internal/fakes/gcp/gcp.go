// Package gcp is an in-memory fake of the Google Cloud calls internal/hosting
// makes, served under one base URL with each API under its own prefix
// (/secretmanager, /run, /cloudscheduler, /artifactregistry), as
// hosting.Connect expects with `base_url`. Secret Manager behaves like the real
// one for what leadscore uses: secrets hold numbered versions, `latest` is the
// newest enabled one, a missing secret or one with no enabled version is 404,
// and checksums are checked. Cloud Run, Cloud Scheduler and Artifact Registry
// resources are JSON documents a test sets, answered as written.
//
// The request and answer shapes follow Google's public REST references, not
// recorded calls (none are recorded for the Cloud Run checks yet).
package gcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	mu        sync.Mutex
	secrets   map[string][]version // "project/secret" -> versions, oldest first
	resources map[string]string    // API path (without the API prefix) -> JSON
	denied    map[string]bool      // "project/secret" -> answer 403
	noWrites  map[string]bool      // "project/secret" -> answer 403 to addVersion only
	calls     []string             // "METHOD /api/path", in order
}

type version struct {
	data    []byte
	enabled bool
}

// New returns a fake with no secrets and no resources.
func New() *Server {
	return &Server{secrets: map[string][]version{}, resources: map[string]string{}, denied: map[string]bool{}, noWrites: map[string]bool{}}
}

// CreateSecret makes an empty secret (no versions), as `gcloud secrets create`.
func (s *Server) CreateSecret(project, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[project+"/"+secret]; !ok {
		s.secrets[project+"/"+secret] = []version{}
	}
}

// RemoveSecret deletes a secret and its versions.
func (s *Server) RemoveSecret(project, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, project+"/"+secret)
}

// AddVersion adds an enabled version and returns its number.
func (s *Server) AddVersion(project, secret string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := project + "/" + secret
	s.secrets[k] = append(s.secrets[k], version{data: append([]byte(nil), data...), enabled: true})
	return strconv.Itoa(len(s.secrets[k]))
}

// DisableVersion disables a version by number.
func (s *Server) DisableVersion(project, secret, n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, _ := strconv.Atoi(n)
	if vs := s.secrets[project+"/"+secret]; i >= 1 && i <= len(vs) {
		vs[i-1].enabled = false
	}
}

// Deny makes every call on a secret answer 403, as for an account without
// access to it.
func (s *Server) Deny(project, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied[project+"/"+secret] = true
}

// DenyWrites makes adding a version to a secret answer 403, as for an
// account that may read it but not add to it.
func (s *Server) DenyWrites(project, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noWrites[project+"/"+secret] = true
}

// Versions returns a secret's version values, oldest first.
func (s *Server) Versions(project, secret string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for _, v := range s.secrets[project+"/"+secret] {
		out = append(out, append([]byte(nil), v.data...))
	}
	return out
}

// SetResource sets the JSON answered for GET <path> (path under the API's
// prefix, for example /run/v2/projects/p/locations/r/services/leadscore-receiver).
// An empty doc removes it, so the path answers 404.
func (s *Server) SetResource(path, doc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc == "" {
		delete(s.resources, path)
		return
	}
	s.resources[path] = doc
}

// Calls returns every request served, as "METHOD path".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.EscapedPath()
	s.calls = append(s.calls, r.Method+" "+path)
	if strings.HasPrefix(path, "/secretmanager/v1/projects/") {
		s.secretManager(w, r, strings.TrimPrefix(path, "/secretmanager/v1/projects/"))
		return
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "the fake serves only reads here")
		return
	}
	doc, ok := s.resources[path]
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, doc)
}

// secretManager serves {project}/secrets/{secret} (get),
// {project}/secrets/{secret}/versions/latest:access and
// {project}/secrets/{secret}:addVersion.
func (s *Server) secretManager(w http.ResponseWriter, r *http.Request, rest string) {
	project, rest, _ := strings.Cut(rest, "/secrets/")
	var secret, action string
	switch {
	case !strings.Contains(rest, "/") && !strings.Contains(rest, ":") && r.Method == http.MethodGet:
		secret, action = rest, "get"
	case strings.HasSuffix(rest, "/versions/latest:access") && r.Method == http.MethodGet:
		secret, action = strings.TrimSuffix(rest, "/versions/latest:access"), "access"
	case strings.HasSuffix(rest, ":addVersion") && r.Method == http.MethodPost:
		secret, action = strings.TrimSuffix(rest, ":addVersion"), "add"
	default:
		apiError(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	k := project + "/" + secret
	if s.denied[k] || (action == "add" && s.noWrites[k]) {
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED")
		return
	}
	vs, ok := s.secrets[k]
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if action == "get" {
		writeJSON(w, map[string]string{"name": fmt.Sprintf("projects/%s/secrets/%s", project, secret)})
		return
	}
	name := func(n int) string { return fmt.Sprintf("projects/%s/secrets/%s/versions/%d", project, secret, n) }
	if action == "add" {
		var in struct {
			Payload struct {
				Data       string `json:"data"`
				DataCrc32c string `json:"dataCrc32c"`
			} `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		data, err := base64.StdEncoding.DecodeString(in.Payload.Data)
		if err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		if in.Payload.DataCrc32c != "" {
			if want, err := strconv.ParseUint(in.Payload.DataCrc32c, 10, 32); err != nil || uint32(want) != crc32.Checksum(data, castagnoli) {
				apiError(w, http.StatusBadRequest, "DATA_LOSS")
				return
			}
		}
		if len(data) > 64*1024 {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT")
			return
		}
		s.secrets[k] = append(vs, version{data: data, enabled: true})
		writeJSON(w, map[string]string{"name": name(len(s.secrets[k])), "state": "ENABLED"})
		return
	}
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].enabled {
			writeJSON(w, map[string]any{"name": name(i + 1), "payload": map[string]string{
				"data":       base64.StdEncoding.EncodeToString(vs[i].data),
				"dataCrc32c": strconv.FormatUint(uint64(crc32.Checksum(vs[i].data, castagnoli)), 10),
			}})
			return
		}
	}
	apiError(w, http.StatusNotFound, "NOT_FOUND")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// apiError writes Google's error shape: {"error":{"code","message","status"}}.
func apiError(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": status, "status": status}})
}
