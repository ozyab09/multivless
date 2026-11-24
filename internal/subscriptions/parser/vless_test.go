package parser

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseRealityURI(t *testing.T) {
	uri := "vless://11111111-2222-4333-8444-555555555555@203.0.113.10:443" +
		"?security=reality&sni=example.com&fp=chrome&pbk=PUBKEY&sid=abcd" +
		"&type=tcp&flow=xtls-rprx-vision#Amsterdam%20One"

	srv, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if srv.Host != "203.0.113.10" || srv.Port != 443 {
		t.Errorf("host/port = %s/%d", srv.Host, srv.Port)
	}
	if srv.Security != SecurityReality {
		t.Errorf("security = %q", srv.Security)
	}
	if srv.SNI != "example.com" || srv.Fingerprint != "chrome" || srv.PublicKey != "PUBKEY" || srv.ShortID != "abcd" {
		t.Errorf("tls fields not parsed: %+v", srv)
	}
	if srv.Flow != "xtls-rprx-vision" {
		t.Errorf("flow = %q", srv.Flow)
	}
	if srv.Name != "Amsterdam One" {
		t.Errorf("name = %q", srv.Name)
	}
	if !strings.HasPrefix(srv.ID, "srv_") || len(srv.ID) != len("srv_")+12 {
		t.Errorf("unexpected id %q", srv.ID)
	}
	if strings.Contains(srv.ID, srv.UUID) {
		t.Error("id must not contain the UUID")
	}
}

func TestParseWebSocketURI(t *testing.T) {
	uri := "vless://11111111-2222-4333-8444-555555555555@example.org:8443" +
		"?security=tls&sni=example.org&type=ws&host=cdn.example.org&path=%2Fws#WS"

	srv, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if srv.Network != NetworkWS {
		t.Errorf("network = %q", srv.Network)
	}
	if srv.Path != "/ws" {
		t.Errorf("path = %q", srv.Path)
	}
	if srv.HostHeader != "cdn.example.org" {
		t.Errorf("host header = %q", srv.HostHeader)
	}
}

func TestServerIDSeparatesConnections(t *testing.T) {
	base := "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=tls&sni=example.org&type=ws&path=%2Fa#A"
	other := "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=tls&sni=example.org&type=ws&path=%2Fb#A"

	a, err := ParseURI(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseURI(other)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("servers with different paths must have different IDs")
	}

	renamed := strings.Replace(base, "#A", "#B", 1)
	c, err := ParseURI(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != c.ID {
		t.Fatal("renaming must not change the ID")
	}
}

func TestParseURIErrors(t *testing.T) {
	cases := map[string]string{
		"not vless":        "ss://foo",
		"missing uuid":     "vless://@example.org:443?security=tls&sni=example.org#x",
		"malformed uuid":   "vless://not-a-uuid@example.org:443?security=tls&sni=example.org#x",
		"missing port":     "vless://11111111-2222-4333-8444-555555555555@example.org?security=tls&sni=example.org#x",
		"bad port":         "vless://11111111-2222-4333-8444-555555555555@example.org:70000?security=tls&sni=example.org#x",
		"bad security":     "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=nonexistent#x",
		"bad transport":    "vless://11111111-2222-4333-8444-555555555555@example.org:443?type=kcp&security=tls&sni=e.org#x",
		"reality no pbk":   "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=reality&sni=e.org&fp=chrome#x",
		"tls no sni":       "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=tls#x",
		"grpc no service":  "vless://11111111-2222-4333-8444-555555555555@example.org:443?type=grpc&security=tls&sni=e.org#x",
		"ws path no slash": "vless://11111111-2222-4333-8444-555555555555@example.org:443?type=ws&security=tls&sni=e.org&path=no#x",
	}
	for name, uri := range cases {
		if _, err := ParseURI(uri); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseContentBase64SKipsBadLines(t *testing.T) {
	good := "vless://11111111-2222-4333-8444-555555555555@example.org:443?security=tls&sni=example.org#One"
	body := good + "\nbroken-line\n" + good
	encoded := base64.StdEncoding.EncodeToString([]byte(body))

	servers, errs := ParseContent([]byte(encoded))
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2 (duplicates are collapsed by the registry)", len(servers))
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %d, want 1: %v", len(errs), errs)
	}
}
