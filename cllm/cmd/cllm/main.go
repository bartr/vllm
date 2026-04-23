package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cllm/internal/buildinfo"
	"cllm/internal/config"
	"cllm/internal/httpapi"
)

const queueDepthLogInterval = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		_, _ = fmt.Fprint(stdout, config.Usage())
		return 0
	}
	if hasVersionFlag(args) {
		_, _ = fmt.Fprintf(stdout, "cllm %s\n", buildinfo.Version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.LoadFromArgs(args)
	if err != nil {
		logger.Error("load config", "err", err)
		return 1
	}

	resolvedModel, err := resolveStartupDownstreamModel(context.Background(), logger, cfg)
	if err != nil {
		logger.Error("resolve downstream model", "err", err, "downstream_url", cfg.DownstreamURL)
		return 1
	}
	if resolvedModel != "" {
		cfg.DownstreamModel = resolvedModel
	}

	handler := httpapi.NewHandlerWithDependencies(cfg.DownstreamURL, nil, cfg.CacheSize, httpapi.NewAskOptions(cfg.SystemPrompt, cfg.MaxTokens, cfg.Temperature))
	handler.SetDownstreamToken(cfg.DownstreamToken)
	handler.SetDownstreamModel(cfg.DownstreamModel)
	handler.SetRequestProcessingLimits(cfg.MaxTokensPerSecond, cfg.MaxConcurrentRequests, cfg.MaxWaitingRequests, cfg.MaxDegradation)
	server := newServer(cfg, handler.Routes())
	queueLogCtx, stopQueueLogger := context.WithCancel(context.Background())
	defer stopQueueLogger()
	go startQueueDepthLogger(queueLogCtx, logger, handler, queueDepthLogInterval)

	serverErrCh := make(chan error, 1)
	go func() {
		_, concurrentRequests, _, waitingRequests := handler.RequestQueueStats()
		logger.Info(
			"server starting",
			"addr", cfg.Addr,
			"cache_size", cfg.CacheSize,
			"downstream_url", cfg.DownstreamURL,
			"downstream_model", cfg.DownstreamModel,
			"system_prompt", cfg.SystemPrompt,
			"max_tokens", cfg.MaxTokens,
			"max_tokens_per_second", cfg.MaxTokensPerSecond,
			"max_concurrent_requests", cfg.MaxConcurrentRequests,
			"concurrent_requests", concurrentRequests,
			"max_waiting_requests", cfg.MaxWaitingRequests,
			"waiting_requests", waitingRequests,
			"max_degradation", cfg.MaxDegradation,
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

func startQueueDepthLogger(ctx context.Context, logger *slog.Logger, handler *httpapi.Handler, interval time.Duration) {
	if interval <= 0 {
		interval = queueDepthLogInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maxConcurrentRequests, concurrentRequests, maxWaitingRequests, waitingRequests := handler.RequestQueueStats()
			logger.Info(
				"queue depth",
				"concurrent_requests", concurrentRequests,
				"max_concurrent_requests", maxConcurrentRequests,
				"waiting_requests", waitingRequests,
				"max_waiting_requests", maxWaitingRequests,
			)
		}
	}
}

func newServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
	}
}

type modelsListResponse struct {
	Data []struct {
		ID        string `json:"id"`
		Default   bool   `json:"default"`
		IsDefault bool   `json:"is_default"`
	} `json:"data"`
}

func resolveStartupDownstreamModel(ctx context.Context, logger *slog.Logger, cfg config.Config) (string, error) {
	if cfg.DownstreamModel != "" {
		return cfg.DownstreamModel, nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.DownstreamURL, "/")+"/v1/models", nil)
	if err != nil {
		return "", fmt.Errorf("build models request: %w", err)
	}
	if cfg.DownstreamToken != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.DownstreamToken)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("send models request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read models response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("models request failed with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var modelsResponse modelsListResponse
	if err := json.Unmarshal(body, &modelsResponse); err != nil {
		return "", fmt.Errorf("decode models response: %w", err)
	}

	modelIDs := make([]string, 0, len(modelsResponse.Data))
	defaultModel := ""
	for _, model := range modelsResponse.Data {
		if model.ID == "" {
			continue
		}
		modelIDs = append(modelIDs, model.ID)
		if defaultModel == "" && (model.Default || model.IsDefault) {
			defaultModel = model.ID
		}
	}
	if len(modelIDs) == 0 {
		return "", fmt.Errorf("models response did not include a model id")
	}

	selectedModel := defaultModel
	selectionSource := "default"
	if selectedModel == "" {
		selectedModel = modelIDs[0]
		selectionSource = "first"
	}

	logger.Info(
		"resolved downstream model from downstream /v1/models",
		"selected_model", selectedModel,
		"selection_source", selectionSource,
		"downstream_url", cfg.DownstreamURL,
	)
	if len(modelIDs) > 1 {
		logger.Info(
			"multiple downstream models available",
			"selected_model", selectedModel,
			"available_models", slices.Clone(modelIDs),
		)
	}

	return selectedModel, nil
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
