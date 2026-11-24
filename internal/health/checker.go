// Package health probes every VLESS outbound through the tunnel itself.
package health

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/servers"
)

// Dialer opens a connection through a specific outbound on a specific lane.
type Dialer interface {
	DialThrough(ctx context.Context, lane int, serverTag, address string) (net.Conn, error)
}

// Checker periodically verifies that each server actually reaches the internet.
type Checker struct {
	registry   *servers.Registry
	dialer     Dialer
	cfg        config.Health
	laneBase   int
	degradedMS float64
	logger     *slog.Logger
}

// New creates a health checker. laneBase is the index of the first lane
// reserved for probes; worker i uses laneBase+i. A smoothed RTT above
// degradedMS marks the server DEGRADED instead of ONLINE.
func New(registry *servers.Registry, dialer Dialer, cfg config.Health, laneBase int, degradedMS float64, logger *slog.Logger) *Checker {
	return &Checker{
		registry:   registry,
		dialer:     dialer,
		cfg:        cfg,
		laneBase:   laneBase,
		degradedMS: degradedMS,
		logger:     logger,
	}
}

// Run probes due servers and repeats every few seconds so that per-source
// intervals (subscription server_check_seconds) take effect on time.
func (c *Checker) Run(ctx context.Context) {
	c.CheckDue(ctx)
	tick := 5 * time.Second
	if cfgInterval := c.cfg.Interval.Std(); cfgInterval > 0 && cfgInterval < tick {
		tick = cfgInterval
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckDue(ctx)
		}
	}
}

// CheckDue probes servers whose interval has elapsed. Servers without a
// per-subscription interval use the global health_check.interval.
func (c *Checker) CheckDue(ctx context.Context) {
	now := time.Now()
	var due []*servers.Server
	for _, srv := range c.registry.List() {
		if !srv.Enabled() {
			continue
		}
		interval := srv.CheckInterval()
		if interval <= 0 {
			interval = c.cfg.Interval.Std()
		}
		if last := srv.LastCheck(); !last.IsZero() && now.Sub(last) < interval {
			continue
		}
		due = append(due, srv)
	}
	c.check(ctx, due)
}

// CheckAll probes every enabled server with bounded concurrency.
func (c *Checker) CheckAll(ctx context.Context) {
	c.check(ctx, c.registry.List())
}

func (c *Checker) check(ctx context.Context, list []*servers.Server) {
	if len(list) == 0 {
		return
	}

	workers := c.cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(list) {
		workers = len(list)
	}

	jobs := make(chan *servers.Server)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			lane := c.laneBase + worker
			for srv := range jobs {
				c.checkOne(ctx, srv, lane)
			}
		}(i)
	}

	for _, srv := range list {
		if !srv.Enabled() {
			continue
		}
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- srv:
		}
	}
	close(jobs)
	wg.Wait()
}

// CheckServer probes one server immediately (used by the API and CLI).
func (c *Checker) CheckServer(ctx context.Context, srv *servers.Server) {
	c.checkOne(ctx, srv, c.laneBase)
}

func (c *Checker) checkOne(ctx context.Context, srv *servers.Server, lane int) {
	// Servers parked as ERROR have no outbound in sing-box at all, so probing
	// them would only produce a confusing Clash API error.
	if srv.Status() == servers.StatusError || srv.Status() == servers.StatusDisabled {
		return
	}
	urls := append([]string{c.cfg.URL}, c.cfg.FallbackURLs...)
	var lastErr error
	for _, target := range urls {
		if target == "" {
			continue
		}
		rtt, err := c.probe(ctx, srv, lane, target)
		if err == nil {
			c.applySuccess(srv, rtt)
			return
		}
		lastErr = err
	}
	c.applyFailure(srv, lastErr)
}

func (c *Checker) probe(ctx context.Context, srv *servers.Server, lane int, target string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout.Std())
	defer cancel()

	tag := srv.ID()
	client := &http.Client{
		Timeout: c.cfg.Timeout.Std(),
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// The address from the URL is what must be reached through the
				// tunnel; the proxy itself is addressed by the lane.
				return c.dialer.DialThrough(ctx, lane, tag, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "multivless-healthcheck/1")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = resp.Body.Read(make([]byte, 1))
	rtt := time.Since(start)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return rtt, &statusError{code: resp.StatusCode}
	}
	return rtt, nil
}

func (c *Checker) applySuccess(srv *servers.Server, rtt time.Duration) {
	changed, from, to := srv.MarkSuccess(rtt, c.cfg.SuccessesBeforeUp, c.degradedMS)
	if changed {
		c.logger.Info("server status changed",
			"server_id", srv.ID(),
			"name", srv.Info.Name,
			"from", from,
			"to", to,
			"latency_ms", float64(rtt.Microseconds())/1000,
		)
	}
}

func (c *Checker) applyFailure(srv *servers.Server, err error) {
	if err == nil {
		err = &statusError{code: 0}
	}
	changed, from, to := srv.MarkFailure(err, c.cfg.FailuresBeforeDown)
	if changed {
		c.logger.Warn("server status changed",
			"server_id", srv.ID(),
			"name", srv.Info.Name,
			"from", from,
			"to", to,
			"error", err.Error(),
		)
	}
}

type statusError struct{ code int }

func (e *statusError) Error() string {
	if e.code == 0 {
		return "health check failed"
	}
	return "unexpected health check status " + http.StatusText(e.code)
}
