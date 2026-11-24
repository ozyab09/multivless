package connections

import "testing"

func TestClosedConnectionIsRetained(t *testing.T) {
	tracker := New()
	conn := tracker.Open("alice", "example.com:443", "srv_1", "exit", nil)
	conn.AddRead(100)
	conn.AddWrite(50)
	tracker.Remove(conn)

	if active := tracker.List(); len(active) != 0 {
		t.Fatalf("active = %d, want 0", len(active))
	}
	recent := tracker.Recent()
	if len(recent) != 1 {
		t.Fatalf("recent = %d, want 1", len(recent))
	}
	got := recent[0]
	if got.ID != conn.ID || got.Active || got.EndedAt == nil {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
	if got.BytesIn != 100 || got.BytesOut != 50 {
		t.Fatalf("bytes = %d/%d, want 100/50", got.BytesIn, got.BytesOut)
	}
	summary := tracker.Summary()
	if summary.BytesIn != 100 || summary.BytesOut != 50 {
		t.Fatalf("summary bytes = %d/%d", summary.BytesIn, summary.BytesOut)
	}
}
