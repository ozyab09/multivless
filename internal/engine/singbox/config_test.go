package singbox

import (
	"encoding/json"
	"testing"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

func TestBuildGeneratesLanesSelectorsAndNoDirectFallback(t *testing.T) {
	servers := []*parser.Server{
		{
			ID: "srv_a", Name: "a", Host: "203.0.113.10", Port: 443,
			UUID:     "11111111-2222-4333-8444-555555555555",
			Security: parser.SecurityReality, SNI: "example.com", Fingerprint: "chrome",
			PublicKey: "PUBKEY", ShortID: "abcd", Network: parser.NetworkTCP,
			Flow: "xtls-rprx-vision",
		},
		{
			ID: "srv_b", Name: "b", Host: "203.0.113.11", Port: 443,
			UUID:     "11111111-2222-4333-8444-555555555556",
			Security: parser.SecurityTLS, SNI: "example.org",
			Network: parser.NetworkWS, Path: "/ws", HostHeader: "example.org",
		},
	}
	lanes := []Lane{
		{InboundTag: "lane-in-data-0", GroupTag: "lane-group-data-0", Port: 20001},
		{InboundTag: "lane-in-health-1", GroupTag: "lane-group-health-1", Port: 20002},
	}

	raw, unsupported, err := Build(BuildOptions{
		Lanes:    lanes,
		Servers:  servers,
		ClashAPI: "127.0.0.1:19091",
		LogLevel: "warn",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(unsupported) != 0 {
		t.Fatalf("unexpected unsupported servers: %v", unsupported)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}

	inbounds := cfg["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Fatalf("inbounds = %d, want 2", len(inbounds))
	}

	outbounds := cfg["outbounds"].([]any)
	if len(outbounds) != 4 { // 2 vless + 2 selectors
		t.Fatalf("outbounds = %d, want 4", len(outbounds))
	}

	var selectors, vless int
	for _, item := range outbounds {
		ob := item.(map[string]any)
		switch ob["type"] {
		case "selector":
			selectors++
			members := ob["outbounds"].([]any)
			if len(members) != 2 {
				t.Errorf("selector %v has %d members", ob["tag"], len(members))
			}
		case "vless":
			vless++
		case "direct":
			t.Error("generated config must never contain a direct outbound")
		}
	}
	if selectors != 2 || vless != 2 {
		t.Fatalf("selectors=%d vless=%d", selectors, vless)
	}

	route := cfg["route"].(map[string]any)
	rules := route["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("route rules = %d, want 2", len(rules))
	}
	for _, r := range rules {
		rule := r.(map[string]any)
		if _, ok := rule["outbound"]; !ok {
			t.Error("route rule without an outbound")
		}
	}

	// Secrets must only live in the generated engine config, never in the IDs.
	for _, item := range outbounds {
		ob := item.(map[string]any)
		if tag, ok := ob["tag"].(string); ok && len(tag) == 0 {
			t.Error("empty outbound tag")
		}
	}
}

func TestBuildSkipsUnsupportedTransport(t *testing.T) {
	servers := []*parser.Server{
		{ID: "srv_a", Host: "h", Port: 1, UUID: "u", Network: parser.NetworkTCP},
		{ID: "srv_x", Host: "h", Port: 1, UUID: "u", Network: parser.Network("kcp")},
	}
	_, unsupported, err := Build(BuildOptions{
		Lanes:    []Lane{{InboundTag: "in", GroupTag: "grp", Port: 1000}},
		Servers:  servers,
		ClashAPI: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(unsupported) != 1 || unsupported[0].ID != "srv_x" {
		t.Fatalf("unsupported = %v, want srv_x", unsupported)
	}
}

func TestBuildXHTTPTransport(t *testing.T) {
	srv := &parser.Server{
		ID: "srv_x", Name: "x", Host: "h", Port: 443, UUID: "u",
		Network: parser.NetworkXHTTP, Path: "/cvt/", HostHeader: "cdn.example", Mode: "auto",
		Security: parser.SecurityTLS, SNI: "cdn.example",
	}
	raw, unsupported, err := Build(BuildOptions{
		Lanes:    []Lane{{InboundTag: "in", GroupTag: "grp", Port: 1000}},
		Servers:  []*parser.Server{srv},
		ClashAPI: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, ob := range cfg.Outbounds {
		if ob["tag"] != "srv_x" {
			continue
		}
		tr, _ := ob["transport"].(map[string]any)
		if tr["type"] != "xhttp" || tr["path"] != "/cvt/" || tr["host"] != "cdn.example" {
			t.Fatalf("transport = %v", tr)
		}
		if tr["mode"] != "stream-one" {
			t.Fatalf("mode = %v, want stream-one", tr["mode"])
		}
		return
	}
	t.Fatal("xhttp outbound not found")
}

func TestBuildFailsWhenNothingIsUsable(t *testing.T) {
	_, _, err := Build(BuildOptions{
		Lanes:    []Lane{{InboundTag: "in", GroupTag: "grp", Port: 1000}},
		Servers:  []*parser.Server{{ID: "x", Host: "h", Port: 1, UUID: "u", Network: parser.Network("kcp")}},
		ClashAPI: "127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected error when every server is unsupported")
	}
}

func TestBuildTorrentBypassLane(t *testing.T) {
	servers := []*parser.Server{{ID: "srv_a", Host: "203.0.113.10", Port: 443, UUID: "u", Network: parser.NetworkTCP}}
	raw, _, err := Build(BuildOptions{
		Lanes: []Lane{
			{InboundTag: "lane-in-data-0", GroupTag: "lane-group-data-0", Port: 1000},
			{InboundTag: "lane-in-torrent-1", GroupTag: "lane-group-torrent-1", Port: 1001, TorrentDirect: true},
		},
		Servers:  servers,
		ClashAPI: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
		Route     struct {
			Rules []struct {
				Inbound  []string `json:"inbound"`
				Action   string   `json:"action"`
				Protocol string   `json:"protocol"`
				Outbound string   `json:"outbound"`
			} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hasDirect := false
	for _, outbound := range cfg.Outbounds {
		if outbound["type"] == "direct" && outbound["tag"] == "direct" {
			hasDirect = true
		}
	}
	if !hasDirect {
		t.Fatal("direct outbound is missing")
	}
	var sniffed, torrentDirect bool
	for _, rule := range cfg.Route.Rules {
		if len(rule.Inbound) != 1 || rule.Inbound[0] != "lane-in-torrent-1" {
			continue
		}
		if rule.Action == "sniff" {
			sniffed = true
		}
		if rule.Protocol == "bittorrent" && rule.Outbound == "direct" {
			torrentDirect = true
		}
	}
	if !sniffed || !torrentDirect {
		t.Fatalf("sniffed=%v torrentDirect=%v", sniffed, torrentDirect)
	}
}
