package config

import (
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
	defaultCacheSize             = 100
	defaultDownstreamURL         = "http://127.0.0.1:32080"
	defaultMaxTokens             = 4000
	defaultTemperature           = 0.2
	defaultSystemPrompt          = "You are a helpful assistant."
	defaultMaxTokensPerSecond    = 32
	defaultMaxConcurrentRequests = 5
	defaultMaxWaitingRequests    = 5
	defaultMaxDegradation        = 10
	minMaxTokens                 = 100
	maxMaxTokens                 = 4000
	minMaxTokensPerSecond        = 0
	maxMaxTokensPerSecond        = 1000
	minMaxConcurrentRequests     = 1
	maxMaxConcurrentRequests     = 512
	minMaxWaitingRequests        = 0
	maxMaxWaitingRequests        = 1024
	minMaxDegradation            = 0
	maxMaxDegradation            = 95
)

type Config struct {
	Addr            string
	CacheSize       int
	DownstreamURL   string
	DownstreamToken string
	DownstreamModel string
	SystemPrompt    string
	MaxTokens       int
	Temperature     float64
	MaxTokensPerSecond    int
	MaxConcurrentRequests int
	MaxWaitingRequests    int
	MaxDegradation        int
	ShutdownTimeout time.Duration
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
		Addr:            net.JoinHostPort("", strconv.Itoa(portNumber)),
		CacheSize:       runtimeOptions.cacheSize,
		DownstreamURL:   runtimeOptions.downstreamURL,
		DownstreamToken: runtimeOptions.downstreamToken,
		DownstreamModel: runtimeOptions.downstreamModel,
		SystemPrompt:    runtimeOptions.systemPrompt,
		MaxTokens:       runtimeOptions.maxTokens,
		Temperature:     runtimeOptions.temperature,
		MaxTokensPerSecond:    runtimeOptions.maxTokensPerSecond,
		MaxConcurrentRequests: runtimeOptions.maxConcurrentRequests,
		MaxWaitingRequests:    runtimeOptions.maxWaitingRequests,
		MaxDegradation:        runtimeOptions.maxDegradation,
		ShutdownTimeout: shutdownTimeout,
	}, nil
}

func Usage() string {
	var builder strings.Builder
	builder.WriteString("Usage: cllm [options]\n\n")
	builder.WriteString("Options:\n")
	builder.WriteString("  -h, --help                  Show this help message and exit\n")
	builder.WriteString("      --version               Show version information and exit\n")
	builder.WriteString("  -c, --cache-size int        Maximum number of cached chat responses\n")
	builder.WriteString("      --downstream-url string Downstream OpenAI-compatible base URL (default http://127.0.0.1:32080)\n")
	builder.WriteString("      --downstream-token str  Bearer token for downstream requests\n")
	builder.WriteString("      --downstream-model str  Default downstream model when requests omit model\n")
	builder.WriteString(fmt.Sprintf("      --system-prompt string  Default system prompt for /ask (default %q)\n", defaultSystemPrompt))
	builder.WriteString(fmt.Sprintf("      --max-tokens int        Default max tokens for /ask (default %d)\n", defaultMaxTokens))
	builder.WriteString(fmt.Sprintf("      --max-tokens-per-second int   Max cached replay tokens per request per second (default %d)\n", defaultMaxTokensPerSecond))
	builder.WriteString(fmt.Sprintf("      --max-concurrent-requests int Max number of concurrent request slots (default %d)\n", defaultMaxConcurrentRequests))
	builder.WriteString(fmt.Sprintf("      --max-waiting-requests int    Max number of queued waiting requests (default %d)\n", defaultMaxWaitingRequests))
	builder.WriteString(fmt.Sprintf("      --max-degradation int         Percent degradation applied after 10%% concurrency usage (default %d)\n", defaultMaxDegradation))
	builder.WriteString(fmt.Sprintf("      --temperature float     Default temperature for /ask (default %.1f)\n\n", defaultTemperature))
	builder.WriteString("Environment:\n")
	builder.WriteString("  CACHE_PORT\n")
	builder.WriteString("  CACHE_SHUTDOWN_TIMEOUT\n")
	builder.WriteString("  CACHE_CACHE_SIZE\n")
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
	cacheSize      int
	downstreamURL  string
	downstreamToken string
	downstreamModel string
	systemPrompt string
	maxTokens    int
	temperature  float64
	maxTokensPerSecond    int
	maxConcurrentRequests int
	maxWaitingRequests    int
	maxDegradation        int
}

func loadRuntimeOptions(args []string) (runtimeOptions, error) {
	options := runtimeOptions{
		cacheSize:      defaultCacheSize,
		downstreamURL:  defaultDownstreamURL,
		systemPrompt: defaultSystemPrompt,
		maxTokens:    defaultMaxTokens,
		temperature:  defaultTemperature,
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
	flagSet.IntVar(&options.cacheSize, "cache-size", options.cacheSize, "maximum number of cached ask responses")
	flagSet.IntVar(&options.cacheSize, "c", options.cacheSize, "maximum number of cached ask responses")
	flagSet.StringVar(&options.downstreamURL, "downstream-url", options.downstreamURL, "downstream OpenAI-compatible base URL")
	flagSet.StringVar(&options.downstreamToken, "downstream-token", options.downstreamToken, "bearer token for downstream requests")
	flagSet.StringVar(&options.downstreamModel, "downstream-model", options.downstreamModel, "default downstream model when model is omitted")
	flagSet.StringVar(&options.systemPrompt, "system-prompt", options.systemPrompt, "default system prompt for ask requests")
	flagSet.IntVar(&options.maxTokens, "max-tokens", options.maxTokens, "default max tokens for ask requests")
	flagSet.IntVar(&options.maxTokensPerSecond, "max-tokens-per-second", options.maxTokensPerSecond, "maximum cached replay tokens per request per second")
	flagSet.IntVar(&options.maxConcurrentRequests, "max-concurrent-requests", options.maxConcurrentRequests, "maximum number of concurrent request slots")
	flagSet.IntVar(&options.maxWaitingRequests, "max-waiting-requests", options.maxWaitingRequests, "maximum number of waiting requests")
	flagSet.IntVar(&options.maxDegradation, "max-degradation", options.maxDegradation, "degradation percent applied after 10 percent concurrency usage")
	flagSet.Float64Var(&options.temperature, "temperature", options.temperature, "default temperature for ask requests")

	normalizedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--cache-size":
			normalizedArgs = append(normalizedArgs, "-cache-size")
		case len(arg) > len("--cache-size=") && arg[:len("--cache-size=")] == "--cache-size=":
			normalizedArgs = append(normalizedArgs, "-cache-size="+arg[len("--cache-size="):])
		case arg == "--downstream-url":
			normalizedArgs = append(normalizedArgs, "-downstream-url")
		case len(arg) > len("--downstream-url=") && arg[:len("--downstream-url=")] == "--downstream-url=":
			normalizedArgs = append(normalizedArgs, "-downstream-url="+arg[len("--downstream-url="):])
		case arg == "--downstream-token":
			normalizedArgs = append(normalizedArgs, "-downstream-token")
		case len(arg) > len("--downstream-token=") && arg[:len("--downstream-token=")] == "--downstream-token=":
			normalizedArgs = append(normalizedArgs, "-downstream-token="+arg[len("--downstream-token="):])
		case arg == "--downstream-model":
			normalizedArgs = append(normalizedArgs, "-downstream-model")
		case len(arg) > len("--downstream-model=") && arg[:len("--downstream-model=")] == "--downstream-model=":
			normalizedArgs = append(normalizedArgs, "-downstream-model="+arg[len("--downstream-model="):])
		case arg == "--system-prompt":
			normalizedArgs = append(normalizedArgs, "-system-prompt")
		case len(arg) > len("--system-prompt=") && arg[:len("--system-prompt=")] == "--system-prompt=":
			normalizedArgs = append(normalizedArgs, "-system-prompt="+arg[len("--system-prompt="):])
		case arg == "--max-tokens":
			normalizedArgs = append(normalizedArgs, "-max-tokens")
		case len(arg) > len("--max-tokens=") && arg[:len("--max-tokens=")] == "--max-tokens=":
			normalizedArgs = append(normalizedArgs, "-max-tokens="+arg[len("--max-tokens="):])
		case arg == "--max-tokens-per-second":
			normalizedArgs = append(normalizedArgs, "-max-tokens-per-second")
		case len(arg) > len("--max-tokens-per-second=") && arg[:len("--max-tokens-per-second=")] == "--max-tokens-per-second=":
			normalizedArgs = append(normalizedArgs, "-max-tokens-per-second="+arg[len("--max-tokens-per-second="):])
		case arg == "--max-concurrent-requests":
			normalizedArgs = append(normalizedArgs, "-max-concurrent-requests")
		case len(arg) > len("--max-concurrent-requests=") && arg[:len("--max-concurrent-requests=")] == "--max-concurrent-requests=":
			normalizedArgs = append(normalizedArgs, "-max-concurrent-requests="+arg[len("--max-concurrent-requests="):])
		case arg == "--max-waiting-requests":
			normalizedArgs = append(normalizedArgs, "-max-waiting-requests")
		case len(arg) > len("--max-waiting-requests=") && arg[:len("--max-waiting-requests=")] == "--max-waiting-requests=":
			normalizedArgs = append(normalizedArgs, "-max-waiting-requests="+arg[len("--max-waiting-requests="):])
		case arg == "--max-degradation":
			normalizedArgs = append(normalizedArgs, "-max-degradation")
		case len(arg) > len("--max-degradation=") && arg[:len("--max-degradation=")] == "--max-degradation=":
			normalizedArgs = append(normalizedArgs, "-max-degradation="+arg[len("--max-degradation="):])
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
