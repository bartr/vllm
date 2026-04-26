package main

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Prompt is one row in a YAML prompt file. The DSL field, when set, is
// prepended to the user prompt as ":dsl <DSL>\n<prompt>". A request-level
// --dsl flag, when also set, is appended after the per-prompt DSL.
//
// MaxTokens and Temperature are optional per-prompt overrides; when set
// they take precedence over the corresponding command-line flags.
type Prompt struct {
	Text        string   `yaml:"prompt"`
	DSL         string   `yaml:"dsl,omitempty"`
	MaxTokens   *int     `yaml:"max_tokens,omitempty"`
	Temperature *float64 `yaml:"temperature,omitempty"`
}

// applyOverrides returns a copy of opts with any per-prompt overrides
// applied. When a Prompt field is nil, the corresponding opts value is
// preserved.
func (p Prompt) applyOverrides(opts options) options {
	if p.MaxTokens != nil {
		opts.maxTokens = *p.MaxTokens
	}
	if p.Temperature != nil {
		opts.temperature = *p.Temperature
	}
	return opts
}

// promptSource is the iterator the bench loop pulls prompts from. Next
// returns ok=false when the source is exhausted; sources may be infinite
// (e.g. --random or --duration with --loop=0) in which case ok is always
// true and the bench loop relies on its own stop conditions.
type promptSource interface {
	Next() (Prompt, bool)
}

// canonicalSource is the single-prompt fallback used by bench mode when
// --file is not supplied. It is unbounded; the bench loop relies on
// duration/count/Ctrl-C to stop.
type canonicalSource struct {
	p Prompt
}

func (c *canonicalSource) Next() (Prompt, bool) { return c.p, true }

// orderedSource walks a prompt list in order, optionally repeating it
// `loop` times. loop=0 means "exactly once".
type orderedSource struct {
	mu      sync.Mutex
	prompts []Prompt
	loop    int // total full passes; 0 means once
	cursor  int // global index, 0 .. len*loop
	limit   int
}

func newOrderedSource(prompts []Prompt, loop int) *orderedSource {
	if loop < 1 {
		loop = 1
	}
	return &orderedSource{
		prompts: prompts,
		loop:    loop,
		limit:   len(prompts) * loop,
	}
}

func (o *orderedSource) Next() (Prompt, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cursor >= o.limit {
		return Prompt{}, false
	}
	p := o.prompts[o.cursor%len(o.prompts)]
	o.cursor++
	return p, true
}

// randomSource returns a random prompt each call. It is unbounded; the
// bench loop's stop conditions terminate it. With loop>0 it caps the
// total number of pulls at len*loop.
type randomSource struct {
	mu      sync.Mutex
	prompts []Prompt
	rng     *rand.Rand
	limit   int // 0 = unlimited
	served  int
}

func newRandomSource(prompts []Prompt, loop int) *randomSource {
	limit := 0
	if loop > 0 {
		limit = len(prompts) * loop
	}
	return &randomSource{
		prompts: prompts,
		rng:     rand.New(rand.NewSource(rand.Int63())),
		limit:   limit,
	}
}

func (r *randomSource) Next() (Prompt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limit > 0 && r.served >= r.limit {
		return Prompt{}, false
	}
	p := r.prompts[r.rng.Intn(len(r.prompts))]
	r.served++
	return p, true
}

// loadPromptFiles reads one or more YAML files and returns the
// concatenated prompt list. Each file's top level must be a YAML
// sequence of {prompt, dsl?} mappings.
func loadPromptFiles(paths []string) ([]Prompt, error) {
	var all []Prompt
	for _, path := range paths {
		buf, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var entries []Prompt
		if err := yaml.Unmarshal(buf, &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for i, e := range entries {
			if e.Text == "" {
				return nil, fmt.Errorf("%s: entry %d has empty `prompt`", path, i)
			}
		}
		all = append(all, entries...)
	}
	if len(all) == 0 {
		return nil, errors.New("no prompts found in --file inputs")
	}
	return all, nil
}

// buildPromptSource picks the right promptSource implementation for the
// current options. canonical is the single-prompt fallback used when
// --file is not supplied.
func buildPromptSource(opts options, canonical Prompt) (promptSource, int, error) {
	if len(opts.files) == 0 {
		return &canonicalSource{p: canonical}, 0, nil
	}
	prompts, err := loadPromptFiles(opts.files)
	if err != nil {
		return nil, 0, err
	}
	if opts.random {
		return newRandomSource(prompts, opts.loop), len(prompts), nil
	}
	loop := opts.loop
	if loop < 1 {
		loop = 1 // default: process each prompt once, then exit
	}
	return newOrderedSource(prompts, loop), len(prompts), nil
}
