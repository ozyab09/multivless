// Package config loads and validates the gateway configuration file.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings such as "20s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the value as a standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the root of config.yaml.
type Config struct {
	App           App           `yaml:"app"`
	Engine        Engine        `yaml:"engine"`
	SOCKS5        SOCKS5        `yaml:"socks5"`
	Balancer      Balancer      `yaml:"balancer"`
	HealthCheck   Health        `yaml:"health_check"`
	Storage       Storage       `yaml:"storage"`
	API           API           `yaml:"api"`
	Subscriptions Subscriptions `yaml:"subscriptions"`
	// GeoIP points at a v2fly/Xray geoip.dat used to label DNS answers.
	GeoIP GeoIP `yaml:"geoip"`
	// Servers are manually added VLESS URIs.
	Servers []string `yaml:"servers"`
	// Sources are local files with VLESS URIs or base64-encoded subscription bodies.
	Sources []string `yaml:"sources"`
}

// App holds general process settings.
type App struct {
	Name     string `yaml:"name"`
	LogLevel string `yaml:"log_level"`
	DataDir  string `yaml:"data_dir"`
}

// Engine configures the sing-box process that carries VLESS traffic.
type Engine struct {
	Type             string   `yaml:"type"`
	Binary           string   `yaml:"binary"`
	ConfigPath       string   `yaml:"config_path"`
	ClashAPI         string   `yaml:"clash_api"`
	Lanes            int      `yaml:"lanes"`
	StartTimeout     Duration `yaml:"start_timeout"`
	RestartOnFailure bool     `yaml:"restart_on_failure"`
	RestartDelay     Duration `yaml:"restart_delay"`
	LogLevel         string   `yaml:"log_level"`
}

// Storage configures the SQLite database and admin token storage.
type Storage struct {
	Path string `yaml:"path"`
}

// API configures the REST API and the embedded web UI.
type API struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	Port    int    `yaml:"port"`
	Token   string `yaml:"token"`
}

// GeoIP configures the optional country database for the logs tab.
type GeoIP struct {
	Path string `yaml:"path"`
}

// Subscriptions configures subscription downloads.
type Subscriptions struct {
	Interval          Duration `yaml:"interval"`
	Timeout           Duration `yaml:"timeout"`
	MaxSize           int64    `yaml:"max_size"`
	AllowPrivateHosts bool     `yaml:"allow_private_hosts"`
}

// Strategy names accepted by the balancer.
const (
	StrategyAdaptive         = "adaptive"
	StrategyRoundRobin       = "round-robin"
	StrategyRandom           = "random"
	StrategyLeastLatency     = "least-latency"
	StrategyLeastConnections = "least-connections"
	StrategyWeighted         = "weighted"
)

// Constructor is the main balancing strategy used for new connections.
type Balancer struct {
	Strategy          string   `yaml:"strategy"`
	FallbackStrategy  string   `yaml:"fallback_strategy"`
	MaxAttempts       int      `yaml:"max_attempts"`
	DegradedLatencyMS float64  `yaml:"degraded_latency_ms"`
	Weights           Weights  `yaml:"weights"`
	HostCooldown      Duration `yaml:"host_cooldown"`
}

// Weights controls the adaptive score components; they are normalized to 1.0.
type Weights struct {
	Latency     float64 `yaml:"latency"`
	Connections float64 `yaml:"connections"`
	Throughput  float64 `yaml:"throughput"`
	Reliability float64 `yaml:"reliability"`
}

// Sum returns the total of all adaptive weights.
func (w Weights) Sum() float64 {
	return w.Latency + w.Connections + w.Throughput + w.Reliability
}

// SOCKS5 is the inbound gateway configuration.
type SOCKS5 struct {
	Enabled           bool     `yaml:"enabled"`
	Listen            string   `yaml:"listen"`
	Port              int      `yaml:"port"`
	Authentication    bool     `yaml:"authentication"`
	AllowNoAuth       bool     `yaml:"allow_no_auth"`
	Network           string   `yaml:"network"`
	MaxConnections    int      `yaml:"max_connections"`
	ConnectionTimeout Duration `yaml:"connection_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	Users             []User   `yaml:"users"`
}

// User is a SOCKS5 account. Stage 1 keeps passwords in the config file; the
// Argon2id hash in SQLite arrives with the storage stage.
type User struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Health configures periodic liveness checks that run through each outbound.
type Health struct {
	Enabled            bool     `yaml:"enabled"`
	Interval           Duration `yaml:"interval"`
	Timeout            Duration `yaml:"timeout"`
	FailuresBeforeDown int      `yaml:"failures_before_offline"`
	SuccessesBeforeUp  int      `yaml:"successes_before_online"`
	Concurrency        int      `yaml:"concurrency"`
	URL                string   `yaml:"url"`
	FallbackURLs       []string `yaml:"fallback_urls"`
}

// Default returns a configuration with the values from the specification.
func Default() Config {
	return Config{
		App: App{Name: "multivless", LogLevel: "info", DataDir: "/var/lib/multivless"},
		Engine: Engine{
			Type:             "sing-box",
			Binary:           "/usr/bin/sing-box",
			ConfigPath:       "/var/lib/multivless/sing-box.json",
			ClashAPI:         "127.0.0.1:19091",
			Lanes:            8,
			StartTimeout:     Duration(15 * time.Second),
			RestartOnFailure: true,
			RestartDelay:     Duration(5 * time.Second),
			LogLevel:         "warn",
		},
		SOCKS5: SOCKS5{
			Enabled:           true,
			Listen:            "127.0.0.1",
			Port:              1080,
			MaxConnections:    10000,
			ConnectionTimeout: Duration(30 * time.Second),
			IdleTimeout:       Duration(5 * time.Minute),
			Network:           "tcp",
		},
		Balancer: Balancer{
			Strategy:          StrategyAdaptive,
			FallbackStrategy:  StrategyRoundRobin,
			MaxAttempts:       2,
			DegradedLatencyMS: 500,
			Weights:           Weights{Latency: 0.30, Connections: 0.25, Throughput: 0.35, Reliability: 0.10},
		},
		HealthCheck: Health{
			Enabled:            true,
			Interval:           Duration(20 * time.Second),
			Timeout:            Duration(5 * time.Second),
			FailuresBeforeDown: 3,
			SuccessesBeforeUp:  2,
			Concurrency:        20,
			URL:                "https://www.gstatic.com/generate_204",
			FallbackURLs:       []string{"https://www.google.com/generate_204"},
		},
		GeoIP:   GeoIP{Path: "/etc/multivless/geoip.dat"},
		Storage: Storage{Path: "/var/lib/multivless/multivless.db"},
		API: API{
			Enabled: true,
			Listen:  "127.0.0.1",
			Port:    8080,
		},
		Subscriptions: Subscriptions{
			Interval: Duration(6 * time.Hour),
			Timeout:  Duration(30 * time.Second),
			MaxSize:  2 << 20,
		},
	}
}

// Load reads the YAML file, applies defaults for unset fields and validates it.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	// Decode into the defaulted struct so that omitted fields keep defaults.
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate reports the first configuration problem it finds.
func (c Config) Validate() error {
	if c.App.Name == "" {
		return fmt.Errorf("app.name is required")
	}
	switch c.App.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("app.log_level: unknown level %q", c.App.LogLevel)
	}

	if c.Engine.Binary == "" {
		return fmt.Errorf("engine.binary is required")
	}
	if c.Engine.ConfigPath == "" {
		return fmt.Errorf("engine.config_path is required")
	}
	if c.Engine.Lanes < 1 || c.Engine.Lanes > 256 {
		return fmt.Errorf("engine.lanes must be between 1 and 256, got %d", c.Engine.Lanes)
	}
	if c.Engine.ClashAPI == "" {
		return fmt.Errorf("engine.clash_api is required")
	}
	if _, _, err := net.SplitHostPort(c.Engine.ClashAPI); err != nil {
		return fmt.Errorf("engine.clash_api: %w", err)
	}
	if c.Engine.StartTimeout.Std() <= 0 {
		return fmt.Errorf("engine.start_timeout must be positive")
	}

	if !c.SOCKS5.Enabled {
		return fmt.Errorf("socks5.enabled=false: proxy gateway is the only purpose of the service")
	}
	if c.SOCKS5.Network != "" && c.SOCKS5.Network != "tcp" {
		return fmt.Errorf("socks5.network: only tcp is supported in this version, got %q", c.SOCKS5.Network)
	}
	if c.SOCKS5.Port < 1 || c.SOCKS5.Port > 65535 {
		return fmt.Errorf("socks5.port out of range: %d", c.SOCKS5.Port)
	}
	if c.SOCKS5.MaxConnections < 1 {
		return fmt.Errorf("socks5.max_connections must be positive")
	}
	if c.SOCKS5.Authentication && len(c.SOCKS5.Users) == 0 {
		return fmt.Errorf("socks5.authentication=true requires at least one user")
	}
	if !c.SOCKS5.Authentication && !c.SOCKS5.AllowNoAuth {
		return fmt.Errorf("socks5: enable authentication or allow_no_auth explicitly")
	}
	seen := map[string]bool{}
	for i, u := range c.SOCKS5.Users {
		if u.Username == "" || u.Password == "" {
			return fmt.Errorf("socks5.users[%d]: username and password are required", i)
		}
		if seen[u.Username] {
			return fmt.Errorf("socks5.users[%d]: duplicate username %q", i, u.Username)
		}
		seen[u.Username] = true
	}
	if c.SOCKS5.Listen == "" {
		return fmt.Errorf("socks5.listen is required")
	}
	if ip := net.ParseIP(c.SOCKS5.Listen); ip == nil {
		return fmt.Errorf("socks5.listen must be an IP address, got %q", c.SOCKS5.Listen)
	} else if !ip.IsLoopback() && !c.SOCKS5.Authentication && !c.SOCKS5.AllowNoAuth {
		return fmt.Errorf("socks5.listen=%s is not loopback: authentication is required", c.SOCKS5.Listen)
	}

	switch c.Balancer.Strategy {
	case StrategyAdaptive, StrategyRoundRobin, StrategyRandom,
		StrategyLeastLatency, StrategyLeastConnections, StrategyWeighted:
	default:
		return fmt.Errorf("balancer.strategy: unknown strategy %q", c.Balancer.Strategy)
	}
	switch c.Balancer.FallbackStrategy {
	case "", StrategyAdaptive, StrategyRoundRobin, StrategyRandom,
		StrategyLeastLatency, StrategyLeastConnections, StrategyWeighted:
	default:
		return fmt.Errorf("balancer.fallback_strategy: unknown strategy %q", c.Balancer.FallbackStrategy)
	}
	if c.Balancer.MaxAttempts < 1 || c.Balancer.MaxAttempts > 10 {
		return fmt.Errorf("balancer.max_attempts must be between 1 and 10")
	}
	if sum := c.Balancer.Weights.Sum(); sum <= 0 {
		return fmt.Errorf("balancer.weights must have a positive total")
	}

	if !c.HealthCheck.Enabled {
		return fmt.Errorf("health_check.enabled=false: fail-closed operation requires health checks")
	}
	if c.HealthCheck.Interval.Std() < 5*time.Second {
		return fmt.Errorf("health_check.interval must be at least 5s")
	}
	if c.HealthCheck.Timeout.Std() <= 0 {
		return fmt.Errorf("health_check.timeout must be positive")
	}
	if c.HealthCheck.FailuresBeforeDown < 1 {
		return fmt.Errorf("health_check.failures_before_offline must be at least 1")
	}
	if c.HealthCheck.SuccessesBeforeUp < 1 {
		return fmt.Errorf("health_check.successes_before_online must be at least 1")
	}
	if c.HealthCheck.Concurrency < 1 || c.HealthCheck.Concurrency > 256 {
		return fmt.Errorf("health_check.concurrency must be between 1 and 256")
	}
	if err := validateURL(c.HealthCheck.URL); err != nil {
		return fmt.Errorf("health_check.url: %w", err)
	}
	for i, u := range c.HealthCheck.FallbackURLs {
		if err := validateURL(u); err != nil {
			return fmt.Errorf("health_check.fallback_urls[%d]: %w", i, err)
		}
	}

	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}
	if c.API.Enabled {
		if c.API.Port < 1 || c.API.Port > 65535 {
			return fmt.Errorf("api.port out of range: %d", c.API.Port)
		}
		if ip := net.ParseIP(c.API.Listen); ip == nil {
			return fmt.Errorf("api.listen must be an IP address, got %q", c.API.Listen)
		} else if !ip.IsLoopback() && !c.SOCKS5.Authentication {
			return fmt.Errorf("api.listen=%s is not loopback: enable SOCKS5 authentication", c.API.Listen)
		}
	}
	if c.Subscriptions.MaxSize < 1024 {
		return fmt.Errorf("subscriptions.max_size must be at least 1024 bytes")
	}
	if c.Subscriptions.Interval.Std() < 10*time.Second {
		return fmt.Errorf("subscriptions.interval must be at least 10s")
	}

	// Servers may also come from the database (subscriptions), so an empty
	// servers list is not an error: fail-closed keeps the gateway idle until
	// they arrive.
	return nil
}

func validateURL(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("must start with http:// or https://")
	}
	if _, err := url.Parse(raw); err != nil {
		return err
	}
	return nil
}
