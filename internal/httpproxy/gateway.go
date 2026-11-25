// Package httpproxy implements an HTTP CONNECT proxy (with optional Basic Auth)
// wired through the same router/balancer as SOCKS5.
package httpproxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/socks5"
)

// Gateway listens for HTTP connections and forwards CONNECT requests.
type Gateway struct {
	cfg    config.HTTPProxy
	auth   socks5.Authenticator
	dial   socks5.DialFunc
	logger *slog.Logger
	users  map[string]string
	slots  chan struct{}

	userMu     sync.Mutex
	userActive map[string]int

	listener net.Listener
}

// New creates a gateway. If auth is nil, users from the configuration are used.
func New(cfg config.HTTPProxy, auth socks5.Authenticator, dial socks5.DialFunc, logger *slog.Logger) *Gateway {
	users := make(map[string]string, len(cfg.Users))
	for _, u := range cfg.Users {
		users[u.Username] = u.Password
	}
	return &Gateway{
		cfg:        cfg,
		auth:       auth,
		dial:       dial,
		logger:     logger,
		users:      users,
		slots:      make(chan struct{}, cfg.MaxConnections),
		userActive: make(map[string]int),
	}
}

// Listen opens the configured address and starts serving.
func (g *Gateway) Listen(ctx context.Context) error {
	addr := net.JoinHostPort(g.cfg.Listen, strconv.Itoa(g.cfg.Port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http proxy listen on %s: %w", addr, err)
	}
	g.listener = l
	g.logger.Info("http proxy listening", "addr", l.Addr().String(), "auth", g.cfg.Authentication)

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}

		select {
		case g.slots <- struct{}{}:
		default:
			g.logger.Warn("connection limit reached, rejecting client", "remote", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}

		go func() {
			defer func() { <-g.slots }()
			g.handle(ctx, conn)
		}()
	}
}

// Addr returns the bound listener address.
func (g *Gateway) Addr() string {
	if g.listener == nil {
		return ""
	}
	return g.listener.Addr().String()
}

// ParseProxyAuth parses Proxy-Authorization header value, returning (username, password).
func ParseProxyAuth(value string) (string, string) {
	if !strings.HasPrefix(value, "Basic ") {
		return "", ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "Basic "))
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

func (g *Gateway) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(g.cfg.ConnectionTimeout.Std()))

	principal, ok := g.authenticate(conn)

	// At this point the reader may have consumed some header bytes if auth wasn't found.
	// We buffer all remaining input to be safe.
	bufReader := bufio.NewReaderSize(conn, 4096)

	// Read request line + headers into our buffer.
	reqLine, err := bufReader.ReadString('\n')
	if err != nil {
		g.logger.Debug("http read request failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}
	reqLine = strings.TrimSpace(reqLine)

	method, path, err := parseRequestLine(reqLine)
	if err != nil {
		g.writeResponse(conn, 400, "Bad Request")
		return
	}

	// Read remaining headers (everything until empty line).
	headersRaw, err := bufReader.ReadString('\n')
	if err != nil && err != io.EOF {
		g.logger.Debug("http read headers failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}

	// Collect headers into a map for Host lookup.
	headerMap := make(httpHeaders)
	parseHeaders(headersRaw, headerMap)

	hostFromHeaders := headerMap.Get("Host")

	target := resolveTarget(path, hostFromHeaders, method)

	// Authenticate if required but client didn't provide credentials.
	if !ok && g.cfg.Authentication {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		g.writeResponse(conn, 407, "Unauthorized")
		_ = conn.Close()
		return
	}

	switch method {
	case "CONNECT":
		if !g.acquireUser(principal) {
			g.writeResponse(conn, 429, "Too Many Connections")
			return
		}
		defer g.releaseUser(principal)

		err = g.handleConnect(ctx, bufReader, conn, principal, target)

		if err != nil {
			g.logger.Warn("connect failed",
				"user", principal.Username,
				"target", target,
				"error", err,
			)
		}
		_ = conn.Close()

	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		// Transparent HTTP proxy: relay body over the tunnel.
		_ = conn.SetReadDeadline(time.Time{})
		if !g.acquireUser(principal) {
			g.writeResponse(conn, 429, "Too Many Connections")
			return
		}
		defer g.releaseUser(principal)

		err = g.handleHTTPRequest(ctx, bufReader, conn, principal, target, method, path)
		if err != nil {
			g.logger.Warn("http forward failed",
				"user", principal.Username,
				"target", target,
				"method", method,
				"error", err,
			)
		}
		_ = conn.Close()

	default:
		g.writeResponse(conn, 501, "Not Implemented")
	}
}

// parseRequestLine extracts method and path from the raw request line.
func parseRequestLine(line string) (method, path string, err error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("malformed request line")
	}
	method = parts[0]
	path = parts[1]
	return method, path, nil
}

// httpHeaders is a case-insensitive map of header name → values.
type httpHeaders map[string][]string

// Get returns the first value for key (case-insensitive).
func (h httpHeaders) Get(key string) string {
	vs := h[strings.ToLower(key)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func parseHeaders(raw string, m httpHeaders) {
	for _, h := range strings.Split(raw, "\n") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		kv := strings.SplitN(h, ": ", 2)
		if len(kv) == 2 {
			key := strings.ToLower(kv[0])
			m[key] = append(m[key], kv[1])
		}
	}
}

func resolveTarget(path, hostHeader, method string) string {
	// CONNECT uses absolute form: CONNECT host:port HTTP/1.1
	if method == "CONNECT" {
		// path is like "example.com:443 HTTP/1.1" — strip trailing version.
		parts := strings.Fields(path)
		if len(parts) >= 1 {
			addr := parts[0]
			// Ensure port suffix exists.
			if _, _, err := net.SplitHostPort(addr); err != nil {
				addr = net.JoinHostPort(addr, "443")
			}
			return addr
		}
		return path
	}

	// Non-CONNECT: extract from Host header.
	if hostHeader != "" {
		hostOnly, _, err := net.SplitHostPort(hostHeader)
		if err != nil {
			return net.JoinHostPort(hostHeader, "80")
		}
		return net.JoinHostPort(hostOnly, "80")
	}

	return ""
}

func (g *Gateway) acquireUser(p socks5.Principal) bool {
	if p.Username == "" || p.MaxConnections <= 0 {
		return true
	}
	g.userMu.Lock()
	defer g.userMu.Unlock()
	if g.userActive[p.Username] >= p.MaxConnections {
		return false
	}
	g.userActive[p.Username]++
	return true
}

func (g *Gateway) releaseUser(p socks5.Principal) {
	if p.Username == "" || p.MaxConnections <= 0 {
		return
	}
	g.userMu.Lock()
	defer g.userMu.Unlock()
	if g.userActive[p.Username] > 0 {
		g.userActive[p.Username]--
	}
}

// authenticate checks Proxy-Authorization header.
func (g *Gateway) authenticate(conn net.Conn) (socks5.Principal, bool) {
	if !g.cfg.Authentication {
		return socks5.Principal{}, false
	}

	reader := bufio.NewReaderSize(conn, 4096)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return socks5.Principal{}, false
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return socks5.Principal{}, false
		}

		// Check for Proxy-Authorization header (case-insensitive prefix check).
		lower := strings.ToLower(line[:len(line)-1]) // trim \r\n
		if strings.HasPrefix(lower, "proxy-authorization:") || strings.HasPrefix(lower, "proxy-auth:") {
			value := strings.TrimSpace(line[22:]) // trim prefix + space
			username, password := ParseProxyAuth(value)
			if username != "" {
				if g.auth != nil {
					p, err := g.auth.Authenticate(username, password)
					if err == nil {
						return *p, true
					}
				} else if pw, ok := g.users[username]; ok && subtle.ConstantTimeCompare([]byte(pw), []byte(password)) == 1 {
					return socks5.Principal{Username: username}, true
				}
			}
			break // invalid auth — fallthrough to reject after headers
		}
	}

	return socks5.Principal{Username: ""}, false
}

// handleConnect processes an HTTP CONNECT request: establishes tunnel through the proxy.
func (g *Gateway) handleConnect(ctx context.Context, buf *bufio.Reader, conn net.Conn, principal socks5.Principal, target string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialCtx := ctx
	if timeout := g.cfg.ConnectionTimeout.Std(); timeout > 0 {
		var dcl context.CancelFunc
		dialCtx, dcl = context.WithTimeout(ctx, timeout)
		defer dcl()
	}

	upstream, err := g.dial(dialCtx, principal, target)
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		g.writeResponse(conn, 502, "Bad Gateway")
		return err
	}
	defer upstream.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	g.writeResponse(conn, 200, "Connection Established")

	// Reset deadline; pass full control to pipe.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	_ = upstream.SetReadDeadline(time.Time{})
	_ = upstream.SetWriteDeadline(time.Time{})

	g.pipe(conn, upstream)
	return nil
}

// handleHTTPRequest relays a regular HTTP request through the tunnel.
func (g *Gateway) handleHTTPRequest(ctx context.Context, buf *bufio.Reader, conn net.Conn, principal socks5.Principal, target string, method, path string) error {
	dialCtx := ctx
	if timeout := g.cfg.ConnectionTimeout.Std(); timeout > 0 {
		var dcl context.CancelFunc
		dialCtx, dcl = context.WithTimeout(ctx, timeout)
		defer dcl()
	}

	upstream, err := g.dial(dialCtx, principal, target)
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		g.writeResponse(conn, 502, "Bad Gateway")
		return err
	}
	defer upstream.Close()

	// Forward the original request line.
	_, _ = upstream.Write([]byte(fmt.Sprintf("%s %s HTTP/1.1\r\n", method, path)))

	// Copy the headers that were already buffered (after the initial request line).
	_, _ = buf.WriteTo(upstream)

	// Relay response back.
	idle := g.cfg.IdleTimeout.Std()
	copyWithIdle(upstream, conn, idle)
	return nil
}

func (g *Gateway) pipe(client, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}

	go copyOne(upstream, client)
	go copyOne(client, upstream)
	wg.Wait()
}

func copyWithIdle(dst, src net.Conn, idle time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)

	copyOne := func(d, s net.Conn) {
		defer wg.Done()
		r := idleReader{Conn: s, timeout: idle}
		w := idleWriter{Conn: d, timeout: idle}
		_, _ = io.Copy(w, r)
	}

	go copyOne(dst, src)
	go copyOne(src, dst)
	wg.Wait()
}

type idleReader struct {
	net.Conn
	timeout time.Duration
}

func (c idleReader) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(p)
}

type idleWriter struct {
	net.Conn
	timeout time.Duration
}

func (c idleWriter) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(p)
}

func (g *Gateway) writeResponse(conn net.Conn, status int, reason string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", status, reason)
	_, _ = conn.Write([]byte(resp))
}
