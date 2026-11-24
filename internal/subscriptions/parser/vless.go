// Package parser converts VLESS URIs (and subscription bodies) into server
// definitions. Secrets are never included in derived identifiers or error text.
package parser

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Security is the transport security layer of a VLESS connection.
type Security string

const (
	SecurityNone    Security = "none"
	SecurityTLS     Security = "tls"
	SecurityReality Security = "reality"
)

// Network is the VLESS transport.
type Network string

const (
	NetworkTCP         Network = "tcp"
	NetworkWS          Network = "ws"
	NetworkGRPC        Network = "grpc"
	NetworkHTTPUpgrade Network = "httpupgrade"
	NetworkXHTTP       Network = "xhttp"
)

// Server is a single VLESS outbound definition.
type Server struct {
	ID   string
	Name string

	Host string
	Port int
	UUID string

	Encryption     string
	Flow           string
	PacketEncoding string

	Security    Security
	SNI         string
	Fingerprint string
	PublicKey   string
	ShortID     string
	ALPN        []string

	Network     Network
	HostHeader  string
	Path        string
	ServiceName string
	Mode        string

	Fragment string

	// Weight is the operator-defined balancing weight (default 100).
	Weight int
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a canonical RFC 4122 UUID.
func IsUUID(s string) bool { return uuidPattern.MatchString(s) }

// ParseURI parses a single vless:// URI.
func ParseURI(raw string) (*Server, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
	if raw == "" {
		return nil, fmt.Errorf("empty vless URI")
	}
	if !strings.HasPrefix(strings.ToLower(raw), "vless://") {
		return nil, fmt.Errorf("not a vless URI")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed vless URI: %w", err)
	}
	if strings.ToLower(u.Scheme) != "vless" {
		return nil, fmt.Errorf("unexpected scheme %q", u.Scheme)
	}

	uuid := u.User.Username()
	if uuid == "" {
		return nil, fmt.Errorf("vless URI has no UUID")
	}
	if !IsUUID(uuid) {
		// The UUID is a secret; never echo it in the error.
		return nil, fmt.Errorf("vless URI has a malformed UUID")
	}

	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("vless URI has no host")
	}
	if net.ParseIP(host) == nil {
		if strings.ContainsAny(host, " /?#") {
			return nil, fmt.Errorf("vless URI has an invalid host")
		}
	}
	portRaw := u.Port()
	if portRaw == "" {
		return nil, fmt.Errorf("vless URI has no port")
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("vless URI has an invalid port %q", portRaw)
	}

	q := u.Query()

	srv := &Server{
		Host:           host,
		Port:           port,
		UUID:           uuid,
		Encryption:     defaultString(q.Get("encryption"), "none"),
		Flow:           q.Get("flow"),
		PacketEncoding: firstNonEmpty(q.Get("packetEncoding"), q.Get("packet_encoding")),
		Fingerprint:    q.Get("fp"),
		PublicKey:      firstNonEmpty(q.Get("pbk"), q.Get("publicKey")),
		ShortID:        firstNonEmpty(q.Get("sid"), q.Get("shortId")),
		HostHeader:     q.Get("host"),
		Path:           q.Get("path"),
		ServiceName:    firstNonEmpty(q.Get("serviceName"), q.Get("service_name")),
		Mode:           firstNonEmpty(q.Get("mode"), q.Get("xhttpMode")),
		Fragment:       q.Get("fragment"),
		SNI:            firstNonEmpty(q.Get("sni"), q.Get("peer")),
		Weight:         100,
	}
	if u.Fragment != "" {
		if name, err := url.PathUnescape(u.Fragment); err == nil {
			srv.Name = name
		} else {
			srv.Name = u.Fragment
		}
	}
	if srv.Name == "" {
		srv.Name = fmt.Sprintf("%s:%d", host, port)
	}

	switch sec := Security(strings.ToLower(defaultString(q.Get("security"), "none"))); sec {
	case "", SecurityNone:
		srv.Security = SecurityNone
	case SecurityTLS:
		srv.Security = SecurityTLS
	case SecurityReality:
		srv.Security = SecurityReality
	default:
		return nil, fmt.Errorf("unsupported security %q", sec)
	}

	switch netw := Network(strings.ToLower(defaultString(q.Get("type"), "tcp"))); netw {
	case "", NetworkTCP:
		srv.Network = NetworkTCP
	case NetworkWS:
		srv.Network = NetworkWS
	case NetworkGRPC:
		srv.Network = NetworkGRPC
	case NetworkHTTPUpgrade:
		srv.Network = NetworkHTTPUpgrade
	case NetworkXHTTP:
		srv.Network = NetworkXHTTP
	default:
		return nil, fmt.Errorf("unsupported transport %q", netw)
	}

	if alpn := q.Get("alpn"); alpn != "" {
		for _, part := range strings.Split(alpn, ",") {
			if p := strings.TrimSpace(part); p != "" {
				srv.ALPN = append(srv.ALPN, p)
			}
		}
	}

	if err := srv.validateTransport(); err != nil {
		return nil, err
	}

	srv.ID = srv.deriveID()
	return srv, nil
}

func (s *Server) validateTransport() error {
	if s.Security == SecurityReality {
		if s.PublicKey == "" {
			return fmt.Errorf("reality requires pbk")
		}
		if s.Fingerprint == "" {
			return fmt.Errorf("reality requires fp")
		}
		if s.SNI == "" && s.HostHeader == "" {
			return fmt.Errorf("reality requires sni or host")
		}
	}
	if s.Security == SecurityTLS && s.SNI == "" {
		return fmt.Errorf("tls requires sni")
	}
	switch s.Network {
	case NetworkWS, NetworkHTTPUpgrade:
		if s.Path == "" {
			s.Path = "/"
		}
		if !strings.HasPrefix(s.Path, "/") {
			return fmt.Errorf("ws/httpupgrade path must start with /")
		}
	case NetworkGRPC:
		if s.ServiceName == "" {
			return fmt.Errorf("grpc requires serviceName")
		}
	case NetworkXHTTP:
		if s.Path == "" {
			s.Path = "/"
		}
	}
	if s.Network != NetworkTCP && s.Flow != "" && s.Flow != "xtls-rprx-vision" {
		return fmt.Errorf("flow %q is only valid with xtls-rprx-vision", s.Flow)
	}
	return nil
}

// deriveID returns a stable identifier that depends only on connection
// parameters. Names and weights can change without changing identity.
func (s *Server) deriveID() string {
	h := sha256.New()
	for _, part := range []string{
		strings.ToLower(s.Host),
		strconv.Itoa(s.Port),
		s.UUID,
		string(s.Security),
		string(s.Network),
		s.SNI,
		s.HostHeader,
		s.Path,
		s.ServiceName,
		s.Flow,
		s.PublicKey,
		s.ShortID,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "srv_" + hex.EncodeToString(h.Sum(nil))[:12]
}

// ParseContent parses a subscription body or a plain list of URIs. Content may
// be a base64-encoded subscription. Invalid entries are reported individually
// and do not abort parsing of the remaining entries.
func ParseContent(content []byte) ([]*Server, []error) {
	text := strings.TrimSpace(string(content))
	if text == "" {
		return nil, []error{fmt.Errorf("empty content")}
	}

	// Subscription bodies are commonly base64 of the plain URI list.
	if !strings.HasPrefix(strings.ToLower(text), "vless://") && !strings.Contains(text, "://") {
		if decoded, ok := decodeBase64(text); ok {
			text = strings.TrimSpace(decoded)
		}
	}

	var (
		servers []*Server
		errs    []error
	)
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		srv, err := ParseURI(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
			continue
		}
		servers = append(servers, srv)
	}
	if len(servers) == 0 && len(errs) == 0 {
		return nil, []error{fmt.Errorf("no vless entries found")}
	}
	return servers, errs
}

func decodeBase64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := enc.DecodeString(s); err == nil && len(out) > 0 {
			return string(out), true
		}
	}
	return "", false
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
