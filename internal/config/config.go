package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Config holds all service configuration.
type Config struct {
	Port         int
	Host         string
	APIKey       string
	Timeout      int // seconds
	LogLevel     string
	ShowThinking bool // default: show thinking process for thinking models
	CORSOrigins  string
	RateLimit    int // max requests per minute per IP (0 = disabled)
}

var AvailableModels = []string{
	"qwen3-vl-30b-a3b-thinking",
	"qwen3-vl-30b-a3b-instruct",
	"qwen3-8b",
	"minimax-m2p5",
	"minimax-m2p1",
	"llama-v3p3-70b-instruct",
	"kimi-k2p5",
	"kimi-k2-thinking",
	"kimi-k2-instruct-0905",
	"gpt-oss-20b",
	"gpt-oss-120b",
	"glm-5",
	"glm-4p7",
	"deepseek-v3p2",
	"deepseek-v3p1",
	"cogito-671b-v2-p1",
}

// thinkingModels is the set of models that produce a thinking block
// before the actual response, separated by the 💯 emoji.
var thinkingModels = map[string]bool{
	"qwen3-vl-30b-a3b-thinking": true,
	"kimi-k2-thinking":          true,
}

// IsThinkingModel checks if the model name indicates a thinking/reasoning model.
func IsThinkingModel(model string) bool {
	return thinkingModels[model]
}

// ValidModel checks if a model is in the supported list.
func ValidModel(model string) bool {
	for _, m := range AvailableModels {
		if m == model {
			return true
		}
	}
	return false
}

// Load reads configuration from environment variables and command-line flags.
// Flags take precedence over environment variables.
func Load() *Config {
	cfg := &Config{}

	// Defaults
	cfg.Port = 39527
	cfg.Host = ""
	cfg.APIKey = "sk-admin"
	cfg.Timeout = 120
	cfg.LogLevel = "info"
	cfg.ShowThinking = false
	cfg.CORSOrigins = "*"
	cfg.RateLimit = 60 // 60 req/min per IP

	// Environment variables
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
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
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeout = n
		} else {
			slog.Warn("invalid TIMEOUT value, using default", "value", v)
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
			cfg.RateLimit = n
		} else {
			slog.Warn("invalid RATE_LIMIT value, using default", "value", v)
		}
	}

	// Command-line flags (override env)
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	fs.StringVar(&cfg.Host, "host", cfg.Host, "listen host (default: all interfaces)")
	fs.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "API key for authentication")
	fs.IntVar(&cfg.Timeout, "timeout", cfg.Timeout, "upstream request timeout in seconds")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug, info, warn, error")
	fs.BoolVar(&cfg.ShowThinking, "show-thinking", cfg.ShowThinking, "show thinking process for thinking models")
	fs.StringVar(&cfg.CORSOrigins, "cors-origins", cfg.CORSOrigins, "allowed CORS origins (comma-separated, * for all)")
	fs.IntVar(&cfg.RateLimit, "rate-limit", cfg.RateLimit, "max requests per minute per IP (0 to disable)")
	_ = fs.Parse(os.Args[1:])

	return cfg
}

// Addr returns the listen address string.
func (c *Config) Addr() string {
	if c.Host != "" {
		return fmt.Sprintf("%s:%d", c.Host, c.Port)
	}
	return fmt.Sprintf(":%d", c.Port)
}

