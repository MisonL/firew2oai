package config

import (
	"flag"
	"fmt"
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

// IsThinkingModel checks if the model name indicates a thinking/reasoning model.
func IsThinkingModel(model string) bool {
	thinkingModels := map[string]bool{
		"qwen3-vl-30b-a3b-thinking": true,
		"kimi-k2-thinking":          true,
	}
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

	// Environment variables
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
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
		}
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("SHOW_THINKING"); v != "" {
		cfg.ShowThinking = strings.ToLower(v) == "true" || v == "1"
	}

	// Command-line flags (override env)
	flag.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	flag.StringVar(&cfg.Host, "host", cfg.Host, "listen host (default: all interfaces)")
	flag.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "API key for authentication")
	flag.IntVar(&cfg.Timeout, "timeout", cfg.Timeout, "upstream request timeout in seconds")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug, info, warn, error")
	flag.BoolVar(&cfg.ShowThinking, "show-thinking", cfg.ShowThinking, "show thinking process for thinking models")
	flag.Parse()

	return cfg
}

// Addr returns the listen address string.
func (c *Config) Addr() string {
	if c.Host != "" {
		return fmt.Sprintf("%s:%d", c.Host, c.Port)
	}
	return fmt.Sprintf(":%d", c.Port)
}

// TimeoutDuration returns the timeout as a time.Duration.
func (c *Config) TimeoutDuration() int {
	return c.Timeout
}
