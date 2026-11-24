// Package subscriptions downloads, parses and reconciles subscription sources.
package subscriptions

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/multivless/multivless/internal/storage"
	"github.com/multivless/multivless/internal/subscriptions/parser"
)

// Options configures the subscription manager.
type Options struct {
	Timeout           time.Duration
	MaxSize           int64
	AllowPrivateHosts bool
	UserAgent         string
}

// Applier is invoked after persisted servers change so the engine can reload.
type Applier func(ctx context.Context) error

// Manager updates subscriptions and reconciles their servers into the database.
type Manager struct {
	store    *storage.Store
	logger   *slog.Logger
	opts     Options
	client   *http.Client
	apply    Applier
	updateMu sync.Mutex
	// hwid is a stable per-installation device id. Panels with HWID binding
	// (Remnawave and friends) serve real configs only when it is sent.
	hwid string
}

// New creates a subscription manager.
func New(store *storage.Store, opts Options, apply Applier, logger *slog.Logger) *Manager {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxSize <= 0 {
		opts.MaxSize = 2 << 20
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "multivless/2"
	}
	m := &Manager{store: store, opts: opts, apply: apply, logger: logger}
	if store != nil {
		m.hwid = loadOrCreateHWID(store, logger)
	}
	m.client = &http.Client{
		Timeout: opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			_, err := m.validateURL(req.Context(), req.URL.String())
			return err
		},
	}
	return m
}

// Result describes one subscription update.
type Result struct {
	SubscriptionID string `json:"subscription_id"`
	Name           string `json:"name"`
	ServerCount    int    `json:"server_count"`
	Added          int    `json:"added"`
	Removed        int    `json:"removed"`
	Updated        int    `json:"updated"`
	NotModified    bool   `json:"not_modified"`
	Error          string `json:"error,omitempty"`
}

// Update refreshes one subscription. The last valid server set is preserved if
// the download or parsing fails.
func (m *Manager) Update(ctx context.Context, sub storage.Subscription) Result {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	res := Result{SubscriptionID: sub.ID, Name: sub.Name}

	parsedURL, err := m.validateURL(ctx, sub.URL)
	if err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", m.opts.UserAgent)
	req.Header.Set("Accept", "text/plain, */*")
	if m.hwid != "" {
		req.Header.Set("X-HWID", m.hwid)
	}
	if sub.ETag != "" {
		req.Header.Set("If-None-Match", sub.ETag)
	}
	if sub.LastModified != "" {
		req.Header.Set("If-Modified-Since", sub.LastModified)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		res.ServerCount = sub.ServerCount
		_ = m.store.RecordSubscriptionResult(sub.ID, sub.ETag, sub.LastModified, "", sub.ServerCount, true)
		m.logger.Debug("subscription not modified", "subscription_id", sub.ID, "name", sub.Name)
		return res
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("subscription returned %s", resp.Status)
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, m.opts.MaxSize+1))
	if err != nil {
		err = fmt.Errorf("read subscription body: %w", err)
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	if int64(len(body)) > m.opts.MaxSize {
		err := fmt.Errorf("subscription body exceeds %d bytes", m.opts.MaxSize)
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}

	infos, parseErrs := parser.ParseContent(body)
	for _, perr := range parseErrs {
		m.logger.Warn("subscription entry skipped", "subscription_id", sub.ID, "error", perr)
	}
	if len(infos) == 0 {
		err := fmt.Errorf("subscription contains no valid vless servers")
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	if allPlaceholders(infos) {
		err := fmt.Errorf("subscription returned a placeholder instead of servers (HWID rejected or device limit reached)")
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}

	existing, err := m.store.ListServers()
	if err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	previous := map[string]storage.Server{}
	for _, srv := range existing {
		if srv.SourceID == sub.ID {
			previous[srv.Info.ID] = srv
		}
	}

	records := make([]storage.Server, 0, len(infos))
	seen := map[string]bool{}
	keep := make([]string, 0, len(infos))
	for _, info := range infos {
		if info == nil || seen[info.ID] {
			continue
		}
		seen[info.ID] = true
		keep = append(keep, info.ID)

		record := storage.Server{Info: info, SourceID: sub.ID, Enabled: true, Weight: 100}
		if old, ok := previous[info.ID]; ok {
			record.Enabled = old.Enabled
			record.Weight = old.Weight
			if old.Info.Host != info.Host || old.Info.Port != info.Port || old.Info.Name != info.Name {
				res.Updated++
			}
		} else {
			res.Added++
		}
		records = append(records, record)
	}
	for id := range previous {
		if !seen[id] {
			res.Removed++
		}
	}

	if err := m.store.UpsertServers(records); err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}
	if err := m.store.DeleteSourceServersExcept(sub.ID, keep); err != nil {
		m.fail(sub, err)
		res.Error = err.Error()
		return res
	}

	res.ServerCount = len(records)
	if err := m.store.RecordSubscriptionResult(sub.ID, resp.Header.Get("ETag"),
		resp.Header.Get("Last-Modified"), "", len(records), true); err != nil {
		m.logger.Warn("cannot record subscription result", "error", err)
	}

	m.logger.Info("subscription updated",
		"subscription_id", sub.ID, "name", sub.Name,
		"servers", res.ServerCount, "added", res.Added, "removed", res.Removed)

	if m.apply != nil {
		if err := m.apply(ctx); err != nil {
			m.logger.Error("cannot apply servers after subscription update", "error", err)
			res.Error = "servers stored but engine reload failed: " + err.Error()
		}
	}
	return res
}

func (m *Manager) fail(sub storage.Subscription, err error) {
	m.logger.Warn("subscription update failed",
		"subscription_id", sub.ID, "name", sub.Name, "error", err)
	if recErr := m.store.RecordSubscriptionResult(sub.ID, "", "", err.Error(), sub.ServerCount, false); recErr != nil {
		m.logger.Error("cannot record subscription failure", "error", recErr)
	}
}

// UpdateAll refreshes every enabled subscription.
func (m *Manager) UpdateAll(ctx context.Context) []Result {
	subs, err := m.store.ListSubscriptions()
	if err != nil {
		m.logger.Error("cannot list subscriptions", "error", err)
		return nil
	}
	results := make([]Result, 0, len(subs))
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		results = append(results, m.Update(ctx, sub))
	}
	return results
}

// Run refreshes due subscriptions at startup and then once a minute.
func (m *Manager) Run(ctx context.Context) {
	m.updateDue(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.updateDue(ctx)
		}
	}
}

func (m *Manager) updateDue(ctx context.Context) {
	subs, err := m.store.ListSubscriptions()
	if err != nil {
		m.logger.Error("cannot list subscriptions", "error", err)
		return
	}
	now := time.Now()
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		interval := time.Duration(sub.IntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		if !sub.LastCheckedAt.IsZero() && now.Sub(sub.LastCheckedAt) < interval {
			continue
		}
		m.Update(ctx, sub)
	}
}

// validateURL rejects non-HTTP URLs and, unless explicitly allowed, hosts that
// resolve to loopback, private or link-local addresses (SSRF protection).
func (m *Manager) validateURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid subscription url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("subscription url must use http or https")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("subscription url has no host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("subscription url must not contain credentials")
	}
	if m.opts.AllowPrivateHosts {
		return u, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve subscription host: %w", err)
	}
	for _, addr := range addrs {
		if isForbiddenIP(addr.IP) {
			return nil, fmt.Errorf("subscription host resolves to a private address")
		}
	}
	return u, nil
}

func isForbiddenIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// ValidateURL exposes the SSRF check for API handlers.
func (m *Manager) ValidateURL(ctx context.Context, raw string) error {
	_, err := m.validateURL(ctx, raw)
	return err
}

// allPlaceholders reports whether every parsed server is a non-routable stub
// such as vless://...@0.0.0.0:1 that some panels return without HWID.
func allPlaceholders(infos []*parser.Server) bool {
	if len(infos) == 0 {
		return false
	}
	for _, info := range infos {
		if info == nil {
			continue
		}
		if info.Host != "0.0.0.0" && info.Host != "::" && info.Port != 1 {
			return false
		}
	}
	return true
}

const hwidSettingKey = "subscription_hwid"

// loadOrCreateHWID returns the persisted device id, generating one on first use.
func loadOrCreateHWID(store *storage.Store, logger *slog.Logger) string {
	if value, ok, err := store.GetSetting(hwidSettingKey); err != nil {
		logger.Warn("cannot read subscription hwid", "error", err)
	} else if ok && value != "" {
		return value
	}
	id := newUUID()
	if err := store.SetSetting(hwidSettingKey, id); err != nil {
		logger.Warn("cannot persist subscription hwid", "error", err)
	}
	return id
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ErrNoSubscriptions is returned by helpers when nothing is configured.
var ErrNoSubscriptions = errors.New("no subscriptions configured")

// IsLikelyBase64 reports whether the body looks like a base64 subscription.
func IsLikelyBase64(body []byte) bool {
	text := strings.TrimSpace(string(body))
	return !strings.Contains(text, "://") && len(text) > 16
}
