package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr            string
	CacheSize       int
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	port := envOrDefault("PORT", "8080")
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return Config{}, fmt.Errorf("invalid PORT %q", port)
	}

	shutdownTimeoutRaw := envOrDefault("SHUTDOWN_TIMEOUT", "10s")
	shutdownTimeout, err := time.ParseDuration(shutdownTimeoutRaw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SHUTDOWN_TIMEOUT %q: %w", shutdownTimeoutRaw, err)
	}

	cacheSizeRaw := envOrDefault("CACHE_SIZE", "100")
	cacheSize, err := strconv.Atoi(cacheSizeRaw)
	if err != nil || cacheSize < 1 {
		return Config{}, fmt.Errorf("invalid CACHE_SIZE %q", cacheSizeRaw)
	}

	return Config{
		Addr:            net.JoinHostPort("", strconv.Itoa(portNumber)),
		CacheSize:       cacheSize,
		ShutdownTimeout: shutdownTimeout,
	}, nil
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}
