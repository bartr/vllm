// Package runtimeconfig defines the shared runtime defaults and validation
// bounds used by both startup config parsing and the live HTTP handler.
// Keeping them here prevents the public config surface and in-process behavior
// from drifting apart.
package runtimeconfig

const (
	DefaultCacheSize          = 100
	DefaultCacheFilePath      = "/var/lib/cllm/cache.json"
	DefaultDownstreamURL      = "http://localhost:8000"
	DefaultSystemPrompt       = "You are a helpful assistant."
	DefaultMaxTokens          = 1024
	DefaultTemperature        = 0.2
	DefaultMaxTokensPerSecond = 32

	MinCacheSize          = 0
	MaxCacheSize          = 10000
	MinMaxTokens          = 100
	MaxMaxTokens          = 4000
	MinMaxTokensPerSecond = 0
	MaxMaxTokensPerSecond = 1000
)
