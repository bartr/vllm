package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cplane/internal/config"
	"cplane/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	handler := httpapi.NewHandlerWithDependencies("", nil, cfg.CacheSize)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		logger.Info("server starting", "addr", cfg.Addr)
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
			os.Exit(1)
		}
		return
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}

	if err := <-serverErrCh; err != nil {
		logger.Error("server returned an error during shutdown", "err", err)
		os.Exit(1)
	}

	logger.Info("server stopped")
}
