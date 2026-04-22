package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	defaultCacheSize     = 100
	defaultMaxTokens     = 500
	defaultTemperature   = 0.2
	defaultSystemPrompt  = "You are a concise assistant."
	minMaxTokens         = 100
	maxMaxTokens         = 10000
)

type Config struct {
	Addr            string
	CacheSize       int
	SystemPrompt    string
	MaxTokens       int
	Temperature     float64
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	runtimeOptions, err := loadRuntimeOptions(os.Args[1:])
	if err != nil {
		return Config{}, err
	}

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
		CacheSize:       runtimeOptions.cacheSize,
		SystemPrompt:    runtimeOptions.systemPrompt,
		MaxTokens:       runtimeOptions.maxTokens,
		Temperature:     runtimeOptions.temperature,
		ShutdownTimeout: shutdownTimeout,
	}, nil
}

type runtimeOptions struct {
	cacheSize    int
	systemPrompt string
	maxTokens    int
	temperature  float64
}

func loadRuntimeOptions(args []string) (runtimeOptions, error) {
	options := runtimeOptions{
		cacheSize:    defaultCacheSize,
		systemPrompt: defaultSystemPrompt,
		maxTokens:    defaultMaxTokens,
		temperature:  defaultTemperature,
	}

	if envValue := os.Getenv("CACHE_CACHE_SIZE"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil || parsedValue < 0 {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_CACHE_SIZE %q", envValue)
		}
		options.cacheSize = parsedValue
	}

	if envValue := os.Getenv("CACHE_SYSTEM_PROMPT"); envValue != "" {
		options.systemPrompt = envValue
	}

	if envValue := os.Getenv("CACHE_MAX_TOKENS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_TOKENS %q", envValue)
		}
		if parsedValue < minMaxTokens || parsedValue > maxMaxTokens {
			return runtimeOptions{}, fmt.Errorf("CACHE_MAX_TOKENS must be between %d and %d", minMaxTokens, maxMaxTokens)
		}
		options.maxTokens = parsedValue
	}

	if envValue := os.Getenv("CACHE_TEMPERATURE"); envValue != "" {
		parsedValue, err := strconv.ParseFloat(envValue, 64)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_TEMPERATURE %q", envValue)
		}
		options.temperature = parsedValue
	}

	flagSet := flag.NewFlagSet("cache", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	flagSet.IntVar(&options.cacheSize, "cache-size", options.cacheSize, "maximum number of cached ask responses")
	flagSet.IntVar(&options.cacheSize, "c", options.cacheSize, "maximum number of cached ask responses")
	flagSet.StringVar(&options.systemPrompt, "system-prompt", options.systemPrompt, "default system prompt for ask requests")
	flagSet.IntVar(&options.maxTokens, "max-tokens", options.maxTokens, "default max tokens for ask requests")
	flagSet.Float64Var(&options.temperature, "temperature", options.temperature, "default temperature for ask requests")

	normalizedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--cache-size":
			normalizedArgs = append(normalizedArgs, "-cache-size")
		case len(arg) > len("--cache-size=") && arg[:len("--cache-size=")] == "--cache-size=":
			normalizedArgs = append(normalizedArgs, "-cache-size="+arg[len("--cache-size="):])
		case arg == "--system-prompt":
			normalizedArgs = append(normalizedArgs, "-system-prompt")
		case len(arg) > len("--system-prompt=") && arg[:len("--system-prompt=")] == "--system-prompt=":
			normalizedArgs = append(normalizedArgs, "-system-prompt="+arg[len("--system-prompt="):])
		case arg == "--max-tokens":
			normalizedArgs = append(normalizedArgs, "-max-tokens")
		case len(arg) > len("--max-tokens=") && arg[:len("--max-tokens=")] == "--max-tokens=":
			normalizedArgs = append(normalizedArgs, "-max-tokens="+arg[len("--max-tokens="):])
		case arg == "--temperature":
			normalizedArgs = append(normalizedArgs, "-temperature")
		case len(arg) > len("--temperature=") && arg[:len("--temperature=")] == "--temperature=":
			normalizedArgs = append(normalizedArgs, "-temperature="+arg[len("--temperature="):])
		default:
			normalizedArgs = append(normalizedArgs, arg)
		}
	}

	if err := flagSet.Parse(normalizedArgs); err != nil {
		return runtimeOptions{}, fmt.Errorf("invalid runtime flag: %w", err)
	}
	if options.cacheSize < 0 {
		return runtimeOptions{}, fmt.Errorf("invalid cache size %d", options.cacheSize)
	}
	if options.maxTokens < minMaxTokens || options.maxTokens > maxMaxTokens {
		return runtimeOptions{}, fmt.Errorf("max-tokens must be between %d and %d", minMaxTokens, maxMaxTokens)
	}

	return options, nil
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}
