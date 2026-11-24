// Package api exposes the REST API and the embedded web UI.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multivless/multivless/internal/balancer"
	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/connections"
	"github.com/multivless/multivless/internal/engine/singbox"
	"github.com/multivless/multivless/internal/eventlog"
	"github.com/multivless/multivless/internal/health"
	"github.com/multivless/multivless/internal/security"
	"github.com/multivless/multivless/internal/servers"
	"github.com/multivless/multivless/internal/storage"
	"github.com/multivless/multivless/internal/subscriptions"
	"github.com/multivless/multivless/internal/subscriptions/parser"
	"github.com/multivless/multivless/internal/web"
)

// Per-subscription health-check interval bounds.
const (
	DefaultServerCheckSeconds = 30
	MinServerCheckSeconds     = 5
	MaxServerCheckSeconds     = 86400
)

// Options wires the API to the running application.
type Options struct {
	Config        config.API
	Store         *storage.Store
	Registry      *servers.Registry
	Engine        *singbox.Manager
	Balancer      *balancer.Balancer
	Health        *health.Checker
	Subscriptions *subscriptions.Manager
	Tracker       *connections.Tracker
	Events        *eventlog.Log
	// Reload synchronises the registry from the database and restarts the
	// engine (needed when servers are added or removed).
	Reload func(ctx context.Context) error
	// Refresh synchronises the registry without restarting the engine.
	Refresh func() error
	// DefaultSubInterval is applied to subscriptions created without one.
	DefaultSubInterval time.Duration
	Version            string
	Logger             *slog.Logger
}

// Server serves the REST API and the admin UI.
type Server struct {
	opts      Options
	token     string
	startedAt time.Time
}

// New creates the API server. An empty token in the configuration produces a
// random token which is logged once so the UI is still usable.
func New(opts Options) *Server {
	token := opts.Config.Token
	if token == "" {
		buf := make([]byte, 24)
		_, _ = rand.Read(buf)
		token = "mvl_" + hex.EncodeToString(buf)
		opts.Logger.Warn("api.token is empty; generated a temporary admin token",
			"token", token)
	}
	return &Server{opts: opts, token: token, startedAt: time.Now()}
}

// Listen starts the HTTP server and blocks until the context is cancelled.
func (s *Server) Listen(ctx context.Context) error {
	addr := net.JoinHostPort(s.opts.Config.Listen, strconv.Itoa(s.opts.Config.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.opts.Logger.Info("web ui listening", "addr", "http://"+addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler builds the HTTP routing tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/status", s.handleStatus)
	api.HandleFunc("GET /api/v1/stats", s.handleStats)

	api.HandleFunc("GET /api/v1/servers", s.handleListServers)
	api.HandleFunc("POST /api/v1/servers", s.handleAddServer)
	api.HandleFunc("PATCH /api/v1/servers/{id}", s.handlePatchServer)
	api.HandleFunc("DELETE /api/v1/servers/{id}", s.handleDeleteServer)
	api.HandleFunc("POST /api/v1/servers/{id}/check", s.handleCheckServer)

	api.HandleFunc("GET /api/v1/subscriptions", s.handleListSubscriptions)
	api.HandleFunc("POST /api/v1/subscriptions", s.handleCreateSubscription)
	api.HandleFunc("PATCH /api/v1/subscriptions/{id}", s.handlePatchSubscription)
	api.HandleFunc("DELETE /api/v1/subscriptions/{id}", s.handleDeleteSubscription)
	api.HandleFunc("POST /api/v1/subscriptions/{id}/update", s.handleUpdateSubscription)
	api.HandleFunc("POST /api/v1/subscriptions/update", s.handleUpdateAllSubscriptions)

	api.HandleFunc("GET /api/v1/users", s.handleListUsers)
	api.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	api.HandleFunc("PATCH /api/v1/users/{id}", s.handlePatchUser)
	api.HandleFunc("DELETE /api/v1/users/{id}", s.handleDeleteUser)

	api.HandleFunc("GET /api/v1/connections", s.handleListConnections)
	api.HandleFunc("DELETE /api/v1/connections/{id}", s.handleCloseConnection)
	api.HandleFunc("GET /api/v1/logs", s.handleListLogs)

	api.HandleFunc("GET /api/v1/settings", s.handleGetSettings)
	api.HandleFunc("PATCH /api/v1/settings", s.handlePatchSettings)

	api.HandleFunc("GET /api/v1/metrics", s.handleMetrics)

	mux.Handle("/api/v1/", s.requireAuth(api))
	mux.Handle("/", web.Handler())
	return mux
}

// ---- middleware ----

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) statusOr500(w http.ResponseWriter, err error) {
	s.opts.Logger.Error("api request failed", "error", err)
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// ---- health/status/stats ----

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.opts.Version,
		"engine":  s.opts.Engine.Running(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	counts := s.opts.Registry.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":           s.opts.Version,
		"uptime_seconds":    int64(time.Since(s.startedAt).Seconds()),
		"engine_running":    s.opts.Engine.Running(),
		"servers":           counts,
		"balancer_strategy": s.opts.Balancer.Strategy(),
		"connections":       s.opts.Tracker.Summary(),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	subs, _ := s.opts.Store.ListSubscriptions()
	users, _ := s.opts.Store.ListUsers()
	writeJSON(w, http.StatusOK, map[string]any{
		"servers":        s.opts.Registry.Counts(),
		"connections":    s.opts.Tracker.Summary(),
		"subscriptions":  len(subs),
		"users":          len(users),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

// ---- servers ----

// isSubscriptionURL catches the common mistake of pasting an http(s)
// subscription link into the single-server form.
func isSubscriptionURL(raw string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	return strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://")
}

type serverDTO struct {
	servers.Snapshot
	SourceID string `json:"source_id,omitempty"`
}

func (s *Server) handleListServers(w http.ResponseWriter, _ *http.Request) {
	stored, err := s.opts.Store.ListServers()
	if err != nil {
		s.statusOr500(w, err)
		return
	}
	sources := make(map[string]string, len(stored))
	for _, rec := range stored {
		if rec.SourceID != "" {
			sources[rec.Info.ID] = rec.SourceID
		}
	}
	list := s.opts.Registry.List()
	out := make([]serverDTO, 0, len(list))
	for _, srv := range list {
		snap := srv.Snapshot()
		out = append(out, serverDTO{Snapshot: snap, SourceID: sources[snap.ID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URI    string `json:"uri"`
		Weight int    `json:"weight"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if isSubscriptionURL(body.URI) {
		writeErr(w, http.StatusBadRequest, "this is a subscription URL: add it on the Subscriptions tab, not as a single server")
		return
	}
	info, err := parser.ParseURI(body.URI)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	weight := body.Weight
	if weight <= 0 {
		weight = 100
	}
	rec := storage.Server{Info: info, Enabled: true, Weight: weight}
	if err := s.opts.Store.UpsertServers([]storage.Server{rec}); err != nil {
		s.statusOr500(w, err)
		return
	}
	if err := s.opts.Reload(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "server stored but engine reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": info.ID, "name": info.Name})
}

func (s *Server) handlePatchServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled *bool `json:"enabled"`
		Weight  *int  `json:"weight"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil && body.Weight == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if _, ok := s.opts.Registry.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	if body.Enabled != nil {
		if err := s.opts.Store.SetServerEnabled(id, *body.Enabled); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	if body.Weight != nil {
		if *body.Weight <= 0 || *body.Weight > 10000 {
			writeErr(w, http.StatusBadRequest, "weight must be between 1 and 10000")
			return
		}
		if err := s.opts.Store.SetServerWeight(id, *body.Weight); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	if err := s.opts.Refresh(); err != nil {
		s.statusOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.opts.Registry.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	if err := s.opts.Store.DeleteServer(id); err != nil {
		s.statusOr500(w, err)
		return
	}
	if err := s.opts.Reload(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "server removed but engine reload failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCheckServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv, ok := s.opts.Registry.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	s.opts.Health.CheckServer(ctx, srv)
	snap := srv.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     snap.Status,
		"latency_ms": snap.LatencyMS,
		"error":      snap.LastError,
	})
}

// ---- subscriptions ----

type subscriptionDTO struct {
	storage.Subscription
	MaskedURL string `json:"url_masked"`
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "***"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func (s *Server) handleListSubscriptions(w http.ResponseWriter, _ *http.Request) {
	subs, err := s.opts.Store.ListSubscriptions()
	if err != nil {
		s.statusOr500(w, err)
		return
	}
	out := make([]subscriptionDTO, 0, len(subs))
	for _, sub := range subs {
		out = append(out, subscriptionDTO{Subscription: sub, MaskedURL: maskURL(sub.URL)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string `json:"name"`
		URL                string `json:"url"`
		IntervalSeconds    int    `json:"interval_seconds"`
		ServerCheckSeconds int    `json:"server_check_seconds"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.URL == "" {
		writeErr(w, http.StatusBadRequest, "url is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.opts.Subscriptions.ValidateURL(ctx, body.URL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.IntervalSeconds <= 0 {
		body.IntervalSeconds = int(s.opts.DefaultSubInterval.Seconds())
	}
	if body.ServerCheckSeconds <= 0 {
		body.ServerCheckSeconds = DefaultServerCheckSeconds
	}
	if body.ServerCheckSeconds < MinServerCheckSeconds || body.ServerCheckSeconds > MaxServerCheckSeconds {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("server_check_seconds must be between %d and %d", MinServerCheckSeconds, MaxServerCheckSeconds))
		return
	}
	sub := storage.Subscription{
		ID:                 storage.NewID("sub_"),
		Name:               body.Name,
		URL:                body.URL,
		IntervalSeconds:    body.IntervalSeconds,
		ServerCheckSeconds: body.ServerCheckSeconds,
		Enabled:            true,
	}
	if err := s.opts.Store.CreateSubscription(sub); err != nil {
		s.statusOr500(w, err)
		return
	}

	updateCtx, updateCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer updateCancel()
	result := s.opts.Subscriptions.Update(updateCtx, sub)

	writeJSON(w, http.StatusCreated, map[string]any{
		"subscription": subscriptionDTO{Subscription: sub, MaskedURL: maskURL(sub.URL)},
		"update":       result,
	})
}

func (s *Server) handlePatchSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	current, err := s.opts.Store.GetSubscription(id)
	if errors.Is(err, storage.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "subscription not found")
		return
	}
	if err != nil {
		s.statusOr500(w, err)
		return
	}

	var body struct {
		Name               *string `json:"name"`
		URL                *string `json:"url"`
		IntervalSeconds    *int    `json:"interval_seconds"`
		ServerCheckSeconds *int    `json:"server_check_seconds"`
		Enabled            *bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name != nil {
		current.Name = strings.TrimSpace(*body.Name)
	}
	if body.URL != nil {
		current.URL = strings.TrimSpace(*body.URL)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.opts.Subscriptions.ValidateURL(ctx, current.URL); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.IntervalSeconds != nil {
		if *body.IntervalSeconds < 10 {
			writeErr(w, http.StatusBadRequest, "interval_seconds must be at least 10")
			return
		}
		current.IntervalSeconds = *body.IntervalSeconds
	}
	if body.ServerCheckSeconds != nil {
		if *body.ServerCheckSeconds < MinServerCheckSeconds || *body.ServerCheckSeconds > MaxServerCheckSeconds {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("server_check_seconds must be between %d and %d", MinServerCheckSeconds, MaxServerCheckSeconds))
			return
		}
		current.ServerCheckSeconds = *body.ServerCheckSeconds
	}
	if body.Enabled != nil {
		current.Enabled = *body.Enabled
	}
	if current.Name == "" || current.URL == "" {
		writeErr(w, http.StatusBadRequest, "name and url are required")
		return
	}
	if err := s.opts.Store.UpdateSubscription(current.ID, current.Name, current.URL,
		current.IntervalSeconds, current.ServerCheckSeconds, current.Enabled); err != nil {
		s.statusOr500(w, err)
		return
	}
	// Pick up a new health-check interval without restarting the engine.
	if err := s.opts.Refresh(); err != nil {
		s.statusOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Store.GetSubscription(id); errors.Is(err, storage.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "subscription not found")
		return
	} else if err != nil {
		s.statusOr500(w, err)
		return
	}
	if err := s.opts.Store.DeleteSubscription(id); err != nil {
		s.statusOr500(w, err)
		return
	}
	if err := s.opts.Reload(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "subscription removed but engine reload failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub, err := s.opts.Store.GetSubscription(id)
	if errors.Is(err, storage.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "subscription not found")
		return
	}
	if err != nil {
		s.statusOr500(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := s.opts.Subscriptions.Update(ctx, sub)
	if result.Error != "" {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleUpdateAllSubscriptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	results := s.opts.Subscriptions.UpdateAll(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---- users ----

func (s *Server) handleListUsers(w http.ResponseWriter, _ *http.Request) {
	users, err := s.opts.Store.ListUsers()
	if err != nil {
		s.statusOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		MaxConnections int    `json:"max_connections"`
		BypassTorrent  bool   `json:"bypass_torrent"`
		Enabled        *bool  `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if body.MaxConnections < 0 {
		writeErr(w, http.StatusBadRequest, "max_connections must not be negative")
		return
	}
	if _, err := s.opts.Store.GetUserByUsername(body.Username); err == nil {
		writeErr(w, http.StatusConflict, "username already exists")
		return
	} else if !errors.Is(err, storage.ErrNotFound) {
		s.statusOr500(w, err)
		return
	}
	hash, err := security.HashPassword(body.Password)
	if err != nil {
		s.statusOr500(w, err)
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	user := storage.User{
		ID:             storage.NewID("usr_"),
		Username:       body.Username,
		PasswordHash:   hash,
		Enabled:        enabled,
		MaxConnections: body.MaxConnections,
		BypassTorrent:  body.BypassTorrent,
	}
	if err := s.opts.Store.CreateUser(user); err != nil {
		s.statusOr500(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": user.ID, "username": user.Username})
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Store.GetUser(id); errors.Is(err, storage.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		s.statusOr500(w, err)
		return
	}
	var body struct {
		Password       *string `json:"password"`
		Enabled        *bool   `json:"enabled"`
		MaxConnections *int    `json:"max_connections"`
		BypassTorrent  *bool   `json:"bypass_torrent"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Password != nil {
		if *body.Password == "" {
			writeErr(w, http.StatusBadRequest, "password must not be empty")
			return
		}
		hash, err := security.HashPassword(*body.Password)
		if err != nil {
			s.statusOr500(w, err)
			return
		}
		if err := s.opts.Store.SetUserPassword(id, hash); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	if body.Enabled != nil {
		if err := s.opts.Store.SetUserEnabled(id, *body.Enabled); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	if body.MaxConnections != nil {
		if *body.MaxConnections < 0 {
			writeErr(w, http.StatusBadRequest, "max_connections must not be negative")
			return
		}
		if err := s.opts.Store.SetUserMaxConnections(id, *body.MaxConnections); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	if body.BypassTorrent != nil {
		if err := s.opts.Store.SetUserTorrentBypass(id, *body.BypassTorrent); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.opts.Store.DeleteUser(id); err != nil {
		s.statusOr500(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- connections ----

func (s *Server) handleListConnections(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": s.opts.Tracker.List(),
		"recent":      s.opts.Tracker.Recent(),
	})
}

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.opts.Events.List(limit)})
}

func (s *Server) handleCloseConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.opts.Tracker.Close(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- settings ----

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"balancer_strategy": s.opts.Balancer.Strategy(),
	})
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BalancerStrategy string `json:"balancer_strategy"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.BalancerStrategy != "" {
		if err := s.opts.Balancer.SetStrategy(body.BalancerStrategy); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.opts.Store.SetSetting("balancer_strategy", body.BalancerStrategy); err != nil {
			s.statusOr500(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balancer_strategy": s.opts.Balancer.Strategy(),
	})
}

// ---- prometheus ----

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder

	engineUp := 0
	if s.opts.Engine.Running() {
		engineUp = 1
	}
	fmt.Fprintf(&b, "# HELP multivless_engine_up Whether the sing-box engine is running.\n")
	fmt.Fprintf(&b, "# TYPE multivless_engine_up gauge\n")
	fmt.Fprintf(&b, "multivless_engine_up %d\n", engineUp)

	fmt.Fprintf(&b, "# HELP multivless_server_up Server health (1 = selectable).\n")
	fmt.Fprintf(&b, "# TYPE multivless_server_up gauge\n")
	fmt.Fprintf(&b, "# HELP multivless_server_latency_ms Smoothed probe latency.\n")
	fmt.Fprintf(&b, "# TYPE multivless_server_latency_ms gauge\n")
	fmt.Fprintf(&b, "# HELP multivless_server_active_connections Active proxied connections.\n")
	fmt.Fprintf(&b, "# TYPE multivless_server_active_connections gauge\n")
	fmt.Fprintf(&b, "# HELP multivless_server_bytes_sent_total Bytes sent to servers.\n")
	fmt.Fprintf(&b, "# TYPE multivless_server_bytes_sent_total counter\n")
	fmt.Fprintf(&b, "# HELP multivless_server_bytes_received_total Bytes received from servers.\n")
	fmt.Fprintf(&b, "# TYPE multivless_server_bytes_received_total counter\n")

	for _, srv := range s.opts.Registry.List() {
		snap := srv.Snapshot()
		label := escapeLabel(snap.Name)
		up := 0
		if srv.Selectable() {
			up = 1
		}
		fmt.Fprintf(&b, "multivless_server_up{server=\"%s\"} %d\n", label, up)
		fmt.Fprintf(&b, "multivless_server_latency_ms{server=\"%s\"} %.3f\n", label, snap.LatencyMS)
		fmt.Fprintf(&b, "multivless_server_active_connections{server=\"%s\"} %d\n", label, snap.Active)
		fmt.Fprintf(&b, "multivless_server_bytes_sent_total{server=\"%s\"} %d\n", label, snap.BytesOut)
		fmt.Fprintf(&b, "multivless_server_bytes_received_total{server=\"%s\"} %d\n", label, snap.BytesIn)
	}

	summary := s.opts.Tracker.Summary()
	fmt.Fprintf(&b, "# HELP multivless_connections_total Connections opened since start.\n")
	fmt.Fprintf(&b, "# TYPE multivless_connections_total counter\n")
	fmt.Fprintf(&b, "multivless_connections_total %d\n", summary.Total)
	fmt.Fprintf(&b, "# HELP multivless_connections_active Currently active connections.\n")
	fmt.Fprintf(&b, "# TYPE multivless_connections_active gauge\n")
	fmt.Fprintf(&b, "multivless_connections_active %d\n", summary.Active)

	_, _ = io.WriteString(w, b.String())
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	return strings.ReplaceAll(v, "\n", "\\n")
}
