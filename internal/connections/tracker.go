// Package connections tracks live proxy connections for the API and metrics.
package connections

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is a point-in-time view of one connection.
type Snapshot struct {
	ID         string     `json:"id"`
	User       string     `json:"user"`
	Target     string     `json:"target"`
	ServerID   string     `json:"server_id"`
	ServerName string     `json:"server_name"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Active     bool       `json:"active"`
	DurationMS int64      `json:"duration_ms"`
	BytesIn    uint64     `json:"bytes_in"`
	BytesOut   uint64     `json:"bytes_out"`
}

// Summary aggregates tracker state.
type Summary struct {
	Active   int    `json:"active"`
	Total    uint64 `json:"total"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

// Conn is a tracked connection handle.
type Conn struct {
	ID         string
	User       string
	Target     string
	ServerID   string
	ServerName string
	StartedAt  time.Time

	in    atomic.Uint64
	out   atomic.Uint64
	close func() error
}

// AddRead counts bytes received from the target.
func (c *Conn) AddRead(n int) { c.in.Add(uint64(n)) }

// AddWrite counts bytes sent to the target.
func (c *Conn) AddWrite(n int) { c.out.Add(uint64(n)) }

// inactiveRetention is how long closed connections stay visible.
const inactiveRetention = 60 * time.Second

// Tracker keeps live connections and recently closed ones in memory.
type Tracker struct {
	mu     sync.Mutex
	conns  map[string]*Conn
	recent []Snapshot
	seq    atomic.Uint64

	totalConns atomic.Uint64
	closedIn   atomic.Uint64
	closedOut  atomic.Uint64
}

// New creates a tracker.
func New() *Tracker { return &Tracker{conns: make(map[string]*Conn)} }

// Open registers a new connection.
func (t *Tracker) Open(user, target, serverID, serverName string, closer func() error) *Conn {
	conn := &Conn{
		ID:         fmt.Sprintf("conn_%x", t.seq.Add(1)),
		User:       user,
		Target:     target,
		ServerID:   serverID,
		ServerName: serverName,
		StartedAt:  time.Now(),
		close:      closer,
	}
	t.mu.Lock()
	t.conns[conn.ID] = conn
	t.mu.Unlock()
	t.totalConns.Add(1)
	return conn
}

// Remove unregisters a connection and folds its bytes into the totals.
func (t *Tracker) Remove(c *Conn) {
	if c == nil {
		return
	}
	t.mu.Lock()
	_, ok := t.conns[c.ID]
	delete(t.conns, c.ID)
	if ok {
		now := time.Now()
		t.recent = append(t.recent, Snapshot{
			ID:         c.ID,
			User:       c.User,
			Target:     c.Target,
			ServerID:   c.ServerID,
			ServerName: c.ServerName,
			StartedAt:  c.StartedAt,
			EndedAt:    &now,
			Active:     false,
			DurationMS: now.Sub(c.StartedAt).Milliseconds(),
			BytesIn:    c.in.Load(),
			BytesOut:   c.out.Load(),
		})
		t.trimRecent(now)
	}
	t.mu.Unlock()
	if ok {
		t.closedIn.Add(c.in.Load())
		t.closedOut.Add(c.out.Load())
	}
}

// List returns live connections, newest first.
func (t *Tracker) List() []Snapshot {
	t.mu.Lock()
	out := make([]Snapshot, 0, len(t.conns))
	now := time.Now()
	for _, c := range t.conns {
		out = append(out, Snapshot{
			ID:         c.ID,
			User:       c.User,
			Target:     c.Target,
			ServerID:   c.ServerID,
			ServerName: c.ServerName,
			StartedAt:  c.StartedAt,
			Active:     true,
			DurationMS: now.Sub(c.StartedAt).Milliseconds(),
			BytesIn:    c.in.Load(),
			BytesOut:   c.out.Load(),
		})
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Recent returns connections closed within the retention window, newest first.
func (t *Tracker) Recent() []Snapshot {
	t.mu.Lock()
	t.trimRecent(time.Now())
	out := make([]Snapshot, len(t.recent))
	copy(out, t.recent)
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		iEnd, jEnd := out[i].EndedAt, out[j].EndedAt
		if iEnd == nil || jEnd == nil {
			return out[i].ID > out[j].ID
		}
		if iEnd.Equal(*jEnd) {
			return out[i].ID > out[j].ID
		}
		return iEnd.After(*jEnd)
	})
	return out
}

// trimRecent drops closed connections older than inactiveRetention.
// The caller must hold t.mu.
func (t *Tracker) trimRecent(now time.Time) {
	cutoff := now.Add(-inactiveRetention)
	kept := t.recent[:0]
	for _, snap := range t.recent {
		if snap.EndedAt != nil && snap.EndedAt.After(cutoff) {
			kept = append(kept, snap)
		}
	}
	t.recent = kept
}

// Close terminates one connection (including its live byte counters).
func (t *Tracker) Close(id string) error {
	t.mu.Lock()
	c, ok := t.conns[id]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("connection %s not found", id)
	}
	if c.close != nil {
		return c.close()
	}
	return nil
}

// Summary returns aggregate counters.
func (t *Tracker) Summary() Summary {
	totalIn := t.closedIn.Load()
	totalOut := t.closedOut.Load()

	t.mu.Lock()
	active := len(t.conns)
	for _, c := range t.conns {
		totalIn += c.in.Load()
		totalOut += c.out.Load()
	}
	t.mu.Unlock()

	return Summary{
		Active:   active,
		Total:    t.totalConns.Load(),
		BytesIn:  totalIn,
		BytesOut: totalOut,
	}
}
