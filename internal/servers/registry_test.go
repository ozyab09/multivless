package servers

import (
	"errors"
	"testing"
	"time"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

func newServer(id string) *Server {
	return New(&parser.Server{ID: id, Name: id, Host: "127.0.0.1", Port: 443, Weight: 100})
}

func TestStatusTransitionsFollowThresholds(t *testing.T) {
	srv := newServer("a")
	if srv.Status() != StatusUnknown {
		t.Fatalf("initial status = %s", srv.Status())
	}
	if srv.Selectable() {
		t.Fatal("UNKNOWN server must not be selectable")
	}

	// One success is not enough when successesBeforeOnline is 2.
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	if srv.Selectable() {
		t.Fatal("server became selectable after one success")
	}
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	if !srv.Selectable() {
		t.Fatalf("server not selectable after two successes: %s", srv.Status())
	}

	// Three failures take it offline.
	srv.MarkFailure(errors.New("timeout"), 3)
	srv.MarkFailure(errors.New("timeout"), 3)
	if !srv.Selectable() {
		t.Fatal("server went offline before the failure threshold")
	}
	srv.MarkFailure(errors.New("timeout"), 3)
	if srv.Status() != StatusOffline {
		t.Fatalf("status = %s, want OFFLINE", srv.Status())
	}

	// Recovery needs two successes again.
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	if srv.Status() != StatusOnline {
		t.Fatalf("status = %s, want ONLINE after recovery", srv.Status())
	}
}

func TestDisabledServerIsNotSelectableAndRecovers(t *testing.T) {
	srv := newServer("a")
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	srv.MarkSuccess(10*time.Millisecond, 2, 0)

	srv.SetEnabled(false)
	if srv.Status() != StatusDisabled {
		t.Fatalf("status = %s, want DISABLED", srv.Status())
	}
	if srv.Selectable() {
		t.Fatal("disabled server must not be selectable")
	}

	srv.SetEnabled(true)
	if srv.Selectable() {
		t.Fatal("re-enabled server must be probed before serving traffic")
	}
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	srv.MarkSuccess(10*time.Millisecond, 2, 0)
	if !srv.Selectable() {
		t.Fatalf("re-enabled server did not come back: %s", srv.Status())
	}
}

func TestDegradedStatusOnlineIsSelectable(t *testing.T) {
	srv := newServer("a")
	srv.MarkSuccess(900*time.Millisecond, 2, 500)
	srv.MarkSuccess(900*time.Millisecond, 2, 500)
	if srv.Status() != StatusDegraded {
		t.Fatalf("status = %s, want DEGRADED", srv.Status())
	}
	if !srv.Selectable() {
		t.Fatal("DEGRADED servers stay in the balancing pool")
	}
}

func TestRegistryDeduplicates(t *testing.T) {
	info := &parser.Server{ID: "srv_1", Name: "a", Host: "h", Port: 1, Weight: 100}
	same := &parser.Server{ID: "srv_1", Name: "a-renamed", Host: "h", Port: 1, Weight: 100}
	r := NewRegistry([]*parser.Server{info, same})
	if got := len(r.List()); got != 1 {
		t.Fatalf("servers = %d, want 1", got)
	}
	if got := r.Counts().Total; got != 1 {
		t.Fatalf("counts.total = %d", got)
	}
}

func TestMarkConfigErrorExcludesServer(t *testing.T) {
	r := NewRegistry([]*parser.Server{{ID: "srv_1", Name: "a", Host: "h", Port: 1, Weight: 100}})
	srv, _ := r.Get("srv_1")
	srv.MarkConfigError("unsupported")
	if srv.Selectable() {
		t.Fatal("server with config error must not be selectable")
	}
	if srv.Status() != StatusError {
		t.Fatalf("status = %s, want ERROR", srv.Status())
	}
}
