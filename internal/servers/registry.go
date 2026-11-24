// Package servers holds the in-memory state of every configured VLESS server.
package servers

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

// Status is the liveness of a server as seen by the health checker.
type Status string

const (
	StatusUnknown  Status = "UNKNOWN"
	StatusOnline   Status = "ONLINE"
	StatusDegraded Status = "DEGRADED"
	StatusOffline  Status = "OFFLINE"
	StatusDisabled Status = "DISABLED"
	StatusError    Status = "ERROR"
)

// Server is a configured VLESS outbound plus its runtime state.
type Server struct {
	Info *parser.Server

	mu         sync.RWMutex
	status     Status
	enabled    bool
	weight     int
	failures   int
	successes  int
	latencyMS  float64 // exponential moving average
	throughput float64 // observed throughput score (0-100), neutral until measured
	lastCheck  time.Time
	lastError  string
	// checkInterval comes from the source subscription; zero means the
	// global health_check.interval.
	checkInterval time.Duration

	active   atomic.Int64
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
	probeOK  atomic.Uint64
	probeErr atomic.Uint64
}

// New creates a server in the UNKNOWN state.
func New(info *parser.Server) *Server {
	weight := info.Weight
	if weight <= 0 {
		weight = 100
	}
	return &Server{
		Info:       info,
		status:     StatusUnknown,
		enabled:    true,
		weight:     weight,
		throughput: 50, // neutral score until real throughput data exists
	}
}

// ID returns the stable server identifier.
func (s *Server) ID() string { return s.Info.ID }

// Snapshot is a point-in-time copy safe to serialise.
type Snapshot struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Status     Status    `json:"status"`
	Enabled    bool      `json:"enabled"`
	Weight     int       `json:"weight"`
	Security   string    `json:"security"`
	Network    string    `json:"network"`
	LatencyMS  float64   `json:"latency_ms"`
	Active     int64     `json:"active_connections"`
	Throughput float64   `json:"throughput_score"`
	LastCheck  time.Time `json:"last_check"`
	LastError  string    `json:"last_error,omitempty"`
	BytesIn    uint64    `json:"bytes_in"`
	BytesOut   uint64    `json:"bytes_out"`
}

// Snapshot copies the current state.
func (s *Server) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID:         s.Info.ID,
		Name:       s.Info.Name,
		Host:       s.Info.Host,
		Port:       s.Info.Port,
		Status:     s.status,
		Enabled:    s.enabled,
		Weight:     s.weight,
		Security:   string(s.Info.Security),
		Network:    string(s.Info.Network),
		LatencyMS:  s.latencyMS,
		Active:     s.active.Load(),
		Throughput: s.throughput,
		LastCheck:  s.lastCheck,
		LastError:  s.lastError,
		BytesIn:    s.bytesIn.Load(),
		BytesOut:   s.bytesOut.Load(),
	}
}

// Status returns the current status.
func (s *Server) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// Enabled reports whether the operator has the server switched on.
func (s *Server) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// UpdateInfo replaces the connection parameters of a server, preserving its
// runtime state.
func (s *Server) UpdateInfo(info *parser.Server) {
	if info == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Info = info
}

// Weight returns the configured balancing weight.
func (s *Server) Weight() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.weight
}

// CheckInterval returns the per-source probe interval (zero = global default).
func (s *Server) CheckInterval() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkInterval
}

// SetCheckInterval updates the probe interval derived from the subscription.
func (s *Server) SetCheckInterval(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkInterval = interval
}

// LastCheck returns the time of the last probe (zero before the first one).
func (s *Server) LastCheck() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastCheck
}

// LatencyMS returns the smoothed RTT in milliseconds.
func (s *Server) LatencyMS() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latencyMS
}

// ThroughputScore returns the 0-100 throughput estimate.
func (s *Server) ThroughputScore() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.throughput
}

// SetEnabled switches the server on or off. Disabled servers are not selectable.
func (s *Server) SetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled == enabled {
		return
	}
	s.enabled = enabled
	if enabled {
		// A re-enabled server must be probed again before serving traffic.
		s.status = StatusUnknown
		s.failures = 0
		s.successes = 0
	} else {
		s.status = StatusDisabled
	}
}

// SetWeight updates the balancing weight.
func (s *Server) SetWeight(weight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if weight > 0 {
		s.weight = weight
	}
}

// MarkSuccess records a successful probe and applies the ONLINE/OFFLINE
// thresholds and hysteresis described in the specification.
func (s *Server) MarkSuccess(rtt time.Duration, successesBeforeUp int, degradedLatencyMS float64) (changed bool, from, to Status) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.status
	s.lastCheck = time.Now()
	s.lastError = ""
	s.successes++
	s.failures = 0
	s.probeOK.Add(1)

	ms := float64(rtt.Microseconds()) / 1000.0
	if s.latencyMS == 0 {
		s.latencyMS = ms
	} else {
		// Exponential moving average with alpha = 0.3.
		s.latencyMS = 0.7*s.latencyMS + 0.3*ms
	}

	if s.enabled && s.successes >= successesBeforeUp {
		if degradedLatencyMS > 0 && s.latencyMS > degradedLatencyMS {
			s.status = StatusDegraded
		} else {
			s.status = StatusOnline
		}
	}
	return prev != s.status, prev, s.status
}

// MarkFailure records a failed probe.
func (s *Server) MarkFailure(err error, failuresBeforeDown int) (changed bool, from, to Status) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.status
	s.lastCheck = time.Now()
	if err != nil {
		s.lastError = err.Error()
	}
	if s.status == StatusDisabled {
		return false, prev, prev
	}
	s.failures++
	s.successes = 0
	s.probeErr.Add(1)
	if s.failures >= failuresBeforeDown {
		s.status = StatusOffline
	}
	return prev != s.status, prev, s.status
}

// MarkConfigError parks a server whose configuration cannot be represented in
// sing-box. It is excluded from balancing until the operator fixes it.
func (s *Server) MarkConfigError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = StatusError
	s.lastError = msg
	s.enabled = false
}

// Selectable reports whether new connections may use this server.
func (s *Server) Selectable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return false
	}
	return s.status == StatusOnline || s.status == StatusDegraded
}

// AddActive adjusts the active connection counter by delta.
func (s *Server) AddActive(delta int64) { s.active.Add(delta) }

// Reliability returns the share of successful probes in percent (0-100).
// A server with no probe history is treated as fully reliable so that new
// servers are not permanently penalised.
func (s *Server) Reliability() float64 {
	ok, fail := s.probeOK.Load(), s.probeErr.Load()
	total := ok + fail
	if total == 0 {
		return 100
	}
	return float64(ok) / float64(total) * 100
}

// AddBytes records transferred bytes.
func (s *Server) AddBytes(in, out uint64) {
	s.bytesIn.Add(in)
	s.bytesOut.Add(out)
} // SyncRecord is one server as persisted in storage.
type SyncRecord struct {
	Info    *parser.Server
	Enabled bool
	Weight  int
	// CheckInterval is the source subscription's probe interval.
	CheckInterval time.Duration
}

// Registry is the set of all known servers.
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]*Server
	order []*Server
}

// Sync reconciles the registry with persisted records. Existing servers keep
// their runtime state; added and removed counts describe the ID set change.
func (r *Registry) Sync(records []SyncRecord) (added, removed int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make(map[string]*Server, len(records))
	order := make([]*Server, 0, len(records))
	for _, rec := range records {
		if rec.Info == nil {
			continue
		}
		if _, dup := next[rec.Info.ID]; dup {
			continue
		}
		if existing, ok := r.byID[rec.Info.ID]; ok {
			existing.UpdateInfo(rec.Info)
			existing.SetWeight(rec.Weight)
			existing.SetEnabled(rec.Enabled)
			existing.SetCheckInterval(rec.CheckInterval)
			next[rec.Info.ID] = existing
			order = append(order, existing)
			continue
		}
		srv := New(rec.Info)
		srv.SetWeight(rec.Weight)
		srv.SetEnabled(rec.Enabled)
		srv.SetCheckInterval(rec.CheckInterval)
		next[rec.Info.ID] = srv
		order = append(order, srv)
		added++
	}
	for id := range r.byID {
		if _, ok := next[id]; !ok {
			removed++
		}
	}

	r.byID = next
	r.order = order
	return added, removed
}

// ResetProbeCounters clears failure streaks so that an engine restart does not
// push healthy servers to OFFLINE.
func (r *Registry) ResetProbeCounters() {
	for _, srv := range r.List() {
		srv.mu.Lock()
		srv.failures = 0
		srv.successes = 0
		srv.mu.Unlock()
	}
}

// NewRegistry builds a registry, dropping duplicate IDs (later entries win on
// name/weight but keep the first position).
func NewRegistry(infos []*parser.Server) *Registry {
	r := &Registry{byID: make(map[string]*Server, len(infos))}
	for _, info := range infos {
		if info == nil {
			continue
		}
		if _, ok := r.byID[info.ID]; ok {
			continue
		}
		srv := New(info)
		r.byID[info.ID] = srv
		r.order = append(r.order, srv)
	}
	return r
}

// Get returns a server by ID.
func (r *Registry) Get(id string) (*Server, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	srv, ok := r.byID[id]
	return srv, ok
}

// List returns all servers in insertion order.
func (r *Registry) List() []*Server {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Server, len(r.order))
	copy(out, r.order)
	return out
}

// Selectable returns servers currently eligible for new connections.
func (r *Registry) Selectable() []*Server {
	var out []*Server
	for _, srv := range r.List() {
		if srv.Selectable() {
			out = append(out, srv)
		}
	}
	return out
}

// Counts summarises statuses for logging and dashboards.
type Counts struct {
	Total    int `json:"total"`
	Online   int `json:"online"`
	Degraded int `json:"degraded"`
	Offline  int `json:"offline"`
	Unknown  int `json:"unknown"`
	Disabled int `json:"disabled"`
	Error    int `json:"error"`
}

// Counts computes the current status distribution.
func (r *Registry) Counts() Counts {
	c := Counts{}
	for _, srv := range r.List() {
		c.Total++
		snap := srv.Snapshot()
		switch snap.Status {
		case StatusOnline:
			c.Online++
		case StatusDegraded:
			c.Degraded++
		case StatusOffline:
			c.Offline++
		case StatusDisabled:
			c.Disabled++
		case StatusError:
			c.Error++
		default:
			c.Unknown++
		}
	}
	return c
}

// Tags returns all outbound tags in a deterministic order.
func (r *Registry) Tags() []string {
	list := r.List()
	tags := make([]string, 0, len(list))
	for _, srv := range list {
		tags = append(tags, srv.ID())
	}
	sort.Strings(tags)
	return tags
}
