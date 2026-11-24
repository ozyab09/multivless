package subscriptions

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/multivless/multivless/internal/storage"
)

const (
	uriOne = "vless://11111111-2222-4333-8444-555555555551@198.51.100.1:443?security=tls&sni=one.example&type=tcp#One"
	uriTwo = "vless://11111111-2222-4333-8444-555555555552@198.51.100.2:443?security=tls&sni=two.example&type=tcp#Two"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestUpdateParsesAppliesAndPreservesLastValidSet(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "subs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var (
		mu      sync.Mutex
		body    = uriOne + "\n" + uriTwo
		serve   = true
		applied int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !serve {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		etag := `"v1"`
		if body == uriOne {
			etag = `"v2"`
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, base64.StdEncoding.EncodeToString([]byte(body)))
	}))
	defer server.Close()

	sub := storage.Subscription{ID: "sub_1", Name: "test", URL: server.URL, IntervalSeconds: 60, Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}

	manager := New(store, Options{AllowPrivateHosts: true, Timeout: 5 * time.Second},
		func(context.Context) error { applied++; return nil }, testLogger())

	res := manager.Update(context.Background(), sub)
	if res.Error != "" {
		t.Fatalf("update failed: %s", res.Error)
	}
	if res.ServerCount != 2 || res.Added != 2 {
		t.Fatalf("result = %+v", res)
	}
	if applied != 1 {
		t.Fatalf("apply called %d times", applied)
	}

	servers, _ := store.ListServers()
	if len(servers) != 2 {
		t.Fatalf("servers = %d", len(servers))
	}

	// Second run is answered with 304: nothing changes.
	sub, _ = store.GetSubscription("sub_1")
	res = manager.Update(context.Background(), sub)
	if !res.NotModified {
		t.Fatalf("expected NotModified, got %+v", res)
	}
	if applied != 1 {
		t.Fatalf("apply must not run on 304")
	}

	// A smaller subscription removes the missing server.
	mu.Lock()
	body = uriOne
	mu.Unlock()
	sub, _ = store.GetSubscription("sub_1")
	res = manager.Update(context.Background(), sub)
	if res.Removed != 1 || res.ServerCount != 1 {
		t.Fatalf("removal not detected: %+v", res)
	}
	servers, _ = store.ListServers()
	if len(servers) != 1 {
		t.Fatalf("servers after removal = %d", len(servers))
	}

	// A broken download keeps the last valid server set.
	mu.Lock()
	serve = false
	mu.Unlock()
	sub, _ = store.GetSubscription("sub_1")
	res = manager.Update(context.Background(), sub)
	if res.Error == "" {
		t.Fatal("expected an error for a 503 response")
	}
	servers, _ = store.ListServers()
	if len(servers) != 1 {
		t.Fatalf("last valid set was not preserved: %d", len(servers))
	}
	stored, _ := store.GetSubscription("sub_1")
	if stored.LastError == "" {
		t.Fatal("subscription error not recorded")
	}
}

func TestSSRFGuardRejectsLoopbackByDefault(t *testing.T) {
	manager := New(nil, Options{Timeout: 5 * time.Second}, nil, testLogger())
	if err := manager.ValidateURL(context.Background(), "http://127.0.0.1/sub"); err == nil {
		t.Fatal("loopback subscription URL must be rejected without allow_private_hosts")
	}
	if err := manager.ValidateURL(context.Background(), "ftp://example.com/sub"); err == nil {
		t.Fatal("non-http scheme must be rejected")
	}
	if err := manager.ValidateURL(context.Background(), "http://user:pass@example.com/sub"); err == nil {
		t.Fatal("embedded credentials must be rejected")
	}
}
