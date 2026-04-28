package httpapi

import (
	"strings"
	"sync"
)

// Workload-class resolution mirrors tenant resolution: the
// X-Workload-Class header is trusted advisorily and validated by name
// shape. Class is the third dimension of the (tenant × node-class ×
// workload-class) admission matrix introduced in system-design §14.14.
// Phase 14A surfaces the dimension as a Prometheus label only; later
// phases consume Priority and MaxQueueMs.
const (
	classHeader      = "X-Workload-Class"
	defaultClassName = "default"
	maxClassNameLen  = 32

	// dslWorkloadClassPrefix is the per-prompt class override key.
	// Shape: `:dsl workload-class=NAME`. NAME is validated by the same
	// rules as the header. Shares the directive class "workload-class"
	// for first-wins precedence.
	dslWorkloadClassPrefix = "workload-class="
	dslWorkloadClassKey    = "workload-class"

	// dslPriorityPrefix is the per-request priority override key
	// (Phase 14C). Shape: `:dsl priority=low|medium|high`. Shares the
	// directive class "priority" for first-wins precedence; invalid
	// values are dropped silently.
	dslPriorityPrefix = "priority="
	dslPriorityKey    = "priority"
)

// ClassConfig is the per-class configuration loaded from YAML or set
// programmatically. Fields are exported because they are also used by
// the loader and tests. Phase 14A stores Priority and MaxQueueMs but
// does not act on them.
type ClassConfig struct {
	// Priority is an informational tier ("low" | "medium" | "high"),
	// preserved verbatim. Empty values are normalized to "medium".
	Priority string
	// MaxQueueMs is the intended per-class cap on admission queue wait,
	// in milliseconds. Zero or negative disables the cap.
	MaxQueueMs int
	// InitialTokens is the phase-A boundary for phase-aware token
	// allocation (item 13). 0 disables phase A (single-rate
	// behavior). Phase 13.1 stores the field but does not yet act on
	// it.
	InitialTokens int
	// InitialTPS is the per-class rate during the responsiveness
	// (phase A) portion of the stream. 0 inherits the handler base
	// TPS for that phase.
	InitialTPS int
	// SustainedTPS is the per-class rate during the sustained
	// (phase B) portion of the stream. 0 inherits the handler base
	// TPS for that phase.
	SustainedTPS int
}

// classState bundles a resolved class with its configuration. The name
// field is guaranteed to be a value already validated against
// resolveClassHeader (so it is safe to use as a Prometheus label).
type classState struct {
	name   string
	config ClassConfig
}

// classRegistry maps class IDs (lower-case) to their state. The
// "default" class is always present and serves any request that omits
// the X-Workload-Class header, names an unknown class, or supplies a
// malformed value.
type classRegistry struct {
	mu      sync.RWMutex
	classes map[string]*classState
}

func newClassRegistry() *classRegistry {
	r := &classRegistry{classes: make(map[string]*classState)}
	r.classes[defaultClassName] = newClassState(defaultClassName, ClassConfig{Priority: "medium"})
	return r
}

func newClassState(name string, cfg ClassConfig) *classState {
	if cfg.Priority == "" {
		cfg.Priority = "medium"
	}
	return &classState{name: name, config: cfg}
}

// priorityScore maps the textual class priority tier (or a per-request
// `:dsl priority=NAME` value) onto the small integer space used by
// node.TokenBudget for priority-weighted dequeue (Phase 14C). Unknown
// or empty names fall back to medium=0, which is byte-for-byte FIFO
// equivalent against same-tier traffic.
func priorityScore(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "high":
		return 1
	case "low":
		return -1
	default:
		return 0
	}
}

// validPriorityName reports whether name is one of the three accepted
// priority tier strings. The DSL parser uses this to gate
// `:dsl priority=NAME` (Phase 14C); invalid values are silently
// dropped per the existing DSL parser convention.
func validPriorityName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

// resolveClassHeader normalizes a header value into a class id. Returns
// defaultClassName for empty / oversize / malformed values. Names are
// lower-cased and trimmed; only [a-z0-9_-] are allowed (any other
// character routes to default to avoid metric label explosion from
// typos or attacks).
func resolveClassHeader(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > maxClassNameLen {
		return defaultClassName
	}
	v = strings.ToLower(v)
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			continue
		default:
			return defaultClassName
		}
	}
	return v
}

// resolve returns the classState for a header value. Unknown or invalid
// names fall back to the default class.
func (r *classRegistry) resolve(headerValue string) *classState {
	name := resolveClassHeader(headerValue)
	r.mu.RLock()
	c, ok := r.classes[name]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.RLock()
	def := r.classes[defaultClassName]
	r.mu.RUnlock()
	return def
}

// configure replaces the registry's classes with a new set. The default
// class is always present after this call (created with a medium
// priority and no queue cap if missing from the input). Names are
// validated against the same rules as the header; invalid names are
// silently dropped (loader-level validation already catches most
// problems, but defense-in-depth keeps malformed runtime updates from
// polluting Prometheus labels).
func (r *classRegistry) configure(classes map[string]ClassConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.classes = make(map[string]*classState, len(classes)+1)
	for name, cfg := range classes {
		clean := resolveClassHeader(name)
		if clean == defaultClassName && !strings.EqualFold(strings.TrimSpace(name), defaultClassName) {
			// Header sanitizer turned a non-default name into "default"
			// (i.e. the input contained illegal characters). Drop it
			// rather than silently overwriting the default config.
			continue
		}
		r.classes[clean] = newClassState(clean, cfg)
	}
	if _, ok := r.classes[defaultClassName]; !ok {
		r.classes[defaultClassName] = newClassState(defaultClassName, ClassConfig{Priority: "medium"})
	}
}

// names returns the registered class names in deterministic order, with
// "default" first. Used by the /config surface and by tests.
func (r *classRegistry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.classes))
	out = append(out, defaultClassName)
	for name := range r.classes {
		if name == defaultClassName {
			continue
		}
		out = append(out, name)
	}
	// Stable order for everything after default.
	if len(out) > 2 {
		rest := out[1:]
		// simple insertion sort to avoid sort import bloat
		for i := 1; i < len(rest); i++ {
			for j := i; j > 0 && rest[j] < rest[j-1]; j-- {
				rest[j], rest[j-1] = rest[j-1], rest[j]
			}
		}
	}
	return out
}
