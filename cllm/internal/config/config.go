package config

import (
	"cllm/internal/runtimeconfig"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCacheSize             = runtimeconfig.DefaultCacheSize
	defaultCacheFilePath         = runtimeconfig.DefaultCacheFilePath
	defaultDownstreamURL         = runtimeconfig.DefaultDownstreamURL
	defaultMaxTokens             = runtimeconfig.DefaultMaxTokens
	defaultTemperature           = runtimeconfig.DefaultTemperature
	defaultSystemPrompt          = runtimeconfig.DefaultSystemPrompt
	defaultMaxTokensPerSecond    = runtimeconfig.DefaultMaxTokensPerSecond
	defaultMaxConcurrentRequests = runtimeconfig.DefaultMaxConcurrentRequests
	defaultMaxWaitingRequests    = runtimeconfig.DefaultMaxWaitingRequests
	defaultMaxDegradation        = runtimeconfig.DefaultMaxDegradation
	minMaxTokens                 = runtimeconfig.MinMaxTokens
	maxMaxTokens                 = runtimeconfig.MaxMaxTokens
	minMaxTokensPerSecond        = runtimeconfig.MinMaxTokensPerSecond
	maxMaxTokensPerSecond        = runtimeconfig.MaxMaxTokensPerSecond
	minMaxConcurrentRequests     = runtimeconfig.MinMaxConcurrentRequests
	maxMaxConcurrentRequests     = runtimeconfig.MaxMaxConcurrentRequests
	minMaxWaitingRequests        = runtimeconfig.MinMaxWaitingRequests
	maxMaxWaitingRequests        = runtimeconfig.MaxMaxWaitingRequests
	minMaxDegradation            = runtimeconfig.MinMaxDegradation
	maxMaxDegradation            = runtimeconfig.MaxMaxDegradation
)

type Config struct {
	Addr                  string
	CacheSize             int
	CacheFilePath         string
	DownstreamURL         string
	DownstreamToken       string
	DownstreamModel       string
	SystemPrompt          string
	MaxTokens             int
	Temperature           float64
	MaxTokensPerSecond    int
	MaxConcurrentRequests int
	MaxWaitingRequests    int
	MaxDegradation        int
	ShutdownTimeout       time.Duration
}

func Load() (Config, error) {
	return LoadFromArgs(os.Args[1:])
}

func LoadFromArgs(args []string) (Config, error) {
	runtimeOptions, err := loadRuntimeOptions(args)
	if err != nil {
		return Config{}, err
	}

	port := envOrDefault("CACHE_PORT", "8080")
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return Config{}, fmt.Errorf("invalid CACHE_PORT %q", port)
	}

	shutdownTimeoutRaw := envOrDefault("CACHE_SHUTDOWN_TIMEOUT", "10s")
	shutdownTimeout, err := time.ParseDuration(shutdownTimeoutRaw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid CACHE_SHUTDOWN_TIMEOUT %q: %w", shutdownTimeoutRaw, err)
	}

	return Config{
		Addr:                  net.JoinHostPort("", strconv.Itoa(portNumber)),
		CacheSize:             runtimeOptions.cacheSize,
		CacheFilePath:         runtimeOptions.cacheFilePath,
		DownstreamURL:         runtimeOptions.downstreamURL,
		DownstreamToken:       runtimeOptions.downstreamToken,
		DownstreamModel:       runtimeOptions.downstreamModel,
		SystemPrompt:          runtimeOptions.systemPrompt,
		MaxTokens:             runtimeOptions.maxTokens,
		Temperature:           runtimeOptions.temperature,
		MaxTokensPerSecond:    runtimeOptions.maxTokensPerSecond,
		MaxConcurrentRequests: runtimeOptions.maxConcurrentRequests,
		MaxWaitingRequests:    runtimeOptions.maxWaitingRequests,
		MaxDegradation:        runtimeOptions.maxDegradation,
		ShutdownTimeout:       shutdownTimeout,
	}, nil
}

func Usage() string {
	var builder strings.Builder
	builder.WriteString("Usage: cllm [options]\n\n")
	builder.WriteString("Options:\n")
	builder.WriteString("  -h, --help                  Show this help message and exit\n")
	builder.WriteString("      --version               Show version information and exit\n")
	builder.WriteString("  -c, --cache-size int        Maximum number of cached chat responses\n")
	builder.WriteString("      --cache-file-path str  Cache persistence file path (default /var/lib/cllm/cache.json)\n")
	builder.WriteString("      --downstream-url string Downstream OpenAI-compatible base URL (default http://localhost:8000)\n")
	builder.WriteString("      --downstream-token str  Bearer token for downstream requests\n")
	builder.WriteString("      --downstream-model str  Default downstream model when requests omit model\n")
	builder.WriteString(fmt.Sprintf("      --system-prompt string  Default system prompt for chat completions (default %q)\n", defaultSystemPrompt))
	builder.WriteString(fmt.Sprintf("      --max-tokens int        Default max tokens for chat completions (default %d)\n", defaultMaxTokens))
	builder.WriteString(fmt.Sprintf("      --max-tokens-per-second int   Max cached replay tokens per request per second (default %d)\n", defaultMaxTokensPerSecond))
	builder.WriteString(fmt.Sprintf("      --max-concurrent-requests int Max number of concurrent request slots (default %d)\n", defaultMaxConcurrentRequests))
	builder.WriteString(fmt.Sprintf("      --max-waiting-requests int    Max number of queued waiting requests (default %d)\n", defaultMaxWaitingRequests))
	builder.WriteString(fmt.Sprintf("      --max-degradation int         Percent degradation applied after 10%% concurrency usage (default %d)\n", defaultMaxDegradation))
	builder.WriteString(fmt.Sprintf("      --temperature float     Default temperature for chat completions (default %.1f)\n\n", defaultTemperature))
	builder.WriteString("Environment:\n")
	builder.WriteString("  CACHE_PORT\n")
	builder.WriteString("  CACHE_SHUTDOWN_TIMEOUT\n")
	builder.WriteString("  CACHE_CACHE_SIZE\n")
	builder.WriteString("  CACHE_CACHE_FILE_PATH\n")
	builder.WriteString("  CACHE_DOWNSTREAM_URL\n")
	builder.WriteString("  CACHE_DOWNSTREAM_TOKEN\n")
	builder.WriteString("  CACHE_DOWNSTREAM_MODEL\n")
	builder.WriteString("  CACHE_SYSTEM_PROMPT\n")
	builder.WriteString("  CACHE_MAX_TOKENS\n")
	builder.WriteString("  CACHE_MAX_TOKENS_PER_SECOND\n")
	builder.WriteString("  CACHE_MAX_CONCURRENT_REQUESTS\n")
	builder.WriteString("  CACHE_MAX_WAITING_REQUESTS\n")
	builder.WriteString("  CACHE_MAX_DEGRADATION\n")
	builder.WriteString("  CACHE_TEMPERATURE\n")
	return builder.String()
}

type runtimeOptions struct {
	cacheSize             int
	cacheFilePath         string
	downstreamURL         string
	downstreamToken       string
	downstreamModel       string
	systemPrompt          string
	maxTokens             int
	temperature           float64
	maxTokensPerSecond    int
	maxConcurrentRequests int
	maxWaitingRequests    int
	maxDegradation        int
}

func loadRuntimeOptions(args []string) (runtimeOptions, error) {
	options := runtimeOptions{
		cacheSize:             defaultCacheSize,
		cacheFilePath:         defaultCacheFilePath,
		downstreamURL:         defaultDownstreamURL,
		systemPrompt:          defaultSystemPrompt,
		maxTokens:             defaultMaxTokens,
		temperature:           defaultTemperature,
		maxTokensPerSecond:    defaultMaxTokensPerSecond,
		maxConcurrentRequests: defaultMaxConcurrentRequests,
		maxWaitingRequests:    defaultMaxWaitingRequests,
		maxDegradation:        defaultMaxDegradation,
	}

	if envValue := os.Getenv("CACHE_CACHE_SIZE"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil || parsedValue < 0 {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_CACHE_SIZE %q", envValue)
		}
		options.cacheSize = parsedValue
	}

	if envValue := os.Getenv("CACHE_CACHE_FILE_PATH"); envValue != "" {
		options.cacheFilePath = envValue
	}

	if envValue := os.Getenv("CACHE_DOWNSTREAM_URL"); envValue != "" {
		options.downstreamURL = envValue
	}

	if envValue := os.Getenv("CACHE_DOWNSTREAM_TOKEN"); envValue != "" {
		options.downstreamToken = envValue
	}

	if envValue := os.Getenv("CACHE_DOWNSTREAM_MODEL"); envValue != "" {
		options.downstreamModel = envValue
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

	if envValue := os.Getenv("CACHE_MAX_TOKENS_PER_SECOND"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_TOKENS_PER_SECOND %q", envValue)
		}
		options.maxTokensPerSecond = parsedValue
	}

	if envValue := os.Getenv("CACHE_MAX_CONCURRENT_REQUESTS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_CONCURRENT_REQUESTS %q", envValue)
		}
		options.maxConcurrentRequests = parsedValue
	}

	if envValue := os.Getenv("CACHE_MAX_WAITING_REQUESTS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_WAITING_REQUESTS %q", envValue)
		}
		options.maxWaitingRequests = parsedValue
	}

	if envValue := os.Getenv("CACHE_MAX_DEGRADATION"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_DEGRADATION %q", envValue)
		}
		options.maxDegradation = parsedValue
	}

	if envValue := os.Getenv("CACHE_TEMPERATURE"); envValue != "" {
		parsedValue, err := strconv.ParseFloat(envValue, 64)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_TEMPERATURE %q", envValue)
		}
		options.temperature = parsedValue
	}

	flagSet := flag.NewFlagSet("cllm", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	flagSet.IntVar(&options.cacheSize, "cache-size", options.cacheSize, "maximum number of cached responses")
	flagSet.IntVar(&options.cacheSize, "c", options.cacheSize, "maximum number of cached responses")
	flagSet.StringVar(&options.cacheFilePath, "cache-file-path", options.cacheFilePath, "cache persistence file path")
	flagSet.StringVar(&options.downstreamURL, "downstream-url", options.downstreamURL, "downstream OpenAI-compatible base URL")
	flagSet.StringVar(&options.downstreamToken, "downstream-token", options.downstreamToken, "bearer token for downstream requests")
	flagSet.StringVar(&options.downstreamModel, "downstream-model", options.downstreamModel, "default downstream model when model is omitted")
	flagSet.StringVar(&options.systemPrompt, "system-prompt", options.systemPrompt, "default system prompt for requests")
	flagSet.IntVar(&options.maxTokens, "max-tokens", options.maxTokens, "default max tokens for requests")
	flagSet.IntVar(&options.maxTokensPerSecond, "max-tokens-per-second", options.maxTokensPerSecond, "maximum cached replay tokens per request per second")
	flagSet.IntVar(&options.maxConcurrentRequests, "max-concurrent-requests", options.maxConcurrentRequests, "maximum number of concurrent request slots")
	flagSet.IntVar(&options.maxWaitingRequests, "max-waiting-requests", options.maxWaitingRequests, "maximum number of waiting requests")
	flagSet.IntVar(&options.maxDegradation, "max-degradation", options.maxDegradation, "degradation percent applied after 10 percent concurrency usage")
	flagSet.Float64Var(&options.temperature, "temperature", options.temperature, "default temperature for requests")

	// The standard library flag package accepts both -flag and --flag forms,
	// so no manual argument normalization is required.
	if err := flagSet.Parse(args); err != nil {
		return runtimeOptions{}, fmt.Errorf("invalid runtime flag: %w", err)
	}
	if options.cacheSize < 0 {
		return runtimeOptions{}, fmt.Errorf("invalid cache size %d", options.cacheSize)
	}
	if options.maxTokens < minMaxTokens || options.maxTokens > maxMaxTokens {
		return runtimeOptions{}, fmt.Errorf("max-tokens must be between %d and %d", minMaxTokens, maxMaxTokens)
	}
	if options.maxTokensPerSecond < minMaxTokensPerSecond || options.maxTokensPerSecond > maxMaxTokensPerSecond {
		return runtimeOptions{}, fmt.Errorf("max-tokens-per-second must be between %d and %d", minMaxTokensPerSecond, maxMaxTokensPerSecond)
	}
	if options.maxConcurrentRequests < minMaxConcurrentRequests || options.maxConcurrentRequests > maxMaxConcurrentRequests {
		return runtimeOptions{}, fmt.Errorf("max-concurrent-requests must be between %d and %d", minMaxConcurrentRequests, maxMaxConcurrentRequests)
	}
	if options.maxWaitingRequests < minMaxWaitingRequests || options.maxWaitingRequests > maxMaxWaitingRequests {
		return runtimeOptions{}, fmt.Errorf("max-waiting-requests must be between %d and %d", minMaxWaitingRequests, maxMaxWaitingRequests)
	}
	if options.maxWaitingRequests >= 2*options.maxConcurrentRequests {
		return runtimeOptions{}, fmt.Errorf("max-waiting-requests must be less than %d", 2*options.maxConcurrentRequests)
	}
	if options.maxDegradation < minMaxDegradation || options.maxDegradation > maxMaxDegradation {
		return runtimeOptions{}, fmt.Errorf("max-degradation must be between %d and %d", minMaxDegradation, maxMaxDegradation)
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
