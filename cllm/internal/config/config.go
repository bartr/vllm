package config

import (
	"cllm/internal/runtimeconfig"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultCacheSize             = runtimeconfig.DefaultCacheSize
	defaultCacheFilePath         = runtimeconfig.DefaultCacheFilePath
	defaultDownstreamURL         = runtimeconfig.DefaultDownstreamURL
	defaultMaxTokens             = runtimeconfig.DefaultMaxTokens
	defaultTemperature           = runtimeconfig.DefaultTemperature
	defaultSystemPrompt          = runtimeconfig.DefaultSystemPrompt
	defaultMaxTokensPerSecond    = runtimeconfig.DefaultMaxTokensPerSecond
	defaultMaxTokensInFlight     = runtimeconfig.DefaultMaxTokensInFlight
	defaultMaxWaitingRequests    = runtimeconfig.DefaultMaxWaitingRequests
	defaultMaxDegradation        = runtimeconfig.DefaultMaxDegradation
	defaultPrefillRateMultiplier = runtimeconfig.DefaultPrefillRateMultiplier
	defaultPrefillBaseOverheadMs = runtimeconfig.DefaultPrefillBaseOverheadMs
	defaultPrefillJitterPercent  = runtimeconfig.DefaultPrefillJitterPercent
	defaultPrefillMaxMs          = runtimeconfig.DefaultPrefillMaxMs
	defaultStreamVariabilityPercent      = runtimeconfig.DefaultStreamVariabilityPercent
	defaultStreamJitterPercent           = runtimeconfig.DefaultStreamJitterPercent
	defaultStreamStallProbabilityPercent = runtimeconfig.DefaultStreamStallProbabilityPercent
	defaultStreamStallMinMs              = runtimeconfig.DefaultStreamStallMinMs
	defaultStreamStallMaxMs              = runtimeconfig.DefaultStreamStallMaxMs
	minMaxTokens                 = runtimeconfig.MinMaxTokens
	maxMaxTokens                 = runtimeconfig.MaxMaxTokens
	minMaxTokensPerSecond        = runtimeconfig.MinMaxTokensPerSecond
	maxMaxTokensPerSecond        = runtimeconfig.MaxMaxTokensPerSecond
	minMaxTokensInFlight         = runtimeconfig.MinMaxTokensInFlight
	maxMaxTokensInFlight         = runtimeconfig.MaxMaxTokensInFlight
	minMaxWaitingRequests        = runtimeconfig.MinMaxWaitingRequests
	maxMaxWaitingRequests        = runtimeconfig.MaxMaxWaitingRequests
	minMaxDegradation            = runtimeconfig.MinMaxDegradation
	maxMaxDegradation            = runtimeconfig.MaxMaxDegradation
	minPrefillRateMultiplier     = runtimeconfig.MinPrefillRateMultiplier
	maxPrefillRateMultiplier     = runtimeconfig.MaxPrefillRateMultiplier
	minPrefillBaseOverheadMs     = runtimeconfig.MinPrefillBaseOverheadMs
	maxPrefillBaseOverheadMs     = runtimeconfig.MaxPrefillBaseOverheadMs
	minPrefillJitterPercent      = runtimeconfig.MinPrefillJitterPercent
	maxPrefillJitterPercent      = runtimeconfig.MaxPrefillJitterPercent
	minStreamVariabilityPercent      = runtimeconfig.MinStreamVariabilityPercent
	maxStreamVariabilityPercent      = runtimeconfig.MaxStreamVariabilityPercent
	minStreamJitterPercent           = runtimeconfig.MinStreamJitterPercent
	maxStreamJitterPercent           = runtimeconfig.MaxStreamJitterPercent
	minStreamStallProbabilityPercent = runtimeconfig.MinStreamStallProbabilityPercent
	maxStreamStallProbabilityPercent = runtimeconfig.MaxStreamStallProbabilityPercent
	minStreamStallMs                 = runtimeconfig.MinStreamStallMs
	maxStreamStallMs                 = runtimeconfig.MaxStreamStallMs
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
	MaxTokensInFlight     int
	MaxWaitingRequests    int
	MaxDegradation        int
	PrefillRateMultiplier float64
	PrefillBaseOverheadMs int
	PrefillJitterPercent  int
	PrefillMaxMs          int
	StreamVariabilityPercent      int
	StreamJitterPercent           int
	StreamStallProbabilityPercent int
	StreamStallMinMs              int
	StreamStallMaxMs              int
	DSLProfiles           map[string][]string
	DSLProfile            string
	Tenants               map[string]TenantSpec
	Classes               map[string]ClassSpec
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

	dslProfiles, err := loadDSLProfiles()
	if err != nil {
		return Config{}, err
	}

	tenants, err := loadTenants()
	if err != nil {
		return Config{}, err
	}

	classes, err := loadClasses()
	if err != nil {
		return Config{}, err
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
		MaxTokensInFlight:     runtimeOptions.maxTokensInFlight,
		MaxWaitingRequests:    runtimeOptions.maxWaitingRequests,
		MaxDegradation:        runtimeOptions.maxDegradation,
		PrefillRateMultiplier: runtimeOptions.prefillRateMultiplier,
		PrefillBaseOverheadMs: runtimeOptions.prefillBaseOverheadMs,
		PrefillJitterPercent:  runtimeOptions.prefillJitterPercent,
		PrefillMaxMs:          runtimeOptions.prefillMaxMs,
		StreamVariabilityPercent:      runtimeOptions.streamVariabilityPercent,
		StreamJitterPercent:           runtimeOptions.streamJitterPercent,
		StreamStallProbabilityPercent: runtimeOptions.streamStallProbabilityPercent,
		StreamStallMinMs:              runtimeOptions.streamStallMinMs,
		StreamStallMaxMs:              runtimeOptions.streamStallMaxMs,
		DSLProfiles:           dslProfiles,
		DSLProfile:            runtimeOptions.dslProfile,
		Tenants:               tenants,
		Classes:               classes,
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
	builder.WriteString(fmt.Sprintf("      --max-tokens-in-flight int    Max admitted token cost in flight (default %d)\n", defaultMaxTokensInFlight))
	builder.WriteString(fmt.Sprintf("      --max-waiting-requests int    Max number of queued waiting requests (default %d)\n", defaultMaxWaitingRequests))
	builder.WriteString(fmt.Sprintf("      --max-degradation int         Percent degradation applied after 10%% concurrency usage (default %d)\n", defaultMaxDegradation))
	builder.WriteString(fmt.Sprintf("      --temperature float     Default temperature for chat completions (default %.1f)\n", defaultTemperature))
	builder.WriteString(fmt.Sprintf("      --prefill-rate-multiplier float  Simulated prefill rate as multiple of max-tokens-per-second; 0 disables (default %g)\n", defaultPrefillRateMultiplier))
	builder.WriteString(fmt.Sprintf("      --prefill-base-overhead-ms int   Fixed simulated prefill startup overhead, ms (default %d)\n", defaultPrefillBaseOverheadMs))
	builder.WriteString(fmt.Sprintf("      --prefill-jitter-percent int     +/- jitter applied to simulated prefill latency, percent (default %d)\n", defaultPrefillJitterPercent))
	builder.WriteString(fmt.Sprintf("      --prefill-max-ms int             Safety cap on simulated prefill latency, ms (default %d)\n", defaultPrefillMaxMs))
	builder.WriteString(fmt.Sprintf("      --stream-variability-percent int +/- token-rate oscillation during cached stream replay, percent (default %d)\n", defaultStreamVariabilityPercent))
	builder.WriteString(fmt.Sprintf("      --stream-jitter-percent int      +/- per-segment jitter during cached stream replay, percent (default %d)\n", defaultStreamJitterPercent))
	builder.WriteString(fmt.Sprintf("      --stream-stall-probability-percent int   Per-segment chance of a partial stall, percent (default %d)\n", defaultStreamStallProbabilityPercent))
	builder.WriteString(fmt.Sprintf("      --stream-stall-min-ms int        Minimum partial-stall duration, ms (default %d)\n", defaultStreamStallMinMs))
	builder.WriteString(fmt.Sprintf("      --stream-stall-max-ms int        Maximum partial-stall duration, ms (default %d)\n", defaultStreamStallMaxMs))
	builder.WriteString("      --dsl-profile string             Default DSL profile applied when a request omits :dsl (must exist in profiles.yaml)\n\n")
	builder.WriteString("Environment:\n")
	builder.WriteString("  CACHE_PORT\n")
	builder.WriteString("  CACHE_SHUTDOWN_TIMEOUT\n")
	builder.WriteString("  CACHE_CACHE_SIZE\n")
	builder.WriteString("  CACHE_CACHE_FILE_PATH\n")
	builder.WriteString("  CACHE_DOWNSTREAM_URL\n")
	builder.WriteString("  CACHE_DOWNSTREAM_TOKEN\n")
	builder.WriteString("  CACHE_DOWNSTREAM_MODEL\n")
	builder.WriteString("  CACHE_SYSTEM_PROMPT\n")
	builder.WriteString("  CACHE_DSL_PROFILE\n")
	builder.WriteString("  CACHE_MAX_TOKENS\n")
	builder.WriteString("  CACHE_MAX_TOKENS_PER_SECOND\n")
	builder.WriteString("  CACHE_MAX_TOKENS_IN_FLIGHT\n")
	builder.WriteString("  CACHE_MAX_WAITING_REQUESTS\n")
	builder.WriteString("  CACHE_MAX_DEGRADATION\n")
	builder.WriteString("  CACHE_TEMPERATURE\n")
	builder.WriteString("  CACHE_PREFILL_RATE_MULTIPLIER\n")
	builder.WriteString("  CACHE_PREFILL_BASE_OVERHEAD_MS\n")
	builder.WriteString("  CACHE_PREFILL_JITTER_PERCENT\n")
	builder.WriteString("  CACHE_PREFILL_MAX_MS\n")
	builder.WriteString("  CACHE_STREAM_VARIABILITY_PERCENT\n")
	builder.WriteString("  CACHE_STREAM_JITTER_PERCENT\n")
	builder.WriteString("  CACHE_STREAM_STALL_PROBABILITY_PERCENT\n")
	builder.WriteString("  CACHE_STREAM_STALL_MIN_MS\n")
	builder.WriteString("  CACHE_STREAM_STALL_MAX_MS\n")
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
	maxTokensInFlight     int
	maxWaitingRequests    int
	maxDegradation        int
	prefillRateMultiplier float64
	prefillBaseOverheadMs int
	prefillJitterPercent  int
	prefillMaxMs          int
	streamVariabilityPercent      int
	streamJitterPercent           int
	streamStallProbabilityPercent int
	streamStallMinMs              int
	streamStallMaxMs              int
	dslProfile                    string
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
		maxTokensInFlight:     defaultMaxTokensInFlight,
		maxWaitingRequests:    defaultMaxWaitingRequests,
		maxDegradation:        defaultMaxDegradation,
		prefillRateMultiplier: defaultPrefillRateMultiplier,
		prefillBaseOverheadMs: defaultPrefillBaseOverheadMs,
		prefillJitterPercent:  defaultPrefillJitterPercent,
		prefillMaxMs:          defaultPrefillMaxMs,
		streamVariabilityPercent:      defaultStreamVariabilityPercent,
		streamJitterPercent:           defaultStreamJitterPercent,
		streamStallProbabilityPercent: defaultStreamStallProbabilityPercent,
		streamStallMinMs:              defaultStreamStallMinMs,
		streamStallMaxMs:              defaultStreamStallMaxMs,
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

	if envValue := os.Getenv("CACHE_DSL_PROFILE"); envValue != "" {
		options.dslProfile = envValue
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

	if envValue := os.Getenv("CACHE_MAX_TOKENS_IN_FLIGHT"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_MAX_TOKENS_IN_FLIGHT %q", envValue)
		}
		options.maxTokensInFlight = parsedValue
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

	if envValue := os.Getenv("CACHE_PREFILL_RATE_MULTIPLIER"); envValue != "" {
		parsedValue, err := strconv.ParseFloat(envValue, 64)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_PREFILL_RATE_MULTIPLIER %q", envValue)
		}
		options.prefillRateMultiplier = parsedValue
	}
	if envValue := os.Getenv("CACHE_PREFILL_BASE_OVERHEAD_MS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_PREFILL_BASE_OVERHEAD_MS %q", envValue)
		}
		options.prefillBaseOverheadMs = parsedValue
	}
	if envValue := os.Getenv("CACHE_PREFILL_JITTER_PERCENT"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_PREFILL_JITTER_PERCENT %q", envValue)
		}
		options.prefillJitterPercent = parsedValue
	}
	if envValue := os.Getenv("CACHE_PREFILL_MAX_MS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_PREFILL_MAX_MS %q", envValue)
		}
		options.prefillMaxMs = parsedValue
	}

	if envValue := os.Getenv("CACHE_STREAM_VARIABILITY_PERCENT"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_STREAM_VARIABILITY_PERCENT %q", envValue)
		}
		options.streamVariabilityPercent = parsedValue
	}
	if envValue := os.Getenv("CACHE_STREAM_JITTER_PERCENT"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_STREAM_JITTER_PERCENT %q", envValue)
		}
		options.streamJitterPercent = parsedValue
	}
	if envValue := os.Getenv("CACHE_STREAM_STALL_PROBABILITY_PERCENT"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_STREAM_STALL_PROBABILITY_PERCENT %q", envValue)
		}
		options.streamStallProbabilityPercent = parsedValue
	}
	if envValue := os.Getenv("CACHE_STREAM_STALL_MIN_MS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_STREAM_STALL_MIN_MS %q", envValue)
		}
		options.streamStallMinMs = parsedValue
	}
	if envValue := os.Getenv("CACHE_STREAM_STALL_MAX_MS"); envValue != "" {
		parsedValue, err := strconv.Atoi(envValue)
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("invalid CACHE_STREAM_STALL_MAX_MS %q", envValue)
		}
		options.streamStallMaxMs = parsedValue
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
	flagSet.IntVar(&options.maxTokensInFlight, "max-tokens-in-flight", options.maxTokensInFlight, "maximum admitted token cost in flight")
	flagSet.IntVar(&options.maxWaitingRequests, "max-waiting-requests", options.maxWaitingRequests, "maximum number of waiting requests")
	flagSet.IntVar(&options.maxDegradation, "max-degradation", options.maxDegradation, "degradation percent applied after 10 percent token-budget usage")
	flagSet.Float64Var(&options.temperature, "temperature", options.temperature, "default temperature for requests")
	flagSet.Float64Var(&options.prefillRateMultiplier, "prefill-rate-multiplier", options.prefillRateMultiplier, "simulated prefill rate as multiple of max-tokens-per-second; 0 disables prefill simulation")
	flagSet.IntVar(&options.prefillBaseOverheadMs, "prefill-base-overhead-ms", options.prefillBaseOverheadMs, "fixed simulated prefill startup overhead in ms")
	flagSet.IntVar(&options.prefillJitterPercent, "prefill-jitter-percent", options.prefillJitterPercent, "+/- jitter applied to simulated prefill latency, percent")
	flagSet.IntVar(&options.prefillMaxMs, "prefill-max-ms", options.prefillMaxMs, "safety cap on simulated prefill latency, ms")
	flagSet.IntVar(&options.streamVariabilityPercent, "stream-variability-percent", options.streamVariabilityPercent, "+/- token-rate oscillation during cached stream replay, percent")
	flagSet.IntVar(&options.streamJitterPercent, "stream-jitter-percent", options.streamJitterPercent, "+/- jitter per content segment during cached stream replay, percent")
	flagSet.IntVar(&options.streamStallProbabilityPercent, "stream-stall-probability-percent", options.streamStallProbabilityPercent, "chance per content segment of a partial stall, percent")
	flagSet.IntVar(&options.streamStallMinMs, "stream-stall-min-ms", options.streamStallMinMs, "minimum stall duration in ms")
	flagSet.IntVar(&options.streamStallMaxMs, "stream-stall-max-ms", options.streamStallMaxMs, "maximum stall duration in ms")
	flagSet.StringVar(&options.dslProfile, "dsl-profile", options.dslProfile, "default DSL profile applied when a request omits :dsl")

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
	if options.maxTokensInFlight < minMaxTokensInFlight || options.maxTokensInFlight > maxMaxTokensInFlight {
		return runtimeOptions{}, fmt.Errorf("max-tokens-in-flight must be between %d and %d", minMaxTokensInFlight, maxMaxTokensInFlight)
	}
	if options.maxWaitingRequests < minMaxWaitingRequests || options.maxWaitingRequests > maxMaxWaitingRequests {
		return runtimeOptions{}, fmt.Errorf("max-waiting-requests must be between %d and %d", minMaxWaitingRequests, maxMaxWaitingRequests)
	}
	// max-waiting-requests is independent of token capacity; the only
	// invariant is its own bounds (checked above).

	if options.maxDegradation < minMaxDegradation || options.maxDegradation > maxMaxDegradation {
		return runtimeOptions{}, fmt.Errorf("max-degradation must be between %d and %d", minMaxDegradation, maxMaxDegradation)
	}
	if options.prefillRateMultiplier < minPrefillRateMultiplier || options.prefillRateMultiplier > maxPrefillRateMultiplier {
		return runtimeOptions{}, fmt.Errorf("prefill-rate-multiplier must be between %g and %g", minPrefillRateMultiplier, maxPrefillRateMultiplier)
	}
	if options.prefillBaseOverheadMs < minPrefillBaseOverheadMs || options.prefillBaseOverheadMs > maxPrefillBaseOverheadMs {
		return runtimeOptions{}, fmt.Errorf("prefill-base-overhead-ms must be between %d and %d", minPrefillBaseOverheadMs, maxPrefillBaseOverheadMs)
	}
	if options.prefillJitterPercent < minPrefillJitterPercent || options.prefillJitterPercent > maxPrefillJitterPercent {
		return runtimeOptions{}, fmt.Errorf("prefill-jitter-percent must be between %d and %d", minPrefillJitterPercent, maxPrefillJitterPercent)
	}
	if options.prefillMaxMs < 1 {
		return runtimeOptions{}, fmt.Errorf("prefill-max-ms must be positive")
	}
	if options.streamVariabilityPercent < minStreamVariabilityPercent || options.streamVariabilityPercent > maxStreamVariabilityPercent {
		return runtimeOptions{}, fmt.Errorf("stream-variability-percent must be between %d and %d", minStreamVariabilityPercent, maxStreamVariabilityPercent)
	}
	if options.streamJitterPercent < minStreamJitterPercent || options.streamJitterPercent > maxStreamJitterPercent {
		return runtimeOptions{}, fmt.Errorf("stream-jitter-percent must be between %d and %d", minStreamJitterPercent, maxStreamJitterPercent)
	}
	if options.streamStallProbabilityPercent < minStreamStallProbabilityPercent || options.streamStallProbabilityPercent > maxStreamStallProbabilityPercent {
		return runtimeOptions{}, fmt.Errorf("stream-stall-probability-percent must be between %d and %d", minStreamStallProbabilityPercent, maxStreamStallProbabilityPercent)
	}
	if options.streamStallMinMs < minStreamStallMs || options.streamStallMinMs > maxStreamStallMs {
		return runtimeOptions{}, fmt.Errorf("stream-stall-min-ms must be between %d and %d", minStreamStallMs, maxStreamStallMs)
	}
	if options.streamStallMaxMs < minStreamStallMs || options.streamStallMaxMs > maxStreamStallMs {
		return runtimeOptions{}, fmt.Errorf("stream-stall-max-ms must be between %d and %d", minStreamStallMs, maxStreamStallMs)
	}
	if options.streamStallMaxMs < options.streamStallMinMs {
		return runtimeOptions{}, fmt.Errorf("stream-stall-max-ms must be >= stream-stall-min-ms")
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

// loadDSLProfiles reads DSL profile bundles from a YAML or JSON file.
//
// Resolution order:
//  1. CLLM_DSL_PROFILES_FILE if set (explicit override, error on missing).
//  2. ./configs/profiles.yaml relative to CWD.
//  3. configs/profiles.yaml relative to the running binary's directory.
//
// Each value in the file may be either a space-separated string of
// directive tokens or a list of token strings. YAML is a superset of JSON
// for the shapes we accept, so JSON files are also valid input. Returns
// nil when no file is found, signaling the handler should keep its
// (empty) defaults.
func loadDSLProfiles() (map[string][]string, error) {
	if explicit := strings.TrimSpace(os.Getenv("CLLM_DSL_PROFILES_FILE")); explicit != "" {
		return readDSLProfilesFile(explicit, true)
	}
	for _, candidate := range dslProfilesSearchPaths() {
		profiles, err := readDSLProfilesFile(candidate, false)
		if err != nil {
			return nil, err
		}
		if profiles != nil {
			return profiles, nil
		}
	}
	return nil, nil
}

// dslProfilesSearchPaths returns the auto-discovery candidates in
// preference order. Missing entries are silently skipped by the caller.
func dslProfilesSearchPaths() []string {
	paths := []string{"configs/profiles.yaml"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "configs", "profiles.yaml"))
	}
	return paths
}

// readDSLProfilesFile loads a single profiles file. When required is
// false, a missing file returns (nil, nil). When required is true, a
// missing file returns an error.
func readDSLProfilesFile(path string, required bool) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read DSL profiles %q: %w", path, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse DSL profiles %q: %w", path, err)
	}
	profiles := make(map[string][]string, len(raw))
	for name, value := range raw {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch v := value.(type) {
		case string:
			profiles[name] = strings.Fields(v)
		case []any:
			tokens := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("profile %q in %s: list entries must be strings", name, path)
				}
				tokens = append(tokens, s)
			}
			profiles[name] = tokens
		case nil:
			profiles[name] = nil
		default:
			return nil, fmt.Errorf("profile %q in %s must be a string or list of strings", name, path)
		}
	}
	return profiles, nil
}
