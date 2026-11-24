// Package geoip reads the v2fly/Xray geoip.dat database and maps IP
// addresses to ISO country codes. Only IPv4 ranges are indexed: those are what
// DNS answers for proxied targets almost always contain and what the flag
// lookup needs.
package geoip

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
)

// DB is a parsed geoip database.
type DB struct {
	ranges []ipRange
}

type ipRange struct {
	start uint32
	end   uint32
	code  string
}

// Load reads and parses a geoip.dat file.
func Load(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("geoip path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes a GeoIPList protobuf message.
func Parse(raw []byte) (*DB, error) {
	db := &DB{}
	err := walk(raw, func(field int, wire int, data []byte, _ uint64) error {
		if field != 1 || wire != 2 {
			return nil
		}
		return parseGeoIP(data, db)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(db.ranges, func(i, j int) bool {
		if db.ranges[i].start == db.ranges[j].start {
			return db.ranges[i].end < db.ranges[j].end
		}
		return db.ranges[i].start < db.ranges[j].start
	})
	return db, nil
}

func parseGeoIP(raw []byte, db *DB) error {
	var code string
	var cidrs [][]byte
	err := walk(raw, func(field int, wire int, data []byte, _ uint64) error {
		switch {
		case field == 1 && wire == 2:
			code = string(data)
		case field == 2 && wire == 2:
			cidrs = append(cidrs, data)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if code == "" {
		return nil
	}
	for _, rawCIDR := range cidrs {
		var ip []byte
		var prefix uint64
		err := walk(rawCIDR, func(field int, wire int, data []byte, value uint64) error {
			switch {
			case field == 1 && wire == 2:
				ip = data
			case field == 2 && wire == 0:
				prefix = value
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(ip) != 4 || prefix > 32 {
			continue
		}
		start := binary.BigEndian.Uint32(ip)
		end := start | maskBits(32-prefix)
		db.ranges = append(db.ranges, ipRange{start: start, end: end, code: code})
	}
	return nil
}

func maskBits(bits uint64) uint32 {
	if bits == 0 {
		return 0
	}
	if bits >= 32 {
		return ^uint32(0)
	}
	return (uint32(1) << bits) - 1
}

// CountryCode returns the ISO code of the range containing ip, or "".
func (d *DB) CountryCode(ip net.IP) string {
	if d == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	target := binary.BigEndian.Uint32(v4)
	idx := sort.Search(len(d.ranges), func(i int) bool {
		return d.ranges[i].start > target
	})
	if idx == 0 {
		return ""
	}
	r := d.ranges[idx-1]
	if target > r.end {
		return ""
	}
	return r.code
}

// walk iterates over a protobuf message, invoking fn for every field. Length
// delimited fields are passed as data; varints as value.
func walk(raw []byte, fn func(field int, wire int, data []byte, value uint64) error) error {
	for len(raw) > 0 {
		key, n := binary.Uvarint(raw)
		if n <= 0 {
			return fmt.Errorf("invalid protobuf key")
		}
		raw = raw[n:]
		field := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			value, n := binary.Uvarint(raw)
			if n <= 0 {
				return fmt.Errorf("invalid protobuf varint")
			}
			if err := fn(field, wire, nil, value); err != nil {
				return err
			}
			raw = raw[n:]
		case 2:
			size, n := binary.Uvarint(raw)
			if n <= 0 || size > uint64(len(raw)-n) {
				return fmt.Errorf("invalid protobuf length")
			}
			data := raw[n : n+int(size)]
			if err := fn(field, wire, data, 0); err != nil {
				return err
			}
			raw = raw[n+int(size):]
		case 1:
			if len(raw) < 8 {
				return io.ErrUnexpectedEOF
			}
			raw = raw[8:]
		case 5:
			if len(raw) < 4 {
				return io.ErrUnexpectedEOF
			}
			raw = raw[4:]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
	}
	return nil
}
