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
	defaultCacheSize     = runtimeconfig.DefaultCacheSize
	defaultCacheFilePath = runtimeconfig.DefaultCacheFilePath
	defaultDownstreamURL = runtimeconfig.DefaultDownstreamURL
	defaultMaxTokens     = runtimeconfig.DefaultMaxTokens
	defaultTemperature   = runtimeconfig.DefaultTemperature
	defaultSystemPrompt  = runtimeconfig.DefaultSystemPrompt
	minMaxTokens         = runtimeconfig.MinMaxTokens
	maxMaxTokens         = runtimeconfig.MaxMaxTokens
)

type Config struct {
	Addr            string
	CacheSize       int
	CacheFilePath   string
	DownstreamURL   string
	DownstreamToken string
	DownstreamModel string
	SystemPrompt    string
	MaxTokens       int
	Temperature     float64
	DSLProfiles     map[string][]string
	DSLProfile      string
	Tenants         map[string]TenantSpec
	Classes         map[string]ClassSpec
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
		Addr:            net.JoinHostPort("", strconv.Itoa(portNumber)),
		CacheSize:       runtimeOptions.cacheSize,
		CacheFilePath:   runtimeOptions.cacheFilePath,
		DownstreamURL:   runtimeOptions.downstreamURL,
		DownstreamToken: runtimeOptions.downstreamToken,
		DownstreamModel: runtimeOptions.downstreamModel,
		SystemPrompt:    runtimeOptions.systemPrompt,
		MaxTokens:       runtimeOptions.maxTokens,
		Temperature:     runtimeOptions.temperature,
		DSLProfiles:     dslProfiles,
		DSLProfile:      runtimeOptions.dslProfile,
		Tenants:         tenants,
		Classes:         classes,
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
	builder.WriteString("      --cache-file-path str  Cache persistence file path (default /var/lib/cllm/cache.json)\n")
	builder.WriteString("      --downstream-url string Downstream Chat Completions API base URL (default http://localhost:8000)\n")
	builder.WriteString("      --downstream-token str  Bearer token for downstream requests\n")
	builder.WriteString("      --downstream-model str  Default downstream model when requests omit model\n")
	builder.WriteString(fmt.Sprintf("      --system-prompt string  Default system prompt for chat completions (default %q)\n", defaultSystemPrompt))
	builder.WriteString(fmt.Sprintf("      --max-tokens int        Default max tokens for chat completions (default %d)\n", defaultMaxTokens))
	builder.WriteString(fmt.Sprintf("      --temperature float     Default temperature for chat completions (default %.1f)\n", defaultTemperature))
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
	builder.WriteString("  CACHE_TEMPERATURE\n")
	return builder.String()
}

type runtimeOptions struct {
	cacheSize       int
	cacheFilePath   string
	downstreamURL   string
	downstreamToken string
	downstreamModel string
	systemPrompt    string
	maxTokens       int
	temperature     float64
	dslProfile      string
}

func loadRuntimeOptions(args []string) (runtimeOptions, error) {
	options := runtimeOptions{
		cacheSize:     defaultCacheSize,
		cacheFilePath: defaultCacheFilePath,
		downstreamURL: defaultDownstreamURL,
		systemPrompt:  defaultSystemPrompt,
		maxTokens:     defaultMaxTokens,
		temperature:   defaultTemperature,
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
	flagSet.StringVar(&options.downstreamURL, "downstream-url", options.downstreamURL, "downstream Chat Completions API base URL")
	flagSet.StringVar(&options.downstreamToken, "downstream-token", options.downstreamToken, "bearer token for downstream requests")
	flagSet.StringVar(&options.downstreamModel, "downstream-model", options.downstreamModel, "default downstream model when model is omitted")
	flagSet.StringVar(&options.systemPrompt, "system-prompt", options.systemPrompt, "default system prompt for requests")
	flagSet.IntVar(&options.maxTokens, "max-tokens", options.maxTokens, "default max tokens for requests")
	flagSet.Float64Var(&options.temperature, "temperature", options.temperature, "default temperature for requests")
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
