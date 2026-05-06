package main

import (
	"sync"
	"time"
)

// runtimeConfig is the live, mutable subset of options that the askd
// HTTP API can read, override per-job, or reset. It is initialized
// from the askd process flags / env (the "defaults") and reset returns
// to those defaults.
type runtimeConfig struct {
	mu       sync.RWMutex
	defaults options
	current  options
}

func newRuntimeConfig(defaults options) *runtimeConfig {
	return &runtimeConfig{defaults: defaults, current: defaults}
}

func (r *runtimeConfig) snapshot() options {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

func (r *runtimeConfig) defaultsSnapshot() options {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaults
}

// reset reverts current to the askd-startup defaults (e.g. ConfigMap +
// flag + env values).
func (r *runtimeConfig) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = r.defaults
}

// update merges the provided spec into current. Zero-valued spec
// fields are treated as "unset" (current is preserved). Returns the
// updated snapshot.
func (r *runtimeConfig) update(spec jobSpec) options {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.current
	if spec.Bench > 0 {
		o.bench = spec.Bench
	}
	if spec.Count > 0 {
		o.count = spec.Count
	}
	if spec.DurationMs > 0 {
		o.duration = time.Duration(spec.DurationMs) * time.Millisecond
	}
	if spec.RampStart > 0 {
		o.rampStart = spec.RampStart
	}
	if spec.RampEnd > 0 {
		o.rampEnd = spec.RampEnd
	}
	if spec.RampDurMs > 0 {
		o.rampDuration = time.Duration(spec.RampDurMs) * time.Millisecond
	}
	if spec.Loop {
		o.loop = true
	}
	if spec.Random {
		o.random = true
	}
	if spec.Warmup {
		o.warmup = true
	}
	if len(spec.Files) > 0 {
		o.files = spec.Files
	}
	if spec.Prompt != "" {
		o.promptText = spec.Prompt
	}
	if spec.URL != "" {
		o.url = spec.URL
	}
	if spec.Token != "" {
		o.token = spec.Token
	}
	if spec.Model != "" {
		o.model = spec.Model
	}
	if spec.SystemPrompt != "" {
		o.systemPrompt = spec.SystemPrompt
	}
	if spec.MaxTokens > 0 {
		o.maxTokens = spec.MaxTokens
	}
	if spec.Temperature != nil {
		o.temperature = *spec.Temperature
	}
	if spec.Stream != nil {
		o.stream = *spec.Stream
	}
	if spec.DSL != "" {
		o.dsl = spec.DSL
	}
	if spec.Quiet {
		o.quiet = true
	}
	if spec.JSON {
		o.json = true
	}
	if spec.Report != nil {
		o.report = *spec.Report
	}
	r.current = o
	return o
}

// optionsToJobSpec returns the current options as a jobSpec for JSON
// serialization on GET /config.
func optionsToJobSpec(o options) jobSpec {
	stream := o.stream
	report := o.report
	temp := o.temperature
	return jobSpec{
		Bench:        o.bench,
		Count:        o.count,
		DurationMs:   int(o.duration / time.Millisecond),
		RampStart:    o.rampStart,
		RampEnd:      o.rampEnd,
		RampDurMs:    int(o.rampDuration / time.Millisecond),
		Loop:         o.loop,
		Random:       o.random,
		Warmup:       o.warmup,
		Files:        o.files,
		Prompt:       o.promptText,
		URL:          o.url,
		Token:        o.token,
		Model:        o.model,
		SystemPrompt: o.systemPrompt,
		MaxTokens:    o.maxTokens,
		Temperature:  &temp,
		Stream:       &stream,
		DSL:          o.dsl,
		Quiet:        o.quiet,
		JSON:         o.json,
		Report:       &report,
	}
}
