// Command multivless is the Multi-Exit VLESS Gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/multivless/multivless/internal/app"
	"github.com/multivless/multivless/internal/config"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.2.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("command is required")
	}

	command := os.Args[1]
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	configPath := fs.String("config", "/etc/multivless/config.yaml", "path to config.yaml")

	switch command {
	case "run", "check-config", "healthcheck":
		_ = fs.Parse(os.Args[2:])
	case "version":
		fmt.Println("multivless", version)
		return nil
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Docker and systemd deployments configure the admin token via environment.
	if token := os.Getenv("MULTIVLESS_API_TOKEN"); token != "" {
		cfg.API.Token = token
	}

	logger := newLogger(cfg.App.LogLevel)
	slog.SetDefault(logger)

	switch command {
	case "check-config":
		fmt.Println("configuration is valid")
		return nil
	case "healthcheck":
		return healthcheck(cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting multivless", "version", version, "config", *configPath)
	if err := app.New(cfg, version, logger).Run(ctx); err != nil {
		return err
	}
	logger.Info("multivless stopped")
	return nil
}

// healthcheck verifies that the SOCKS5 listener accepts connections.
func healthcheck(cfg config.Config) error {
	addr := net.JoinHostPort(cfg.SOCKS5.Listen, strconv.Itoa(cfg.SOCKS5.Port))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("socks5 listener is not reachable at %s: %w", addr, err)
	}
	return conn.Close()
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

func usage() {
	fmt.Fprintf(os.Stderr, `multivless %s

Usage:
  multivless run [--config PATH]          start the gateway and web UI
  multivless check-config [--config PATH] validate configuration
  multivless healthcheck [--config PATH]  verify the SOCKS5 listener
  multivless version                      print version
`, version)
}
