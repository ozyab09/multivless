package balancer

import (
	"testing"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/servers"
	"github.com/multivless/multivless/internal/subscriptions/parser"
)

func fixture(ids ...string) []*servers.Server {
	out := make([]*servers.Server, 0, len(ids))
	for _, id := range ids {
		out = append(out, servers.New(&parser.Server{ID: id, Name: id, Host: "127.0.0.1", Port: 443}))
	}
	return out
}

func online(t *testing.T, srv *servers.Server) {
	t.Helper()
	for i := 0; i < 2; i++ {
		srv.MarkSuccess(10e6, 2, 0)
	}
}

func TestRoundRobinCyclesThroughPool(t *testing.T) {
	b := New(config.Balancer{Strategy: config.StrategyRoundRobin})
	pool := fixture("a", "b", "c")

	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		srv, err := b.Pick(pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[srv.ID()]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != 10 {
			t.Errorf("%s picked %d times, want 10", id, counts[id])
		}
	}
}

func TestPickHonoursExcludeAndFailsClosed(t *testing.T) {
	b := New(config.Balancer{Strategy: config.StrategyRoundRobin})
	pool := fixture("a", "b")

	exclude := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		srv, err := b.Pick(pool, exclude)
		if err != nil {
			t.Fatal(err)
		}
		if srv.ID() != "b" {
			t.Fatalf("picked %s with a excluded", srv.ID())
		}
	}

	exclude["b"] = true
	if _, err := b.Pick(pool, exclude); err == nil {
		t.Fatal("expected error when every candidate is excluded")
	}
	if _, err := b.Pick(nil, nil); err == nil {
		t.Fatal("expected error for empty pool")
	}
}

func TestLeastConnections(t *testing.T) {
	b := New(config.Balancer{Strategy: config.StrategyLeastConnections})
	pool := fixture("a", "b", "c")
	pool[0].AddActive(3)
	pool[1].AddActive(1)
	pool[2].AddActive(7)

	for i := 0; i < 5; i++ {
		srv, err := b.Pick(pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		if srv.ID() != "b" {
			t.Fatalf("picked %s, want least loaded b", srv.ID())
		}
	}
}

func TestAdaptivePrefersFasterServer(t *testing.T) {
	b := New(config.Balancer{Strategy: config.StrategyAdaptive})
	pool := fixture("slow", "fast")
	online(t, pool[0])
	online(t, pool[1])

	// The first success seeds the EMA; drive it to a clear difference.
	for i := 0; i < 10; i++ {
		pool[0].MarkSuccess(200e6, 2, 0) // 200ms
		pool[1].MarkSuccess(20e6, 2, 0)  // 20ms
	}

	fast := 0
	for i := 0; i < 100; i++ {
		srv, err := b.Pick(pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		if srv.ID() == "fast" {
			fast++
		}
	}
	if fast < 90 {
		t.Errorf("adaptive picked the faster server %d/100 times", fast)
	}
}
