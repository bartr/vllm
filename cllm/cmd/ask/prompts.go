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

// orderedSource walks a prompt list in order. When forever is true,
// Next never returns ok=false; otherwise it returns the list once and
// then exhausts.
type orderedSource struct {
	mu      sync.Mutex
	prompts []Prompt
	cursor  int
	forever bool
}

func newOrderedSource(prompts []Prompt, forever bool) *orderedSource {
	return &orderedSource{prompts: prompts, forever: forever}
}

func (o *orderedSource) Next() (Prompt, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.forever && o.cursor >= len(o.prompts) {
		return Prompt{}, false
	}
	p := o.prompts[o.cursor%len(o.prompts)]
	o.cursor++
	return p, true
}

// randomSource returns a random prompt each call. When bounded is true
// it serves exactly len(prompts) pulls; otherwise it is unbounded and
// the bench loop's stop conditions terminate it.
type randomSource struct {
	mu      sync.Mutex
	prompts []Prompt
	rng     *rand.Rand
	limit   int // 0 = unlimited
	served  int
}

func newRandomSource(prompts []Prompt, bounded bool) *randomSource {
	limit := 0
	if bounded {
		limit = len(prompts)
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
// --files is not supplied.
//
// File-mode loop semantics:
//   - --loop set, OR --count > 0, OR --duration > 0: cycle forever
//     (the matching cap drives the stop).
//   - none set: single pass through the concatenated list.
func buildPromptSource(opts options, canonical Prompt) (promptSource, int, error) {
	if len(opts.files) == 0 {
		return &canonicalSource{p: canonical}, 0, nil
	}
	prompts, err := loadPromptFiles(opts.files)
	if err != nil {
		return nil, 0, err
	}
	forever := opts.loop || opts.count > 0 || opts.duration > 0
	if opts.random {
		return newRandomSource(prompts, !forever), len(prompts), nil
	}
	return newOrderedSource(prompts, forever), len(prompts), nil
}
