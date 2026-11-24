package eventlog

import "testing"

func TestLogKeepsNewestAndAssignsIDs(t *testing.T) {
	log := New(3)
	for i := 0; i < 5; i++ {
		log.Add(Entry{Kind: KindDNS, Domain: "example.com"})
	}
	list := log.List(10)
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	if list[0].ID != 5 || list[2].ID != 3 {
		t.Fatalf("ids = %d..%d, want 5..3", list[0].ID, list[2].ID)
	}
	if list[0].Time.IsZero() {
		t.Fatal("timestamp was not set")
	}
}

func TestNilLogIsSafe(t *testing.T) {
	var log *Log
	log.Add(Entry{})
	if log.List(10) != nil {
		t.Fatal("nil log returned entries")
	}
}
