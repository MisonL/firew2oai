package config

import (
	"flag"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config holds all service configuration.
type Config struct {
	Port                        int
	Host                        string
	APIKey                      string // comma-separated keys, JSON file path, or inline JSON
	Timeout                     int    // seconds
	UpstreamRetryCount          int    // retry count for transient upstream failures before first byte
	UpstreamRetryBackoffMS      int    // base backoff in milliseconds for transient upstream retries
	UpstreamEmptyRetryCount     int    // retry count for 200 OK upstream responses that end before any content/done signal
	UpstreamEmptyRetryBackoffMS int    // base backoff in milliseconds for empty upstream retries
	LogLevel                    string
	ShowThinking                bool // default: show thinking process for thinking models
	CORSOrigins                 string
	RateLimit                   int    // global rate limit: max requests per minute per key (0 = disabled)
	IPWhitelist                 string // comma-separated IPs/CIDRs; default "127.0.0.1,::1" (loopback only); set "" or "0.0.0.0/0,::/0" to allow all
	TrustedProxyCount           int    // number of trusted reverse proxies (0 = trust none, use RemoteAddr)
}

const (
	defaultPort                        = 39527
	defaultTimeout                     = 120
	defaultUpstreamRetryCount          = 1
	defaultUpstreamRetryBackoffMS      = 200
	defaultUpstreamEmptyRetryCount     = 1
	defaultUpstreamEmptyRetryBackoffMS = 200
	defaultRateLimit                   = 0
	minPort                            = 1
	maxPort                            = 65535
)

var AvailableModels = []string{
	"qwen3-vl-30b-a3b-thinking",
	"qwen3-vl-30b-a3b-instruct",
	"qwen3-8b",
	"minimax-m2p5",
	"llama-v3p3-70b-instruct",
	"kimi-k2p5",
	"gpt-oss-20b",
	"gpt-oss-120b",
	"glm-5",
	"glm-4p7",
	"deepseek-v3p2",
	"deepseek-v3p1",
	// Removed after live checks on 2026-04-17:
	// minimax-m2p1, kimi-k2-thinking, kimi-k2-instruct-0905,
	// cogito-671b-v2-p1 all returned upstream Fireworks 404.
	// cogito-671b-v2-p1 removed: upstream returns 404 since 2026-04-15
}

// availableModelSet is a lookup map for O(1) model validation.
var availableModelSet map[string]bool

func init() {
	availableModelSet = make(map[string]bool, len(AvailableModels))
	for _, m := range AvailableModels {
		availableModelSet[m] = true
	}
}

// thinkingModels is the set of models that produce a thinking block
// before the actual response, separated by the 💯 emoji.
var thinkingModels = map[string]bool{
	"qwen3-vl-30b-a3b-thinking": true,
	"qwen3-8b":                  true,
}

// IsThinkingModel checks if the model name indicates a thinking/reasoning model.
func IsThinkingModel(model string) bool {
	return thinkingModels[model]
}

// ValidModel checks if a model is in the supported list.
func ValidModel(model string) bool {
	return availableModelSet[model]
}

// Load reads configuration from environment variables only.
// Command-line flags should be parsed separately and passed via ApplyFlags.
func Load() *Config {
	cfg := &Config{}

	// Defaults
	cfg.Port = defaultPort
	cfg.Host = ""
	cfg.APIKey = "sk-admin"
	cfg.Timeout = defaultTimeout
	cfg.UpstreamRetryCount = defaultUpstreamRetryCount
	cfg.UpstreamRetryBackoffMS = defaultUpstreamRetryBackoffMS
	cfg.UpstreamEmptyRetryCount = defaultUpstreamEmptyRetryCount
	cfg.UpstreamEmptyRetryBackoffMS = defaultUpstreamEmptyRetryBackoffMS
	cfg.LogLevel = "info"
	cfg.ShowThinking = false
	cfg.CORSOrigins = "*"
	cfg.RateLimit = defaultRateLimit  // disabled by default; set >0 to enable
	cfg.IPWhitelist = "127.0.0.1,::1" // default: loopback only
	cfg.TrustedProxyCount = 0         // default: trust no proxy, use RemoteAddr

	// Environment variables
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n >= minPort && n <= maxPort {
				cfg.Port = n
			} else {
				slog.Warn("PORT out of range, using default", "value", v, "min", minPort, "max", maxPort)
			}
		} else {
			slog.Warn("invalid PORT value, using default", "value", v)
		}
	}
	if v := os.Getenv("HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = n
		} else if n <= 0 {
			slog.Warn("TIMEOUT must be positive, using default", "value", v)
		} else {
			slog.Warn("invalid TIMEOUT value, using default", "value", v)
		}
	}
	if v := os.Getenv("UPSTREAM_RETRY_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.UpstreamRetryCount = n
		} else {
			slog.Warn("invalid UPSTREAM_RETRY_COUNT value, using default", "value", v)
		}
	}
	if v := os.Getenv("UPSTREAM_RETRY_BACKOFF_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.UpstreamRetryBackoffMS = n
		} else {
			slog.Warn("invalid UPSTREAM_RETRY_BACKOFF_MS value, using default", "value", v)
		}
	}
	if v := os.Getenv("UPSTREAM_EMPTY_RETRY_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.UpstreamEmptyRetryCount = n
		} else {
			slog.Warn("invalid UPSTREAM_EMPTY_RETRY_COUNT value, using default", "value", v)
		}
	}
	if v := os.Getenv("UPSTREAM_EMPTY_RETRY_BACKOFF_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.UpstreamEmptyRetryBackoffMS = n
		} else {
			slog.Warn("invalid UPSTREAM_EMPTY_RETRY_BACKOFF_MS value, using default", "value", v)
		}
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("SHOW_THINKING"); v != "" {
		cfg.ShowThinking = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		cfg.CORSOrigins = v
	}
	if v := os.Getenv("RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n >= 0 {
				cfg.RateLimit = n
			} else {
				slog.Warn("RATE_LIMIT must be non-negative, using default", "value", v)
			}
		} else {
			slog.Warn("invalid RATE_LIMIT value, using default", "value", v)
		}
	}
	if v, ok := os.LookupEnv("IP_WHITELIST"); ok {
		cfg.IPWhitelist = v
	}
	if v := os.Getenv("TRUSTED_PROXY_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.TrustedProxyCount = n
		} else {
			slog.Warn("invalid TRUSTED_PROXY_COUNT value, using default", "value", v)
		}
	}

	return cfg
}

// ApplyFlags parses command-line flags and overrides config values.
// This is called from main() to avoid flag pollution in tests.
func (c *Config) ApplyFlags(args []string) {
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	fs.IntVar(&c.Port, "port", c.Port, "listen port")
	fs.StringVar(&c.Host, "host", c.Host, "listen host (default: all interfaces)")
	fs.StringVar(&c.APIKey, "api-key", c.APIKey, "API key for authentication")
	fs.IntVar(&c.Timeout, "timeout", c.Timeout, "upstream request timeout in seconds")
	fs.IntVar(&c.UpstreamRetryCount, "upstream-retry-count", c.UpstreamRetryCount, "retry count for transient upstream 429/502/503/504 before first byte")
	fs.IntVar(&c.UpstreamRetryBackoffMS, "upstream-retry-backoff-ms", c.UpstreamRetryBackoffMS, "base backoff in milliseconds for transient upstream retries")
	fs.IntVar(&c.UpstreamEmptyRetryCount, "upstream-empty-retry-count", c.UpstreamEmptyRetryCount, "retry count for upstream 200 responses that end before any content or done signal")
	fs.IntVar(&c.UpstreamEmptyRetryBackoffMS, "upstream-empty-retry-backoff-ms", c.UpstreamEmptyRetryBackoffMS, "base backoff in milliseconds for empty upstream retries")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn, error")
	fs.BoolVar(&c.ShowThinking, "show-thinking", c.ShowThinking, "show thinking process for thinking models")
	fs.StringVar(&c.CORSOrigins, "cors-origins", c.CORSOrigins, "allowed CORS origins (comma-separated, * for all)")
	fs.IntVar(&c.RateLimit, "rate-limit", c.RateLimit, "max requests per minute per key (0 to disable)")
	fs.StringVar(&c.IPWhitelist, "ip-whitelist", c.IPWhitelist, "allowed IPs/CIDRs (comma-separated, empty to allow all)")
	fs.IntVar(&c.TrustedProxyCount, "trusted-proxy-count", c.TrustedProxyCount, "number of trusted reverse proxies for X-Forwarded-For (0 = trust none)")
	_ = fs.Parse(args[1:])
	if c.Port < minPort || c.Port > maxPort {
		slog.Warn("port out of range, using default", "value", c.Port, "min", minPort, "max", maxPort)
		c.Port = defaultPort
	}
	if c.Timeout <= 0 {
		slog.Warn("timeout must be positive, using default", "value", c.Timeout)
		c.Timeout = defaultTimeout
	}
	if c.UpstreamRetryCount < 0 {
		slog.Warn("upstream-retry-count must be non-negative, using default", "value", c.UpstreamRetryCount)
		c.UpstreamRetryCount = defaultUpstreamRetryCount
	}
	if c.UpstreamRetryBackoffMS < 0 {
		slog.Warn("upstream-retry-backoff-ms must be non-negative, using default", "value", c.UpstreamRetryBackoffMS)
		c.UpstreamRetryBackoffMS = defaultUpstreamRetryBackoffMS
	}
	if c.UpstreamEmptyRetryCount < 0 {
		slog.Warn("upstream-empty-retry-count must be non-negative, using default", "value", c.UpstreamEmptyRetryCount)
		c.UpstreamEmptyRetryCount = defaultUpstreamEmptyRetryCount
	}
	if c.UpstreamEmptyRetryBackoffMS < 0 {
		slog.Warn("upstream-empty-retry-backoff-ms must be non-negative, using default", "value", c.UpstreamEmptyRetryBackoffMS)
		c.UpstreamEmptyRetryBackoffMS = defaultUpstreamEmptyRetryBackoffMS
	}
	if c.RateLimit < 0 {
		slog.Warn("rate-limit must be non-negative, clamping to 0", "value", c.RateLimit)
		c.RateLimit = defaultRateLimit
	}
	if c.TrustedProxyCount < 0 {
		slog.Warn("trusted-proxy-count must be non-negative, clamping to 0", "value", c.TrustedProxyCount)
		c.TrustedProxyCount = 0
	}
}

// Addr returns the listen address string.
func (c *Config) Addr() string {
	if c.Host != "" {
		return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	return ":" + strconv.Itoa(c.Port)
}
