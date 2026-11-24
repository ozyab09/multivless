package storage

import (
	"path/filepath"
	"testing"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testServer(id string, source string) Server {
	return Server{
		Info: &parser.Server{
			ID: id, Name: id, Host: "203.0.113.1", Port: 443,
			UUID:     "11111111-2222-4333-8444-555555555555",
			Security: parser.SecurityTLS, SNI: "example.org", Network: parser.NetworkWS,
			Path: "/ws", ALPN: []string{"h2", "http/1.1"},
		},
		SourceID: source,
		Enabled:  true,
		Weight:   120,
	}
}

func TestServerRoundTrip(t *testing.T) {
	store := openTestStore(t)
	if err := store.UpsertServers([]Server{testServer("srv_1", "sub_1")}); err != nil {
		t.Fatalf("UpsertServers: %v", err)
	}

	list, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("servers = %d", len(list))
	}
	got := list[0]
	if got.Info.Name != "srv_1" || got.SourceID != "sub_1" || got.Weight != 120 || !got.Enabled {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.Info.Network != parser.NetworkWS || got.Info.ALPN[1] != "http/1.1" {
		t.Fatalf("transport fields lost: %+v", got.Info)
	}

	if err := store.SetServerEnabled("srv_1", false); err != nil {
		t.Fatal(err)
	}
	list, _ = store.ListServers()
	if list[0].Enabled {
		t.Fatal("enabled flag was not persisted")
	}
	if err := store.SetServerWeight("srv_1", 42); err != nil {
		t.Fatal(err)
	}
	list, _ = store.ListServers()
	if list[0].Weight != 42 {
		t.Fatalf("weight = %d", list[0].Weight)
	}
}

func TestDeleteSourceServersExcept(t *testing.T) {
	store := openTestStore(t)
	if err := store.UpsertServers([]Server{
		testServer("srv_keep", "sub_1"),
		testServer("srv_drop", "sub_1"),
		testServer("srv_other", "sub_2"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSourceServersExcept("sub_1", []string{"srv_keep"}); err != nil {
		t.Fatal(err)
	}
	list, _ := store.ListServers()
	if len(list) != 2 {
		t.Fatalf("servers after cleanup = %d, want 2", len(list))
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	store := openTestStore(t)
	sub := Subscription{ID: "sub_1", Name: "test", URL: "https://example.com/sub",
		IntervalSeconds: 3600, Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	subs, err := store.ListSubscriptions()
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions = %d, err = %v", len(subs), err)
	}

	if err := store.RecordSubscriptionResult("sub_1", "etag-1", "lm-1", "", 3, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSubscription("sub_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != "etag-1" || got.ServerCount != 3 || got.LastSuccessAt.IsZero() {
		t.Fatalf("result not recorded: %+v", got)
	}

	if err := store.RecordSubscriptionResult("sub_1", "", "", "boom", 3, false); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetSubscription("sub_1")
	if got.LastError != "boom" || got.ServerCount != 3 {
		t.Fatalf("failure overwrote good state: %+v", got)
	}

	if err := store.UpdateSubscription("sub_1", "renamed", "https://example.com/other", 7200, 45, false); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetSubscription("sub_1")
	if got.Name != "renamed" || got.IntervalSeconds != 7200 || got.ServerCheckSeconds != 45 || got.Enabled {
		t.Fatalf("update failed: %+v", got)
	}

	if err := store.DeleteSubscription("sub_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSubscription("sub_1"); err == nil {
		t.Fatal("subscription still present")
	}
}

func TestUsersAndSettings(t *testing.T) {
	store := openTestStore(t)
	user := User{ID: "usr_1", Username: "alice", PasswordHash: "$argon2id$fake", Enabled: true}
	if err := store.CreateUser(user); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(user); err == nil {
		t.Fatal("duplicate username must fail")
	}

	got, err := store.GetUserByUsername("alice")
	if err != nil || got.ID != "usr_1" || !got.Enabled {
		t.Fatalf("GetUserByUsername: %+v, %v", got, err)
	}
	if err := store.SetUserEnabled("usr_1", false); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUserBytes("usr_1", 100, 200); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetUser("usr_1")
	if got.Enabled || got.BytesIn != 100 || got.BytesOut != 200 {
		t.Fatalf("user state: %+v", got)
	}

	if err := store.SetSetting("balancer_strategy", "round-robin"); err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.GetSetting("balancer_strategy")
	if err != nil || !ok || value != "round-robin" {
		t.Fatalf("setting = %q, %v, %v", value, ok, err)
	}

	if err := store.DeleteUser("usr_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUser("usr_1"); err == nil {
		t.Fatal("user still present")
	}
}
