package config

import (
	"os"
	"testing"
)

func TestIsThinkingModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"kimi-k2-thinking", true},
		{"qwen3-vl-30b-a3b-thinking", true},
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
		{"kimi-k2-thinking", true},
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
	// Clear env to test pure defaults
	os.Unsetenv("PORT")
	os.Unsetenv("HOST")
	os.Unsetenv("API_KEY")
	os.Unsetenv("TIMEOUT")
	os.Unsetenv("LOG_LEVEL")
	os.Unsetenv("SHOW_THINKING")
	os.Unsetenv("CORS_ORIGINS")
	os.Unsetenv("RATE_LIMIT")

	cfg := Load()
	if cfg.Port != 39527 {
		t.Errorf("default Port = %d, want 39527", cfg.Port)
	}
	if cfg.APIKey != "sk-admin" {
		t.Errorf("default APIKey = %q, want sk-admin", cfg.APIKey)
	}
	if cfg.Timeout != 120 {
		t.Errorf("default Timeout = %d, want 120", cfg.Timeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.ShowThinking != false {
		t.Error("default ShowThinking = true, want false")
	}
	if cfg.CORSOrigins != "*" {
		t.Errorf("default CORSOrigins = %q, want *", cfg.CORSOrigins)
	}
	if cfg.RateLimit != 60 {
		t.Errorf("default RateLimit = %d, want 60", cfg.RateLimit)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	// Use t.Setenv which auto-restores and is safe for parallel tests
	t.Setenv("PORT", "9999")
	t.Setenv("API_KEY", "test-key")
	t.Setenv("TIMEOUT", "300")
	t.Setenv("SHOW_THINKING", "true")
	t.Setenv("CORS_ORIGINS", "https://example.com")
	t.Setenv("RATE_LIMIT", "100")

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
	if cfg.ShowThinking != true {
		t.Error("ShowThinking = false, want true")
	}
	if cfg.CORSOrigins != "https://example.com" {
		t.Errorf("CORSOrigins = %q", cfg.CORSOrigins)
	}
	if cfg.RateLimit != 100 {
		t.Errorf("RateLimit = %d, want 100", cfg.RateLimit)
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

func TestAddr(t *testing.T) {
	cfg := &Config{Port: 8080, Host: ""}
	if got := cfg.Addr(); got != ":8080" {
		t.Errorf("Addr() = %q, want :8080", got)
	}
	cfg.Host = "127.0.0.1"
	if got := cfg.Addr(); got != "127.0.0.1:8080" {
		t.Errorf("Addr() = %q, want 127.0.0.1:8080", got)
	}
}
