package geoip

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func varint(v uint64) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return buf[:n]
}

func pbVarint(field int, v uint64) []byte {
	out := varint(uint64(field<<3) | 0)
	return append(out, varint(v)...)
}

func pbBytes(field int, data []byte) []byte {
	out := varint(uint64(field<<3) | 2)
	out = append(out, varint(uint64(len(data)))...)
	return append(out, data...)
}

func pbString(field int, s string) []byte { return pbBytes(field, []byte(s)) }

func TestParseLookupIPv4(t *testing.T) {
	cidr := func(ip string, prefix uint64) []byte {
		msg := pbBytes(1, net.ParseIP(ip).To4())
		return append(msg, pbVarint(2, prefix)...)
	}
	entry := pbString(1, "PL")
	entry = append(entry, pbBytes(2, cidr("1.2.3.0", 24))...)
	entry = append(entry, pbBytes(2, cidr("10.0.0.0", 8))...)
	raw := pbBytes(1, entry)

	db, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		ip   string
		want string
	}{
		{"1.2.3.4", "PL"},
		{"1.2.3.255", "PL"},
		{"1.2.4.1", ""},
		{"10.9.9.9", "PL"},
		{"127.0.0.1", ""},
	}
	for _, tc := range cases {
		if got := db.CountryCode(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("CountryCode(%s) = %q, want %q", tc.ip, got, tc.want)
		}
	}
}

func TestLoadRealDatabaseIfPresent(t *testing.T) {
	path := filepath.Join("..", "..", "geoip.dat")
	if _, err := os.Stat(path); err != nil {
		t.Skip("geoip.dat is not downloaded")
	}
	db, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := db.CountryCode(net.ParseIP("8.8.8.8")); got != "US" {
		t.Fatalf("CountryCode(8.8.8.8) = %q, want US", got)
	}
}

func TestNilDatabase(t *testing.T) {
	var db *DB
	if got := db.CountryCode(net.ParseIP("1.2.3.4")); got != "" {
		t.Fatalf("nil DB returned %q", got)
	}
}
