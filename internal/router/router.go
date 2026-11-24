// Package router picks a server for each new connection and opens the tunnel.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multivless/multivless/internal/balancer"
	"github.com/multivless/multivless/internal/connections"
	"github.com/multivless/multivless/internal/engine/singbox"
	"github.com/multivless/multivless/internal/eventlog"
	"github.com/multivless/multivless/internal/geoip"
	"github.com/multivless/multivless/internal/servers"
	"github.com/multivless/multivless/internal/socks5"
)

// ErrNoOutbound is returned when the pool is empty. The SOCKS5 gateway turns it
// into a rejection; there is deliberately no direct fallback.
var ErrNoOutbound = errors.New("no online outbound servers")

// Options configures the router.
type Options struct {
	Balancer    *balancer.Balancer
	Registry    *servers.Registry
	Engine      *singbox.Manager
	Tracker     *connections.Tracker
	MaxAttempts int
	// OnUserBytes is called once per connection with its byte counters.
	OnUserBytes func(userID string, in, out uint64)
	// Events receives connection start/end and DNS attribution entries.
	Events *eventlog.Log
	// Geo attributes resolved target addresses to countries; may be nil.
	Geo    *geoip.DB
	Logger *slog.Logger
}

// Router maps client connections onto VLESS outbounds.
type Router struct {
	balancer    *balancer.Balancer
	registry    *servers.Registry
	engine      *singbox.Manager
	tracker     *connections.Tracker
	maxAttempts int
	onUserBytes func(userID string, in, out uint64)
	events      *eventlog.Log
	geo         *geoip.DB
	logger      *slog.Logger

	nextLane atomic.Uint64

	dnsMu    sync.Mutex
	dnsCache map[string]dnsEntry
}

const dnsCacheTTL = 10 * time.Minute

type dnsEntry struct {
	at time.Time
}

// New creates a router.
func New(opts Options) *Router {
	maxAttempts := opts.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Router{
		balancer:    opts.Balancer,
		registry:    opts.Registry,
		engine:      opts.Engine,
		tracker:     opts.Tracker,
		maxAttempts: maxAttempts,
		onUserBytes: opts.OnUserBytes,
		events:      opts.Events,
		geo:         opts.Geo,
		logger:      opts.Logger,
		dnsCache:    make(map[string]dnsEntry),
	}
}

// Dial establishes targetAddr through a selected VLESS outbound. The chosen
// server is pinned to the returned connection for its whole lifetime.
func (r *Router) Dial(ctx context.Context, principal socks5.Principal, targetAddr string) (net.Conn, error) {
	candidates := r.registry.Selectable()
	if len(candidates) == 0 {
		return nil, ErrNoOutbound
	}

	attempts := r.maxAttempts
	if attempts > len(candidates) {
		attempts = len(candidates)
	}

	exclude := make(map[string]bool, attempts)
	var lastErr error

	for i := 0; i < attempts; i++ {
		srv, err := r.balancer.Pick(candidates, exclude)
		if err != nil {
			break
		}
		exclude[srv.ID()] = true

		seq := int(r.nextLane.Add(1) - 1)
		laneCount := r.engine.DataLaneCount()
		laneOffset := 0
		if principal.BypassTorrent && r.engine.TorrentLaneCount() > 0 {
			laneCount = r.engine.TorrentLaneCount()
			laneOffset = r.engine.TorrentLaneOffset()
		}
		if laneCount <= 0 {
			return nil, ErrNoOutbound
		}
		lane := laneOffset + seq%laneCount
		srv.AddActive(1)

		conn, err := r.engine.DialThrough(ctx, lane, srv.ID(), targetAddr)
		if err != nil {
			srv.AddActive(-1)
			lastErr = err
			r.logger.Warn("outbound dial failed",
				"server_id", srv.ID(),
				"server", srv.Info.Name,
				"target", targetAddr,
				"error", err,
			)
			continue
		}

		r.logger.Debug("connection established",
			"server_id", srv.ID(),
			"server", srv.Info.Name,
			"user", principal.Username,
			"target", targetAddr,
			"lane", lane,
		)

		var handle *connections.Conn
		if r.tracker != nil {
			handle = r.tracker.Open(principal.Username, targetAddr, srv.ID(), srv.Info.Name, conn.Close)
		}

		if r.events != nil {
			entry := eventlog.Entry{
				Kind:   eventlog.KindConnectionStart,
				User:   principal.Username,
				Target: targetAddr,
				Server: srv.Info.Name,
			}
			host, _, splitErr := net.SplitHostPort(targetAddr)
			if splitErr == nil {
				if ip := net.ParseIP(host); ip != nil {
					entry.IP = host
					entry.Country = countryFor(r.geo, ip)
				}
			}
			r.events.Add(entry)
			if splitErr == nil && net.ParseIP(host) == nil {
				go r.logDNS(host)
			}
		}

		return newTrackedConn(conn, srv, handle, r.tracker, principal, r.onUserBytes, r.events, targetAddr), nil
	}

	if lastErr == nil {
		lastErr = ErrNoOutbound
	}
	return nil, fmt.Errorf("%w: %v", ErrNoOutbound, lastErr)
}

// trackedConn keeps per-server, per-user and live-connection accounting correct
// for the connection lifetime.
type trackedConn struct {
	net.Conn
	srv         *servers.Server
	handle      *connections.Conn
	tracker     *connections.Tracker
	userID      string
	onUserBytes func(userID string, in, out uint64)
	events      *eventlog.Log
	target      string
	user        string
	startedAt   time.Time

	closeOnce sync.Once
	read      atomic.Uint64
	written   atomic.Uint64
}

func newTrackedConn(conn net.Conn, srv *servers.Server, handle *connections.Conn, tracker *connections.Tracker, principal socks5.Principal, onUserBytes func(string, uint64, uint64), events *eventlog.Log, target string) net.Conn {
	return &trackedConn{
		Conn:        conn,
		srv:         srv,
		handle:      handle,
		tracker:     tracker,
		userID:      principal.ID,
		onUserBytes: onUserBytes,
		events:      events,
		target:      target,
		user:        principal.Username,
		startedAt:   time.Now(),
	}
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Add(uint64(n))
		if c.handle != nil {
			c.handle.AddRead(n)
		}
	}
	return n, err
}

func (c *trackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.written.Add(uint64(n))
		if c.handle != nil {
			c.handle.AddWrite(n)
		}
	}
	return n, err
}

func (c *trackedConn) Close() error {
	c.closeOnce.Do(func() {
		in, out := c.read.Load(), c.written.Load()
		c.srv.AddActive(-1)
		c.srv.AddBytes(in, out)
		if c.tracker != nil {
			c.tracker.Remove(c.handle)
		}
		if c.onUserBytes != nil && c.userID != "" && (in > 0 || out > 0) {
			c.onUserBytes(c.userID, in, out)
		}
		if c.events != nil {
			c.events.Add(eventlog.Entry{
				Kind:       eventlog.KindConnectionEnd,
				User:       c.user,
				Target:     c.target,
				Server:     c.srv.Info.Name,
				BytesIn:    in,
				BytesOut:   out,
				DurationMS: time.Since(c.startedAt).Milliseconds(),
			})
		}
	})
	return c.Conn.Close()
}

// logDNS resolves a proxied domain for log attribution only: routing still
// leaves name resolution to the exit server. Results are cached so a burst of
// connections to the same host produces one lookup.
func (r *Router) logDNS(host string) {
	r.dnsMu.Lock()
	if entry, ok := r.dnsCache[host]; ok && time.Since(entry.at) < dnsCacheTTL {
		r.dnsMu.Unlock()
		return
	}
	r.dnsCache[host] = dnsEntry{at: time.Now()}
	r.dnsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		r.logger.Debug("dns attribution failed", "domain", host, "error", err)
		return
	}
	seen := make(map[string]bool, len(ips))
	logged := 0
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil || seen[raw] {
			continue
		}
		seen[raw] = true
		r.events.Add(eventlog.Entry{
			Kind:    eventlog.KindDNS,
			Domain:  host,
			IP:      raw,
			Country: countryFor(r.geo, ip),
		})
		if logged++; logged >= 3 {
			break
		}
	}
}

func countryFor(db *geoip.DB, ip net.IP) string {
	if db == nil {
		return ""
	}
	return db.CountryCode(ip)
}
