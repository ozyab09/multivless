// Package eventlog keeps a small in-memory history of connection and DNS
// events for the admin UI. It is deliberately ephemeral: events restart with
// the gateway and never contain VLESS secrets.
package eventlog

import (
	"sync"
	"time"
)

// Event kinds.
const (
	KindConnectionStart = "connection_start"
	KindConnectionEnd   = "connection_end"
	KindDNS             = "dns"
)

// Entry is one admin-visible event.
type Entry struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	User       string    `json:"user,omitempty"`
	Target     string    `json:"target,omitempty"`
	Server     string    `json:"server,omitempty"`
	Domain     string    `json:"domain,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Country    string    `json:"country,omitempty"`
	BytesIn    uint64    `json:"bytes_in,omitempty"`
	BytesOut   uint64    `json:"bytes_out,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

// Log is a fixed-size ring buffer of entries.
type Log struct {
	mu      sync.Mutex
	max     int
	next    uint64
	entries []Entry
}

// New creates a log that keeps at most max entries.
func New(max int) *Log {
	if max <= 0 {
		max = 1000
	}
	return &Log{max: max}
}

// Add appends an entry and returns it with the assigned ID and timestamp.
func (l *Log) Add(entry Entry) Entry {
	if l == nil {
		return entry
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	entry.ID = l.next
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	l.entries = append(l.entries, entry)
	if over := len(l.entries) - l.max; over > 0 {
		l.entries = append(l.entries[:0], l.entries[over:]...)
	}
	return entry
}

// List returns up to limit entries, newest first.
func (l *Log) List(limit int) []Entry {
	if l == nil {
		return nil
	}
	if limit <= 0 || limit > l.max {
		limit = l.max
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.entries)
	if n > limit {
		n = limit
	}
	out := make([]Entry, 0, n)
	for i := len(l.entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.entries[i])
	}
	return out
}
