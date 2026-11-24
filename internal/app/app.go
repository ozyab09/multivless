// Package app wires the gateway components together.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/multivless/multivless/internal/api"
	"github.com/multivless/multivless/internal/balancer"
	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/connections"
	"github.com/multivless/multivless/internal/engine/singbox"
	"github.com/multivless/multivless/internal/eventlog"
	"github.com/multivless/multivless/internal/geoip"
	"github.com/multivless/multivless/internal/health"
	"github.com/multivless/multivless/internal/router"
	"github.com/multivless/multivless/internal/servers"
	"github.com/multivless/multivless/internal/socks5"
	"github.com/multivless/multivless/internal/storage"
	"github.com/multivless/multivless/internal/subscriptions"
	"github.com/multivless/multivless/internal/subscriptions/parser"
	"github.com/multivless/multivless/internal/users"
)

// App is the running gateway.
type App struct {
	cfg     config.Config
	version string
	logger  *slog.Logger

	store    *storage.Store
	registry *servers.Registry
	engine   *singbox.Manager
	balancer *balancer.Balancer
	tracker  *connections.Tracker
	events   *eventlog.Log
	geo      *geoip.DB
	health   *health.Checker
	subs     *subscriptions.Manager
	gateway  *socks5.Gateway
}

// New prepares the application without starting network activity.
func New(cfg config.Config, version string, logger *slog.Logger) *App {
	return &App{cfg: cfg, version: version, logger: logger}
}

// Run starts storage, the engine, health checks, subscriptions, the API and the
// SOCKS5 gateway. It blocks until the context is cancelled.
func (a *App) Run(ctx context.Context) error {
	store, err := storage.Open(a.cfg.Storage.Path)
	if err != nil {
		return err
	}
	a.store = store
	defer store.Close()

	if err := users.SeedFromConfig(store, a.cfg.SOCKS5.Users, a.logger); err != nil {
		return err
	}
	if err := a.bootstrapServers(ctx); err != nil {
		return err
	}

	a.registry = servers.NewRegistry(nil)
	if err := a.syncRegistry(); err != nil {
		return err
	}
	a.logger.Info("servers loaded", "total", len(a.registry.List()), "strategy", a.cfg.Balancer.Strategy)

	// Restore the balancer strategy chosen in the UI.
	balance := balancer.New(a.cfg.Balancer)
	if value, ok, err := store.GetSetting("balancer_strategy"); err == nil && ok {
		if err := balance.SetStrategy(value); err != nil {
			a.logger.Warn("ignoring stored balancer strategy", "value", value, "error", err)
		}
	}
	a.balancer = balance

	a.engine = singbox.NewManager(singbox.Options{
		Engine:      a.cfg.Engine,
		HealthLanes: a.cfg.HealthCheck.Concurrency,
		Logger:      a.logger,
	})
	if err := a.startEngine(ctx); err != nil {
		return err
	}
	defer func() {
		if err := a.engine.Stop(); err != nil {
			a.logger.Error("engine stop failed", "error", err)
		}
	}()

	a.tracker = connections.New()
	a.events = eventlog.New(1000)
	if db, err := geoip.Load(a.cfg.GeoIP.Path); err != nil {
		a.logger.Warn("geoip database is unavailable; DNS logs will not show countries",
			"path", a.cfg.GeoIP.Path, "error", err)
	} else {
		a.geo = db
		a.logger.Info("geoip database loaded", "path", a.cfg.GeoIP.Path)
	}
	connRouter := router.New(router.Options{
		Balancer:    a.balancer,
		Registry:    a.registry,
		Engine:      a.engine,
		Tracker:     a.tracker,
		Events:      a.events,
		Geo:         a.geo,
		MaxAttempts: a.cfg.Balancer.MaxAttempts,
		OnUserBytes: func(userID string, in, out uint64) {
			if err := a.store.AddUserBytes(userID, in, out); err != nil {
				a.logger.Debug("cannot record user traffic", "error", err)
			}
		},
		Logger: a.logger,
	})

	a.health = health.New(
		a.registry,
		a.engine,
		a.cfg.HealthCheck,
		a.engine.HealthLaneOffset(),
		a.cfg.Balancer.DegradedLatencyMS,
		a.logger,
	)

	a.subs = subscriptions.New(a.store, subscriptions.Options{
		Timeout:           a.cfg.Subscriptions.Timeout.Std(),
		MaxSize:           a.cfg.Subscriptions.MaxSize,
		AllowPrivateHosts: a.cfg.Subscriptions.AllowPrivateHosts,
		UserAgent:         "multivless/" + a.version,
	}, a.reloadEngine, a.logger)

	auth := users.NewAuthenticator(a.store, a.logger)
	a.gateway = socks5.New(a.cfg.SOCKS5, auth, connRouter.Dial, a.logger)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var apiErr atomic.Value
	if a.cfg.API.Enabled {
		apiServer := api.New(api.Options{
			Config:             a.cfg.API,
			Store:              a.store,
			Registry:           a.registry,
			Engine:             a.engine,
			Balancer:           a.balancer,
			Health:             a.health,
			Subscriptions:      a.subs,
			Tracker:            a.tracker,
			Events:             a.events,
			Reload:             a.reloadEngine,
			Refresh:            a.syncRegistry,
			DefaultSubInterval: a.cfg.Subscriptions.Interval.Std(),
			Version:            a.version,
			Logger:             a.logger,
		})
		go func() {
			if err := apiServer.Listen(runCtx); err != nil {
				apiErr.Store(err)
				cancel()
			}
		}()
	}

	go a.health.Run(runCtx)
	go a.subs.Run(runCtx)

	err = a.gateway.Listen(runCtx)
	if value := apiErr.Load(); value != nil {
		if err == nil {
			return value.(error)
		}
		a.logger.Error("api server failed", "error", value)
	}
	return err
}

// bootstrapServers imports YAML servers/sources into an empty database.
func (a *App) bootstrapServers(_ context.Context) error {
	existing, err := a.store.ListServers()
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	infos, err := a.collectServers()
	if err != nil {
		return nil // subscriptions will fill the pool later
	}
	records := make([]storage.Server, 0, len(infos))
	for _, info := range infos {
		records = append(records, storage.Server{Info: info, Enabled: true, Weight: 100})
	}
	if err := a.store.UpsertServers(records); err != nil {
		return fmt.Errorf("import servers from config: %w", err)
	}
	a.logger.Info("imported servers from config", "count", len(records))
	return nil
}

// startEngine launches sing-box with the currently stored server set.
func (a *App) startEngine(ctx context.Context) error {
	infos := a.registryInfos()
	if len(infos) == 0 {
		a.logger.Warn("no servers configured yet: the gateway is fail-closed until a subscription is added")
		return nil
	}
	unsupported, err := a.engine.Start(ctx, infos)
	a.parkUnsupported(unsupported)
	if err != nil {
		return fmt.Errorf("start proxy engine: %w", err)
	}
	return nil
}

// reloadEngine synchronises the registry from the database and applies the new
// server set to sing-box.
func (a *App) reloadEngine(ctx context.Context) error {
	if err := a.syncRegistry(); err != nil {
		return err
	}
	infos := a.registryInfos()
	if len(infos) == 0 {
		if a.engine.Running() {
			if err := a.engine.Stop(); err != nil {
				return err
			}
		}
		return nil
	}
	unsupported, err := a.engine.Apply(ctx, infos)
	a.parkUnsupported(unsupported)
	if err != nil {
		return err
	}
	a.registry.ResetProbeCounters()
	a.logger.Info("engine configuration applied", "servers", len(infos), "unsupported", len(unsupported))
	return nil
}

// parkUnsupported marks servers the running sing-box build cannot represent as
// ERROR (and disables them) so health checks stop probing them forever. Without
// this they would stay UNKNOWN/OFFLINE with a misleading Clash API error.
func (a *App) parkUnsupported(list []*parser.Server) {
	for _, srv := range list {
		if entry, ok := a.registry.Get(srv.ID); ok {
			entry.MarkConfigError(fmt.Sprintf("transport %q is not supported by the configured sing-box version", srv.Network))
			a.logger.Warn("server parked as ERROR", "server_id", srv.ID, "name", srv.Name, "transport", srv.Network)
		}
	}
}

func (a *App) syncRegistry() error {
	stored, err := a.store.ListServers()
	if err != nil {
		return err
	}
	subs, err := a.store.ListSubscriptions()
	if err != nil {
		return err
	}
	checkIntervals := make(map[string]time.Duration, len(subs))
	for _, sub := range subs {
		if sub.ServerCheckSeconds > 0 {
			checkIntervals[sub.ID] = time.Duration(sub.ServerCheckSeconds) * time.Second
		}
	}
	records := make([]servers.SyncRecord, 0, len(stored))
	for _, rec := range stored {
		records = append(records, servers.SyncRecord{
			Info:          rec.Info,
			Enabled:       rec.Enabled,
			Weight:        rec.Weight,
			CheckInterval: checkIntervals[rec.SourceID],
		})
	}
	added, removed := a.registry.Sync(records)
	if added > 0 || removed > 0 {
		a.logger.Info("registry synchronised", "added", added, "removed", removed, "total", len(records))
	}
	return nil
}

func (a *App) registryInfos() []*parser.Server {
	list := a.registry.List()
	infos := make([]*parser.Server, 0, len(list))
	for _, srv := range list {
		infos = append(infos, srv.Info)
	}
	return infos
}

// collectServers parses configured URIs and source files.
func (a *App) collectServers() ([]*parser.Server, error) {
	var (
		infos []*parser.Server
		seen  = map[string]bool{}
	)
	add := func(candidates []*parser.Server) {
		for _, srv := range candidates {
			if seen[srv.ID] {
				continue
			}
			seen[srv.ID] = true
			infos = append(infos, srv)
		}
	}

	for i, raw := range a.cfg.Servers {
		srv, err := parser.ParseURI(raw)
		if err != nil {
			a.logger.Warn("invalid vless URI in config", "index", i, "error", err)
			continue
		}
		add([]*parser.Server{srv})
	}
	for i, path := range a.cfg.Sources {
		raw, err := os.ReadFile(path)
		if err != nil {
			a.logger.Warn("cannot read source file", "index", i, "path", path, "error", err)
			continue
		}
		parsed, errs := parser.ParseContent(raw)
		for _, perr := range errs {
			a.logger.Warn("source entry skipped", "path", path, "error", perr)
		}
		add(parsed)
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("no valid VLESS servers in config")
	}
	return infos, nil
}

// Healthy reports whether at least one outbound is selectable.
func (a *App) Healthy() bool { return a.registry != nil && len(a.registry.Selectable()) > 0 }
