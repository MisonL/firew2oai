package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mison/firew2oai/internal/config"
	"github.com/mison/firew2oai/internal/proxy"
	"github.com/mison/firew2oai/internal/tokenauth"
	"github.com/mison/firew2oai/internal/transport"
	"github.com/mison/firew2oai/internal/whitelist"
)

// Version is injected at build time via -ldflags "-X main.Version=x.y.z"
var Version = "dev"

func main() {
	cfg := config.Load()
	cfg.ApplyFlags(os.Args)

	// Setup structured logger
	level := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	slog.Info("starting firew2oai",
		"version", Version,
		"port", cfg.Port,
		"timeout", cfg.Timeout,
		"rate_limit", cfg.RateLimit,
		"cors_origins", cfg.CORSOrigins,
		"ip_whitelist", cfg.IPWhitelist,
		"models", len(config.AvailableModels),
		"gomaxprocs", runtime.GOMAXPROCS(0),
		"num_cpu", runtime.NumCPU(),
	)

	// Create transport with Chrome TLS fingerprint
	timeout := time.Duration(cfg.Timeout) * time.Second
	tp := transport.New(timeout)

	// Create token auth manager
	tm, err := tokenauth.New(cfg.APIKey, cfg.RateLimit)
	if err != nil {
		slog.Error("invalid token configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("token auth configured",
		"tokens", tm.TokenCount(),
		"global_rate_limit", cfg.RateLimit,
	)

	// Security warnings for overly permissive defaults
	if cfg.CORSOrigins == "*" {
		slog.Warn("CORS is set to wildcard (*) — any origin can access the API; restrict CORS_ORIGINS for production")
	}
	if cfg.IPWhitelist == "" {
		slog.Warn("IP whitelist is empty — all IPs are allowed; set IP_WHITELIST for production")
	}

	// Create proxy handler
	p := proxy.New(tp, Version, cfg.ShowThinking)
	handler := proxy.NewMux(p, cfg.CORSOrigins, tm)

	// Wrap with IP whitelist (applied first, constructed once at startup)
	if cfg.IPWhitelist != "" {
		wl, err := whitelist.New(cfg.IPWhitelist)
		if err != nil {
			slog.Error("invalid IP whitelist configuration", "whitelist", cfg.IPWhitelist, "error", err)
			os.Exit(1)
		}
		// Pre-construct the whitelist middleware handler once at startup
		// instead of re-creating closures on every request.
		// CRITICAL: capture the original handler before re-assignment to avoid
		// the closure capturing the reassigned variable (infinite recursion).
		originalHandler := handler
		handler = wl.Middleware(cfg.TrustedProxyCount)(func(w http.ResponseWriter, r *http.Request) {
			originalHandler.ServeHTTP(w, r)
		})
		slog.Info("IP whitelist enabled", "whitelist", cfg.IPWhitelist, "trusted_proxy_count", cfg.TrustedProxyCount)
	}

	// Create HTTP server with timeouts.
	// WriteTimeout is set to 0 (disabled) because SSE streaming responses
	// can last longer than any fixed timeout (especially for thinking models).
	// Actual timeout is enforced by the transport client and request context.
	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("server error: %w", err)
		}
		close(errCh)
	}()

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	select {
	case err := <-errCh:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	}

	// Give outstanding requests 15 seconds to finish
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	// Stop token auth rate limiters
	tm.Stop()

	slog.Info("server stopped")
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
