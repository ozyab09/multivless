package singbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/subscriptions/parser"
)

// Options configures the engine manager.
type Options struct {
	Engine      config.Engine
	HealthLanes int
	Logger      *slog.Logger
}

// Manager owns the sing-box process and the lane-based outbound selection.
type Manager struct {
	engine      config.Engine
	healthLanes int
	logger      *slog.Logger

	mu       sync.Mutex
	lanes    []Lane
	locks    []sync.Mutex
	clash    *clashClient
	cmd      *exec.Cmd
	stopping bool
	running  bool
	done     chan struct{}
}

// NewManager creates an engine manager. No process is started until Start.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Manager{
		engine:      opts.Engine,
		healthLanes: opts.HealthLanes,
		logger:      opts.Logger,
		clash:       newClashClient(opts.Engine.ClashAPI),
	}
}

// Lanes returns the lane layout (data lanes first, then health lanes).
func (m *Manager) Lanes() []Lane { return m.lanes }

// DataLaneCount returns how many lanes may carry client traffic.
func (m *Manager) DataLaneCount() int { return m.engine.Lanes }

// TorrentLaneOffset returns the index of the first torrent-bypass lane.
func (m *Manager) TorrentLaneOffset() int { return m.engine.Lanes }

// TorrentLaneCount returns how many lanes sniff for direct BitTorrent.
func (m *Manager) TorrentLaneCount() int { return m.engine.Lanes }

// HealthLaneOffset returns the index of the first health-check lane.
func (m *Manager) HealthLaneOffset() int { return 2 * m.engine.Lanes }

// Start allocates lane ports, writes and validates the sing-box configuration,
// launches the process and waits until it accepts API calls.
// Servers whose transports cannot be represented are returned as unsupported.
func (m *Manager) Start(ctx context.Context, srvs []*parser.Server) ([]*parser.Server, error) {
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if running {
		return nil, fmt.Errorf("engine already running")
	}
	if _, err := os.Stat(m.engine.Binary); err != nil {
		return nil, fmt.Errorf("sing-box binary %q: %w", m.engine.Binary, err)
	}

	// Lanes are allocated once: reloading servers must not change the local
	// ports the gateway dials into.
	m.mu.Lock()
	existing := m.lanes
	m.mu.Unlock()

	totalLanes := 2*m.engine.Lanes + m.healthLanes
	lanes := make([]Lane, 0, totalLanes)
	if len(existing) == totalLanes {
		lanes = append(lanes, existing...)
	} else {
		for i := 0; i < totalLanes; i++ {
			port, err := freePort()
			if err != nil {
				return nil, err
			}
			kind := "data"
			torrent := false
			switch {
			case i >= 2*m.engine.Lanes:
				kind = "health"
			case i >= m.engine.Lanes:
				kind = "torrent"
				torrent = true
			}
			lanes = append(lanes, Lane{
				InboundTag:    fmt.Sprintf("lane-in-%s-%d", kind, i),
				GroupTag:      fmt.Sprintf("lane-group-%s-%d", kind, i),
				Port:          port,
				TorrentDirect: torrent,
			})
		}
	}

	raw, unsupported, err := Build(BuildOptions{
		Lanes:    lanes,
		Servers:  srvs,
		ClashAPI: m.engine.ClashAPI,
		LogLevel: m.engine.LogLevel,
	})
	if err != nil {
		return unsupported, err
	}

	if err := os.MkdirAll(filepath.Dir(m.engine.ConfigPath), 0o750); err != nil {
		return unsupported, fmt.Errorf("create engine config dir: %w", err)
	}
	if err := os.WriteFile(m.engine.ConfigPath, raw, 0o600); err != nil {
		return unsupported, fmt.Errorf("write engine config: %w", err)
	}
	if err := m.checkConfig(ctx); err != nil {
		return unsupported, err
	}

	m.mu.Lock()
	m.lanes = lanes
	m.locks = make([]sync.Mutex, len(lanes))
	m.done = make(chan struct{})
	m.stopping = false
	m.mu.Unlock()

	if err := m.spawn(ctx); err != nil {
		return unsupported, err
	}

	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	go m.supervise()
	return unsupported, nil
}

// Apply rebuilds the engine configuration with a new server set. sing-box is
// restarted, so existing connections are dropped; the lane ports stay the same.
func (m *Manager) Apply(ctx context.Context, srvs []*parser.Server) ([]*parser.Server, error) {
	if !m.Running() {
		return m.Start(ctx, srvs)
	}
	if err := m.Stop(); err != nil {
		return nil, err
	}
	return m.Start(ctx, srvs)
}

// Running reports whether the engine process is supervised.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Manager) checkConfig(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, m.engine.Binary, "check", "-c", m.engine.ConfigPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sing-box config rejected: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// spawn starts the process and waits for the Clash API to answer.
func (m *Manager) spawn(ctx context.Context) error {
	cmd := exec.Command(m.engine.Binary, "run", "-c", m.engine.ConfigPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("engine stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("engine stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()

	go m.pipeLogs("engine", stdout)
	go m.pipeLogs("engine", stderr)

	readyCtx, cancel := context.WithTimeout(ctx, m.engine.StartTimeout.Std())
	defer cancel()
	if err := m.clash.waitReady(readyCtx); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("sing-box did not become ready: %w", err)
	}
	return nil
}

func (m *Manager) pipeLogs(component string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m.logger.Debug("engine output", "component", component, "line", scanner.Text())
	}
}

// supervise waits for the engine process and restarts it after a crash when
// restart_on_failure is enabled.
func (m *Manager) supervise() {
	defer close(m.done)

	for {
		m.mu.Lock()
		cmd := m.cmd
		m.mu.Unlock()
		if cmd == nil {
			return
		}

		waitErr := cmd.Wait()

		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		if stopping {
			return
		}

		m.logger.Error("sing-box exited unexpectedly", "error", waitErr)
		if !m.engine.RestartOnFailure {
			m.logger.Error("engine.restart_on_failure=false: the gateway now rejects every new connection until restarted")
			m.mu.Lock()
			m.running = false
			m.mu.Unlock()
			return
		}

		delay := m.engine.RestartDelay.Std()
		if delay <= 0 {
			delay = 5 * time.Second
		}

		restarted := false
		for !restarted {
			select {
			case <-time.After(delay):
			}
			m.mu.Lock()
			stopping = m.stopping
			m.mu.Unlock()
			if stopping {
				return
			}
			if err := m.spawn(context.Background()); err != nil {
				m.logger.Error("sing-box restart failed", "error", err)
				continue
			}
			restarted = true
			m.logger.Info("sing-box restarted")
		}
	}
}

// Stop terminates the engine and waits for the supervisor to finish.
func (m *Manager) Stop() error {
	m.mu.Lock()
	m.stopping = true
	m.running = false
	cmd := m.cmd
	done := m.done
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("timed out waiting for sing-box to stop")
		}
	}
	return nil
}

// DialThrough routes a connection to address through serverTag using lane.
// The lane mutex is held only for the "select + SOCKS handshake" pair, so the
// selected outbound stays pinned to this TCP connection afterwards.
func (m *Manager) DialThrough(ctx context.Context, lane int, serverTag, address string) (net.Conn, error) {
	m.mu.Lock()
	if lane < 0 || lane >= len(m.lanes) {
		m.mu.Unlock()
		return nil, fmt.Errorf("lane %d out of range", lane)
	}
	ln := m.lanes[lane]
	m.mu.Unlock()

	m.locks[lane].Lock()
	defer m.locks[lane].Unlock()

	if err := m.clash.selectOutbound(ctx, ln.GroupTag, serverTag); err != nil {
		return nil, fmt.Errorf("select outbound %s: %w", serverTag, err)
	}

	conn, err := dialSOCKS5(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Port)), address)
	if err != nil {
		return nil, fmt.Errorf("dial lane %d: %w", lane, err)
	}
	return conn, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate local port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// clashClient is a minimal Clash API client: only outbound selection is needed.
type clashClient struct {
	base string
	http *http.Client
}

func newClashClient(addr string) *clashClient {
	return &clashClient{
		base: "http://" + addr,
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *clashClient) selectOutbound(ctx context.Context, group, outbound string) error {
	body, err := json.Marshal(map[string]string{"name": outbound})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/proxies/"+group, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("clash api returned %s", resp.Status)
	}
	return nil
}

func (c *clashClient) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c.probe(ctx) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *clashClient) probe(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.base+"/proxies", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clash api status %s", resp.Status)
	}
	return nil
}

// dialSOCKS5 performs a CONNECT handshake against a local SOCKS5 entry point.
func dialSOCKS5(ctx context.Context, proxyAddr, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("target address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("target port %q", portStr)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fail(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fail(err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fail(fmt.Errorf("socks5 proxy refused no-auth method"))
	}

	req := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	switch {
	case ip != nil && ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	case ip != nil:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	default:
		if len(host) > 255 {
			return fail(fmt.Errorf("host too long"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))

	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(err)
	}
	if head[0] != 0x05 {
		return fail(fmt.Errorf("unexpected socks version %d", head[0]))
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("socks5 connect failed with code 0x%02x", head[1]))
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return fail(err)
		}
		skip = int(one[0])
	default:
		return fail(fmt.Errorf("unexpected socks address type %d", head[3]))
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fail(err)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
