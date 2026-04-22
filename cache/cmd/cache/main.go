package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cache/internal/buildinfo"
	"cache/internal/config"
	"cache/internal/httpapi"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		_, _ = fmt.Fprint(stdout, config.Usage())
		return 0
	}
	if hasVersionFlag(args) {
		_, _ = fmt.Fprintf(stdout, "cache %s\n", buildinfo.Version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.LoadFromArgs(args)
	if err != nil {
		logger.Error("load config", "err", err)
		return 1
	}

	handler := httpapi.NewHandlerWithDependencies("", nil, cfg.CacheSize, httpapi.NewAskOptions(cfg.SystemPrompt, cfg.MaxTokens, cfg.Temperature))
	handler.SetModelsCacheTTL(cfg.ModelsCacheTTL)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		logger.Info(
			"server starting",
			"addr", cfg.Addr,
			"cache_size", cfg.CacheSize,
			"models_cache_ttl", cfg.ModelsCacheTTL,
			"system_prompt", cfg.SystemPrompt,
			"max_tokens", cfg.MaxTokens,
			"temperature", cfg.Temperature,
		)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
			return
		}
		serverErrCh <- nil
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErrCh:
		if err != nil {
			logger.Error("server stopped unexpectedly", "err", err)
			return 1
		}
		return 0
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		return 1
	}

	if err := <-serverErrCh; err != nil {
		logger.Error("server returned an error during shutdown", "err", err)
		return 1
	}

	logger.Info("server stopped")
	return 0
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func hasVersionFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--version" {
			return true
		}
	}
	return false
}
