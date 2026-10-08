// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// `leadscore healthz` calls the local /healthz on receiver.port and exits 0
// only on 200 (the compose health check).
func TestHealthzCallsTheLocalEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("verdict\n"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	cfg := filepath.Join(dir, "leadscore.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\nstore: { type: sqlite }\nreceiver: { port: "+strconv.Itoa(port)+" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"--config", cfg, "healthz"}, &out, &errb); code != exitOK || !strings.Contains(out.String(), "verdict") {
		t.Errorf("healthy: exit %d, out %q, err %q", code, out.String(), errb.String())
	}
	status.Store(http.StatusServiceUnavailable)
	out.Reset()
	errb.Reset()
	if code := Main([]string{"--config", cfg, "healthz"}, &out, &errb); code != exitFail || !strings.Contains(errb.String(), "503") {
		t.Errorf("unhealthy: exit %d, err %q, want 1 and the status", code, errb.String())
	}
	_ = srv.Close()
	if code := Main([]string{"--config", cfg, "healthz"}, &out, &errb); code != exitFail {
		t.Errorf("nothing listening: exit %d, want 1", code)
	}
}
