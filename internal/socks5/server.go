// Package socks5 implements the SOCKS5 CONNECT gateway.
package socks5

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/multivless/multivless/internal/config"
)

// Principal identifies an authenticated SOCKS5 user. Anonymous clients have an
// empty Username and ID.
type Principal struct {
	ID             string
	Username       string
	MaxConnections int
	// BypassTorrent sends sniffed BitTorrent flows directly instead of through
	// the VLESS pool (uses dedicated lanes when the engine has them).
	BypassTorrent bool
}

// Authenticator verifies SOCKS5 credentials.
type Authenticator interface {
	Authenticate(username, password string) (*Principal, error)
}

// DialFunc opens target through the tunnel on behalf of a principal.
type DialFunc func(ctx context.Context, principal Principal, target string) (net.Conn, error)

// Gateway accepts SOCKS5 clients and forwards them through Dial.
type Gateway struct {
	cfg    config.SOCKS5
	auth   Authenticator
	dial   DialFunc
	logger *slog.Logger
	users  map[string]string
	slots  chan struct{}

	userMu     sync.Mutex
	userActive map[string]int

	listener net.Listener
}

// New creates a gateway. If auth is nil, users from the configuration are used
// (Stage 1 behaviour, kept for bootstrapping).
func New(cfg config.SOCKS5, auth Authenticator, dial DialFunc, logger *slog.Logger) *Gateway {
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
		return fmt.Errorf("socks5 listen on %s: %w", addr, err)
	}
	g.listener = l
	g.logger.Info("socks5 gateway listening", "addr", l.Addr().String(), "auth", g.cfg.Authentication)

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

func (g *Gateway) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(g.cfg.ConnectionTimeout.Std()))

	principal, err := g.handshake(conn)
	if err != nil {
		g.logger.Debug("socks5 handshake failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}

	target, err := g.readRequest(conn)
	if err != nil {
		g.logger.Debug("socks5 request rejected", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}

	if !g.acquireUser(principal) {
		g.logger.Warn("per-user connection limit reached",
			"user", principal.Username, "limit", principal.MaxConnections)
		_ = writeReply(conn, replyNotAllowed)
		return
	}
	defer g.releaseUser(principal)

	dialCtx := ctx
	if timeout := g.cfg.ConnectionTimeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	upstream, err := g.dial(dialCtx, principal, target)
	if err != nil {
		g.logger.Warn("cannot route connection",
			"user", principal.Username,
			"target", target,
			"error", err,
		)
		_ = writeReply(conn, replyGeneralFailure)
		return
	}
	defer upstream.Close()

	if err := writeReply(conn, replySucceeded); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	g.pipe(conn, upstream, principal.Username, target)
}

func (g *Gateway) acquireUser(p Principal) bool {
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

func (g *Gateway) releaseUser(p Principal) {
	if p.Username == "" || p.MaxConnections <= 0 {
		return
	}
	g.userMu.Lock()
	defer g.userMu.Unlock()
	if g.userActive[p.Username] > 0 {
		g.userActive[p.Username]--
	}
}

// handshake negotiates the auth method and verifies credentials.
func (g *Gateway) handshake(conn net.Conn) (Principal, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return Principal{}, err
	}
	if head[0] != 0x05 {
		return Principal{}, fmt.Errorf("unsupported socks version %d", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return Principal{}, err
	}

	wantMethod := byte(0x00)
	if g.cfg.Authentication {
		wantMethod = 0x02
	}
	offered := false
	for _, m := range methods {
		if m == wantMethod {
			offered = true
			break
		}
	}
	if !offered {
		_, _ = conn.Write([]byte{0x05, 0xFF})
		return Principal{}, fmt.Errorf("client offered no acceptable auth method")
	}
	if _, err := conn.Write([]byte{0x05, wantMethod}); err != nil {
		return Principal{}, err
	}
	if wantMethod == 0x00 {
		return Principal{}, nil
	}

	// RFC 1929 username/password sub-negotiation.
	ver := make([]byte, 2)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return Principal{}, err
	}
	if ver[0] != 0x01 {
		return Principal{}, fmt.Errorf("unsupported auth sub-negotiation version %d", ver[0])
	}
	user := make([]byte, int(ver[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return Principal{}, err
	}
	passLen := make([]byte, 1)
	if _, err := io.ReadFull(conn, passLen); err != nil {
		return Principal{}, err
	}
	pass := make([]byte, int(passLen[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return Principal{}, err
	}

	if g.auth != nil {
		principal, err := g.auth.Authenticate(string(user), string(pass))
		if err != nil {
			_, _ = conn.Write([]byte{0x01, 0x01})
			return Principal{}, err
		}
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return Principal{}, err
		}
		return *principal, nil
	}

	expected, ok := g.users[string(user)]
	if ok && subtle.ConstantTimeCompare([]byte(expected), pass) == 1 {
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return Principal{}, err
		}
		return Principal{Username: string(user)}, nil
	}
	_, _ = conn.Write([]byte{0x01, 0x01})
	return Principal{}, fmt.Errorf("authentication failed")
}

// readRequest parses a CONNECT request and returns host:port.
func (g *Gateway) readRequest(conn net.Conn) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return "", err
	}
	if head[0] != 0x05 {
		return "", fmt.Errorf("unsupported socks version %d", head[0])
	}
	if head[1] != 0x01 {
		_ = writeReply(conn, replyCommandNotSupported)
		return "", fmt.Errorf("unsupported command 0x%02x (only CONNECT)", head[1])
	}
	if head[2] != 0x00 {
		return "", fmt.Errorf("unsupported reserved byte 0x%02x", head[2])
	}

	var host string
	switch head[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", err
		}
		buf := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		host = string(buf)
	default:
		return "", fmt.Errorf("unsupported address type 0x%02x", head[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	if host == "" || port == 0 {
		_ = writeReply(conn, replyGeneralFailure)
		return "", fmt.Errorf("invalid target %s:%d", host, port)
	}

	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// pipe copies both directions until either side closes.
func (g *Gateway) pipe(client, upstream net.Conn, username, target string) {
	idle := g.cfg.IdleTimeout.Std()
	var wg sync.WaitGroup
	wg.Add(2)

	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(writerWithIdle(dst, idle), readerWithIdle(src, idle))
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}

	go copyOne(upstream, client)
	go copyOne(client, upstream)
	wg.Wait()

	g.logger.Debug("connection closed", "user", username, "target", target)
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

func readerWithIdle(c net.Conn, timeout time.Duration) io.Reader {
	return idleReader{Conn: c, timeout: timeout}
}
func writerWithIdle(c net.Conn, timeout time.Duration) io.Writer {
	return idleWriter{Conn: c, timeout: timeout}
}

// SOCKS5 reply codes.
const (
	replySucceeded           = 0x00
	replyGeneralFailure      = 0x01
	replyNotAllowed          = 0x02
	replyCommandNotSupported = 0x07
)

func writeReply(conn net.Conn, code byte) error {
	_, err := conn.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}
