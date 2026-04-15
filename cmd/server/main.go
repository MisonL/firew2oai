package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		"ip_whitelist", cfg.IPWhitelist,
		"models", len(config.AvailableModels),
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

	// Create proxy handler
	p := proxy.New(tp, timeout, Version)
	handler := proxy.NewMux(p, cfg.CORSOrigins, cfg.APIKey, tm)

	// Wrap with IP whitelist (applied first)
	if cfg.IPWhitelist != "" {
		wl, err := whitelist.New(cfg.IPWhitelist)
		if err != nil {
			slog.Error("invalid IP whitelist configuration", "whitelist", cfg.IPWhitelist, "error", err)
			os.Exit(1)
		}
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wl.Middleware()(func(w2 http.ResponseWriter, r2 *http.Request) {
				inner.ServeHTTP(w2, r2)
			}).ServeHTTP(w, r)
		})
		slog.Info("IP whitelist enabled", "whitelist", cfg.IPWhitelist)
	}

	// Create HTTP server with timeouts
	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
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
