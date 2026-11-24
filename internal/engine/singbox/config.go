// Package singbox generates sing-box configuration and supervises the process.
//
// Outbound selection works through per-lane SOCKS inbounds: every lane has its
// own selector group in sing-box, and the gateway switches the group's chosen
// member through the Clash API immediately before dialling the lane. A lane is
// used by one connection at a time, which makes the "select then dial" pair
// race-free without touching sing-box internals.
package singbox

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

// Lane is one SOCKS inbound inside sing-box plus the selector group that
// routes it.
type Lane struct {
	InboundTag string
	GroupTag   string
	Port       int
	// TorrentDirect sniffs the lane and sends BitTorrent flows to the direct
	// outbound, leaving every other protocol on the selector group.
	TorrentDirect bool
}

// BuildOptions describes the generated configuration.
type BuildOptions struct {
	Lanes    []Lane
	Servers  []*parser.Server
	ClashAPI string
	LogLevel string
}

type sbLog struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type sbInbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
}

type sbRouteRule struct {
	Inbound  []string `json:"inbound,omitempty"`
	Action   string   `json:"action,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
	Outbound string   `json:"outbound,omitempty"`
}

type sbRoute struct {
	Rules []sbRouteRule `json:"rules"`
}

type sbClashAPI struct {
	ExternalController string `json:"external_controller"`
}

type sbExperimental struct {
	ClashAPI sbClashAPI `json:"clash_api"`
}

type sbConfig struct {
	Log          sbLog          `json:"log"`
	Inbounds     []sbInbound    `json:"inbounds"`
	Outbounds    []any          `json:"outbounds"`
	Route        sbRoute        `json:"route"`
	Experimental sbExperimental `json:"experimental"`
}

// Build renders the sing-box configuration. Servers whose transport is not
// supported by this sing-box generation are skipped and returned in unsupported
// so the caller can mark them as ERROR instead of failing the whole gateway.
func Build(opts BuildOptions) ([]byte, []*parser.Server, error) {
	if len(opts.Lanes) == 0 {
		return nil, nil, fmt.Errorf("no lanes configured")
	}
	if len(opts.Servers) == 0 {
		return nil, nil, fmt.Errorf("no servers configured")
	}

	cfg := sbConfig{
		Log:   sbLog{Level: defaultString(opts.LogLevel, "warn"), Timestamp: true},
		Route: sbRoute{},
		Experimental: sbExperimental{
			ClashAPI: sbClashAPI{ExternalController: opts.ClashAPI},
		},
	}

	for _, lane := range opts.Lanes {
		cfg.Inbounds = append(cfg.Inbounds, sbInbound{
			Type:       "socks",
			Tag:        lane.InboundTag,
			Listen:     "127.0.0.1",
			ListenPort: lane.Port,
		})
		if lane.TorrentDirect {
			// Sniff first, then divert only BitTorrent; everything else keeps
			// using the fail-closed VLESS selector.
			cfg.Route.Rules = append(cfg.Route.Rules,
				sbRouteRule{Inbound: []string{lane.InboundTag}, Action: "sniff"},
				sbRouteRule{Inbound: []string{lane.InboundTag}, Protocol: "bittorrent", Outbound: "direct"},
			)
		}
		cfg.Route.Rules = append(cfg.Route.Rules, sbRouteRule{
			Inbound:  []string{lane.InboundTag},
			Outbound: lane.GroupTag,
		})
	}

	var (
		unsupported []*parser.Server
		tags        []string
	)
	for _, srv := range opts.Servers {
		outbound, err := outboundFor(srv)
		if err != nil {
			unsupported = append(unsupported, srv)
			continue
		}
		cfg.Outbounds = append(cfg.Outbounds, outbound)
		tags = append(tags, srv.ID)
	}
	if len(tags) == 0 {
		return nil, unsupported, fmt.Errorf("no usable servers after filtering unsupported transports")
	}

	for _, lane := range opts.Lanes {
		cfg.Outbounds = append(cfg.Outbounds, map[string]any{
			"type":      "selector",
			"tag":       lane.GroupTag,
			"outbounds": append([]string(nil), tags...),
			"default":   tags[0],
		})
	}
	for _, lane := range opts.Lanes {
		if lane.TorrentDirect {
			cfg.Outbounds = append(cfg.Outbounds, map[string]any{"type": "direct", "tag": "direct"})
			break
		}
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, unsupported, fmt.Errorf("marshal sing-box config: %w", err)
	}
	return raw, unsupported, nil
}

func outboundFor(s *parser.Server) (map[string]any, error) {
	out := map[string]any{
		"type":        "vless",
		"tag":         s.ID,
		"server":      s.Host,
		"server_port": s.Port,
		"uuid":        s.UUID,
	}
	if s.Flow != "" {
		out["flow"] = s.Flow
	}
	if s.PacketEncoding != "" {
		out["packet_encoding"] = s.PacketEncoding
	}
	if tls := tlsOptions(s); tls != nil {
		out["tls"] = tls
	}
	transport, err := transportOptions(s)
	if err != nil {
		return nil, err
	}
	if transport != nil {
		out["transport"] = transport
	}
	return out, nil
}

func tlsOptions(s *parser.Server) map[string]any {
	if s.Security != parser.SecurityTLS && s.Security != parser.SecurityReality {
		return nil
	}
	tls := map[string]any{
		"enabled":     true,
		"server_name": s.SNI,
	}
	if len(s.ALPN) > 0 {
		tls["alpn"] = s.ALPN
	}
	if s.Fingerprint != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": s.Fingerprint}
	}
	if s.Security == parser.SecurityReality {
		tls["reality"] = map[string]any{
			"enabled":    true,
			"public_key": s.PublicKey,
			"short_id":   s.ShortID,
		}
	}
	return tls
}

func transportOptions(s *parser.Server) (map[string]any, error) {
	switch s.Network {
	case parser.NetworkTCP:
		return nil, nil
	case parser.NetworkWS:
		tr := map[string]any{"type": "ws", "path": s.Path}
		if s.HostHeader != "" {
			tr["headers"] = map[string]string{"Host": s.HostHeader}
		}
		return tr, nil
	case parser.NetworkGRPC:
		return map[string]any{"type": "grpc", "service_name": s.ServiceName}, nil
	case parser.NetworkHTTPUpgrade:
		tr := map[string]any{"type": "httpupgrade", "path": s.Path}
		if s.HostHeader != "" {
			tr["host"] = s.HostHeader
		}
		return tr, nil
	case parser.NetworkXHTTP:
		// XHTTP needs the sing-box-lx build (with_xhttp); upstream sing-box
		// rejects the transport entirely.
		tr := map[string]any{
			"type": "xhttp",
			"path": defaultString(s.Path, "/"),
			"mode": xhttpMode(s.Mode),
		}
		if s.HostHeader != "" {
			tr["host"] = s.HostHeader
		}
		return tr, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", s.Network)
	}
}

// xhttpMode normalises the link's mode for sing-box-lx. The Xray "auto" mode
// makes this build pick a download mode that common Xray servers answer with
// HTTP 400; stream-one works against them and is what auto resolves to on the
// server side in practice.
func xhttpMode(mode string) string {
	switch strings.ToLower(mode) {
	case "", "auto":
		return "stream-one"
	default:
		return mode
	}
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
