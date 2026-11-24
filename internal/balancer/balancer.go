// Package balancer selects an outbound for each new connection.
package balancer

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sync"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/servers"
)

// Balancer implements the balancing strategies from the specification. The
// selected outbound is pinned to the connection by the caller; the balancer is
// only consulted for new connections.
type Balancer struct {
	strategy string
	fallback string
	weights  config.Weights

	mu sync.Mutex
	rr uint64
}

// New creates a balancer.
func New(cfg config.Balancer) *Balancer {
	return &Balancer{
		strategy: cfg.Strategy,
		fallback: cfg.FallbackStrategy,
		weights:  normalized(cfg.Weights),
	}
}

// Strategy returns the configured strategy name.
func (b *Balancer) Strategy() string { return b.strategy }

// SetStrategy changes the strategy at runtime.
func (b *Balancer) SetStrategy(name string) error {
	switch name {
	case config.StrategyAdaptive, config.StrategyRoundRobin, config.StrategyRandom,
		config.StrategyLeastLatency, config.StrategyLeastConnections, config.StrategyWeighted:
		b.mu.Lock()
		b.strategy = name
		b.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("unknown strategy %q", name)
	}
}

// Pick chooses one candidate, skipping IDs present in exclude. It returns an
// error when no candidate is eligible, which callers translate into a
// fail-closed SOCKS5 rejection.
func (b *Balancer) Pick(candidates []*servers.Server, exclude map[string]bool) (*servers.Server, error) {
	pool := make([]*servers.Server, 0, len(candidates))
	for _, srv := range candidates {
		if exclude != nil && exclude[srv.ID()] {
			continue
		}
		pool = append(pool, srv)
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no selectable outbound")
	}
	if len(pool) == 1 {
		return pool[0], nil
	}

	b.mu.Lock()
	strategy := b.strategy
	b.mu.Unlock()

	switch strategy {
	case config.StrategyRoundRobin:
		return b.roundRobin(pool), nil
	case config.StrategyRandom:
		return pool[rand.IntN(len(pool))], nil
	case config.StrategyLeastLatency:
		return leastLatency(pool), nil
	case config.StrategyLeastConnections:
		return leastConnections(pool), nil
	case config.StrategyWeighted:
		return weighted(pool), nil
	case config.StrategyAdaptive:
		return b.adaptive(pool), nil
	default:
		return b.roundRobin(pool), nil
	}
}

func (b *Balancer) roundRobin(pool []*servers.Server) *servers.Server {
	b.mu.Lock()
	idx := b.rr
	b.rr++
	b.mu.Unlock()
	return pool[idx%uint64(len(pool))]
}

func leastLatency(pool []*servers.Server) *servers.Server {
	best := pool[0]
	bestLat := effectiveLatency(best)
	for _, srv := range pool[1:] {
		if lat := effectiveLatency(srv); lat < bestLat {
			best, bestLat = srv, lat
		}
	}
	return best
}

func leastConnections(pool []*servers.Server) *servers.Server {
	best := pool[0]
	bestActive := float64(best.Snapshot().Active) / float64(best.Weight())
	for _, srv := range pool[1:] {
		active := float64(srv.Snapshot().Active) / float64(srv.Weight())
		if active < bestActive {
			best, bestActive = srv, active
		}
	}
	return best
}

func weighted(pool []*servers.Server) *servers.Server {
	total := 0
	for _, srv := range pool {
		total += max(srv.Weight(), 1)
	}
	if total == 0 {
		return pool[0]
	}
	pick := rand.IntN(total)
	for _, srv := range pool {
		pick -= max(srv.Weight(), 1)
		if pick < 0 {
			return srv
		}
	}
	return pool[len(pool)-1]
}

// adaptive implements the score from the specification:
//
//	Latency*0.30 + Connections*0.25 + Throughput*0.35 + Reliability*0.10
//
// Every component is normalised to 0-100 within the current pool and a lower
// score is better.
func (b *Balancer) adaptive(pool []*servers.Server) *servers.Server {
	lats := make([]float64, len(pool))
	conns := make([]float64, len(pool))
	var minLat, maxLat, maxConn float64
	minLat = math.Inf(1)
	for i, srv := range pool {
		lats[i] = effectiveLatency(srv)
		conns[i] = float64(srv.Snapshot().Active)
		minLat = math.Min(minLat, lats[i])
		maxLat = math.Max(maxLat, lats[i])
		maxConn = math.Max(maxConn, conns[i])
	}

	var best *servers.Server
	bestScore := math.Inf(1)
	for i, srv := range pool {
		latNorm := 0.0
		if maxLat > minLat {
			latNorm = (lats[i] - minLat) / (maxLat - minLat) * 100
		}
		connNorm := 0.0
		if maxConn > 0 {
			connNorm = conns[i] / maxConn * 100
		}
		throughputNorm := 100 - clamp(srv.ThroughputScore(), 0, 100)
		reliabilityNorm := 100 - clamp(srv.Reliability(), 0, 100)

		score := b.weights.Latency*latNorm +
			b.weights.Connections*connNorm +
			b.weights.Throughput*throughputNorm +
			b.weights.Reliability*reliabilityNorm

		// A configured weight acts as a tie-breaker: heavier servers win.
		score -= float64(min(srv.Weight(), 1000)) / 1e6

		if score < bestScore {
			best, bestScore = srv, score
		}
	}
	if best == nil {
		return pool[0]
	}
	return best
}

// effectiveLatency substitutes a neutral value for servers without probe data
// so that they are neither starved nor blindly preferred (§5.4).
func effectiveLatency(srv *servers.Server) float64 {
	if lat := srv.LatencyMS(); lat > 0 {
		return lat
	}
	return 100
}

func normalized(w config.Weights) config.Weights {
	sum := w.Sum()
	if sum <= 0 {
		return config.Weights{Latency: 0.30, Connections: 0.25, Throughput: 0.35, Reliability: 0.10}
	}
	return config.Weights{
		Latency:     w.Latency / sum,
		Connections: w.Connections / sum,
		Throughput:  w.Throughput / sum,
		Reliability: w.Reliability / sum,
	}
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}
