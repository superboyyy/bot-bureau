package api

import (
	"botbureau/backend/internal/netx"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenMiddleware(t *testing.T) {
	app, _ := newTestApp(t)
	srv := httptest.NewServer(netx.RequireToken("secret123", app.Handler()))
	defer srv.Close()

	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if get("/api/ping", "") != 200 {
		t.Fatal("/api/ping should need no auth")
	}
	if get("/api/state", "") != 401 {
		t.Fatal("no pairing code should 401")
	}
	if get("/api/state", "wrong") != 401 {
		t.Fatal("a wrong pairing code should 401")
	}
	if get("/api/state", "secret123") != 200 {
		t.Fatal("the right pairing code should 200")
	}

	// The pairing code is no longer accepted from the query: URLs land verbatim in proxy access logs,
	// and a long-lived credential sitting there is a leak. The message stream uses a short-lived ticket
	// instead (covered by ticket_test.go in netx).
	res, err := http.Get(srv.URL + "/api/state?token=secret123")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("a query token must be refused, got %d", res.StatusCode)
	}
}

func TestTokenPersistence(t *testing.T) {
	dir := t.TempDir()
	tok1, err := netx.LoadOrCreateToken(dir)
	if err != nil || len(tok1) != 16 {
		t.Fatalf("generating the pairing code failed: %q %v", tok1, err)
	}
	tok2, _ := netx.LoadOrCreateToken(dir)
	if tok1 != tok2 {
		t.Fatal("the pairing code should persist and be reused")
	}
	if info, _ := os.Stat(filepath.Join(dir, "token")); info.Mode().Perm() != 0o600 {
		t.Fatal("the token file should be mode 0600")
	}
}

func TestEngineLock(t *testing.T) {
	dir := t.TempDir()
	l1, err := netx.AcquireEngineLock(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Lock already held: a second engine should be rejected
	if _, err := netx.AcquireEngineLock(dir); err == nil || !strings.Contains(err.Error(), "Another engine is already using") {
		t.Fatalf("a second acquire should fail while held: %v", err)
	}

	// Can be acquired again after release
	l1.Release()
	l2, err := netx.AcquireEngineLock(dir)
	if err != nil {
		t.Fatalf("should acquire after release: %v", err)
	}

	// A stale lock (crash leftover) can be taken over
	l2.Release()
	lockPath := filepath.Join(dir, "engine.lock")
	os.WriteFile(lockPath, []byte("dead pid=1"), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lockPath, old, old)
	l3, err := netx.AcquireEngineLock(dir)
	if err != nil {
		t.Fatalf("a stale lock should be taken over: %v", err)
	}
	l3.Release()
}
