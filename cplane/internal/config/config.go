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

	return Config{
		Addr:            net.JoinHostPort("", strconv.Itoa(portNumber)),
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
