// Package runtimeconfig defines the shared runtime defaults and validation
// bounds used by both startup config parsing and the live HTTP handler.
// Keeping them here prevents the public config surface and in-process behavior
// from drifting apart.
package runtimeconfig

const (
	DefaultCacheSize             = 100
	DefaultCacheFilePath         = "/var/lib/cllm/cache.json"
	DefaultDownstreamURL         = "http://localhost:8000"
	DefaultSystemPrompt          = "You are a helpful assistant."
	DefaultMaxTokens             = 1024
	DefaultTemperature           = 0.2
	DefaultMaxTokensPerSecond    = 32
	DefaultMaxConcurrentRequests = 512
	DefaultMaxWaitingRequests    = 1024
	DefaultMaxDegradation        = 10
	DefaultPrefillRateMultiplier = 0.0
	DefaultPrefillBaseOverheadMs = 0
	DefaultPrefillJitterPercent  = 0
	DefaultPrefillMaxMs          = 3000
	DefaultStreamVariabilityPercent     = 0
	DefaultStreamJitterPercent          = 0
	DefaultStreamStallProbabilityPercent = 0
	DefaultStreamStallMinMs             = 100
	DefaultStreamStallMaxMs             = 800
	MinCacheSize                 = 0
	MaxCacheSize                 = 10000
	MinMaxTokens                 = 100
	MaxMaxTokens                 = 4000
	MinMaxTokensPerSecond        = 0
	MaxMaxTokensPerSecond        = 1000
	MinMaxConcurrentRequests     = 1
	MaxMaxConcurrentRequests     = 512
	MinMaxWaitingRequests        = 0
	MaxMaxWaitingRequests        = 1024
	MinMaxDegradation            = 0
	MaxMaxDegradation            = 95
	MinPrefillRateMultiplier     = 0.0
	MaxPrefillRateMultiplier     = 20.0
	MinPrefillBaseOverheadMs     = 0
	MaxPrefillBaseOverheadMs     = 60000
	MinPrefillJitterPercent      = 0
	MaxPrefillJitterPercent      = 100
	MinStreamVariabilityPercent     = 0
	MaxStreamVariabilityPercent     = 100
	MinStreamJitterPercent          = 0
	MaxStreamJitterPercent          = 100
	MinStreamStallProbabilityPercent = 0
	MaxStreamStallProbabilityPercent = 100
	MinStreamStallMs                = 0
	MaxStreamStallMs                = 60000
)
