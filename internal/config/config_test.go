package config

import (
	"os"
	"testing"
)

func unsetEnv(t *testing.T, key string) {
	t.Helper()

	prev, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q): %v", key, err)
	}

	t.Cleanup(func() {
		var err error
		if existed {
			err = os.Setenv(key, prev)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Fatalf("restore env %q: %v", key, err)
		}
	})
}

func TestIsThinkingModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"qwen3-vl-30b-a3b-thinking", true},
		{"qwen3-8b", true},
		{"deepseek-v3p2", false},
		{"", false},
		{"kimi-k2-thinking-extra", false},
	}
	for _, tt := range tests {
		if got := IsThinkingModel(tt.model); got != tt.want {
			t.Errorf("IsThinkingModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestValidModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"deepseek-v3p2", true},
		{"kimi-k2-thinking", false},
		{"minimax-m2p1", false},
		{"kimi-k2-instruct-0905", false},
		{"nonexistent-model", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ValidModel(tt.model); got != tt.want {
			t.Errorf("ValidModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestValidModel_AllAvailableModels(t *testing.T) {
	for _, m := range AvailableModels {
		if !ValidModel(m) {
			t.Errorf("AvailableModels contains %q but ValidModel returns false", m)
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	for _, key := range []string{"PORT", "HOST", "API_KEY", "TIMEOUT", "LOG_LEVEL", "SHOW_THINKING", "CORS_ORIGINS", "RATE_LIMIT", "IP_WHITELIST", "TRUSTED_PROXY_COUNT"} {
		unsetEnv(t, key)
	}

	cfg := Load()
	if cfg.Port != defaultPort {
		t.Errorf("default Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.Host != "" {
		t.Errorf("default Host = %q, want empty", cfg.Host)
	}
	if cfg.APIKey != "sk-admin" {
		t.Errorf("default APIKey = %q, want sk-admin", cfg.APIKey)
	}
	if cfg.Timeout != defaultTimeout {
		t.Errorf("default Timeout = %d, want %d", cfg.Timeout, defaultTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.ShowThinking {
		t.Error("default ShowThinking = true, want false")
	}
	if cfg.CORSOrigins != "*" {
		t.Errorf("default CORSOrigins = %q, want *", cfg.CORSOrigins)
	}
	if cfg.RateLimit != defaultRateLimit {
		t.Errorf("default RateLimit = %d, want %d", cfg.RateLimit, defaultRateLimit)
	}
	if cfg.IPWhitelist != "127.0.0.1,::1" {
		t.Errorf("default IPWhitelist = %q, want 127.0.0.1,::1", cfg.IPWhitelist)
	}
	if cfg.TrustedProxyCount != 0 {
		t.Errorf("default TrustedProxyCount = %d, want 0", cfg.TrustedProxyCount)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("API_KEY", "test-key")
	t.Setenv("TIMEOUT", "300")
	t.Setenv("SHOW_THINKING", "true")
	t.Setenv("CORS_ORIGINS", "https://example.com")
	t.Setenv("RATE_LIMIT", "100")
	t.Setenv("IP_WHITELIST", "10.0.0.0/8,192.168.0.0/16")

	cfg := Load()
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want test-key", cfg.APIKey)
	}
	if cfg.Timeout != 300 {
		t.Errorf("Timeout = %d, want 300", cfg.Timeout)
	}
	if !cfg.ShowThinking {
		t.Error("ShowThinking = false, want true")
	}
	if cfg.CORSOrigins != "https://example.com" {
		t.Errorf("CORSOrigins = %q", cfg.CORSOrigins)
	}
	if cfg.RateLimit != 100 {
		t.Errorf("RateLimit = %d, want 100", cfg.RateLimit)
	}
	if cfg.IPWhitelist != "10.0.0.0/8,192.168.0.0/16" {
		t.Errorf("IPWhitelist = %q, want 10.0.0.0/8,192.168.0.0/16", cfg.IPWhitelist)
	}
}

func TestLoad_EmptyIPWhitelistOverride(t *testing.T) {
	t.Setenv("IP_WHITELIST", "")

	cfg := Load()
	if cfg.IPWhitelist != "" {
		t.Errorf("IPWhitelist = %q, want empty string when env is explicitly empty", cfg.IPWhitelist)
	}
}

func TestLoad_InvalidPortIgnored(t *testing.T) {
	t.Setenv("PORT", "not-a-number")
	cfg := Load()
	// Should fall back to default, not crash
	if cfg.Port != 39527 {
		t.Errorf("Port = %d, want default 39527 after invalid env", cfg.Port)
	}
}

func TestLoad_NegativeTimeoutIgnored(t *testing.T) {
	t.Setenv("TIMEOUT", "-1")
	cfg := Load()
	if cfg.Timeout != 120 {
		t.Errorf("Timeout = %d, want default 120 after negative value", cfg.Timeout)
	}
}

func TestLoad_ZeroTimeoutIgnored(t *testing.T) {
	t.Setenv("TIMEOUT", "0")
	cfg := Load()
	if cfg.Timeout != 120 {
		t.Errorf("Timeout = %d, want default 120 after zero value", cfg.Timeout)
	}
}

func TestApplyFlags_NegativeTimeout(t *testing.T) {
	cfg := Load() // start with defaults
	cfg.ApplyFlags([]string{"prog", "-timeout", "-5"})
	if cfg.Timeout != 120 {
		t.Errorf("Timeout = %d, want default 120 after negative flag", cfg.Timeout)
	}
}

func TestApplyFlags_ZeroTimeout(t *testing.T) {
	cfg := Load()
	cfg.ApplyFlags([]string{"prog", "-timeout", "0"})
	if cfg.Timeout != 120 {
		t.Errorf("Timeout = %d, want default 120 after zero flag", cfg.Timeout)
	}
}

func TestApplyFlags_InvalidPortRange(t *testing.T) {
	cfg := Load()

	cfg.ApplyFlags([]string{"prog", "-port", "0"})
	if cfg.Port != 39527 {
		t.Errorf("Port = %d, want default 39527 after zero flag", cfg.Port)
	}

	cfg = Load()
	cfg.ApplyFlags([]string{"prog", "-port", "70000"})
	if cfg.Port != 39527 {
		t.Errorf("Port = %d, want default 39527 after out-of-range flag", cfg.Port)
	}
}

func TestApplyFlags_NegativeRateLimit(t *testing.T) {
	cfg := Load()
	cfg.ApplyFlags([]string{"prog", "-rate-limit", "-5"})
	if cfg.RateLimit != defaultRateLimit {
		t.Errorf("RateLimit = %d, want %d after negative flag", cfg.RateLimit, defaultRateLimit)
	}
}

func TestApplyFlags_NegativeTrustedProxyCount(t *testing.T) {
	cfg := Load()
	cfg.ApplyFlags([]string{"prog", "-trusted-proxy-count", "-2"})
	if cfg.TrustedProxyCount != 0 {
		t.Errorf("TrustedProxyCount = %d, want 0 after negative flag", cfg.TrustedProxyCount)
	}
}

func TestAddr(t *testing.T) {
	cfg := &Config{Port: 8080, Host: ""}
	if got := cfg.Addr(); got != ":8080" {
		t.Errorf("Addr() = %q, want :8080", got)
	}

	cfg.Host = "127.0.0.1"
	if got := cfg.Addr(); got != "127.0.0.1:8080" {
		t.Errorf("Addr() = %q, want 127.0.0.1:8080", got)
	}

	cfg.Host = "::1"
	if got := cfg.Addr(); got != "[::1]:8080" {
		t.Errorf("Addr() = %q, want [::1]:8080", got)
	}
}
