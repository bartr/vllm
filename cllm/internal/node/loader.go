package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileSpec is the on-disk shape of configs/nodes.yaml.
//
//	nodes:
//	  rtx-2000-0:
//	    class: rtx-2000
//	    upstream: http://vllm:8000/v1
//	    max_tokens_per_second: 30
//	    max_tokens_in_flight: 8192
//	    max_waiting_requests: 100
//	classes:
//	  rtx-2000:
//	    f_load_shape: piecewise_linear
//	    max_degradation: 10
//	    prefill_rate_multiplier: 4
//	router:
//	  policy: least-loaded
//	  fallback: any
//
// Class entries are templates: a node inherits its class's defaults for
// f_load and the prefill_*/stream_* realism knobs and may override per
// node. Capacity (max_tokens_*) is per-node, not per-class.
type FileSpec struct {
	Nodes   map[string]NodeSpec  `yaml:"nodes"`
	Classes map[string]ClassSpec `yaml:"classes"`
	Router  RouterSpec           `yaml:"router"`
}

// NodeSpec is the per-node section of nodes.yaml. Capacity fields are
// required (zero means "use default"); realism fields override the class.
type NodeSpec struct {
	Class    string `yaml:"class"`
	Upstream string `yaml:"upstream"`
	Token    string `yaml:"token"`
	Model    string `yaml:"model"`

	MaxTokensInFlight  int64 `yaml:"max_tokens_in_flight"`
	MaxTokensPerSecond int   `yaml:"max_tokens_per_second"`
	MaxWaitingRequests int   `yaml:"max_waiting_requests"`

	// MaxKVTokens / KVWeight are the per-node KV-cache axis knobs. 0
	// for MaxKVTokens disables the entire axis on this node and
	// inherits from the class default; KVWeight uses class default
	// when nil. (§4.4 of docs/design-memory-pressure.md.)
	MaxKVTokens int64    `yaml:"max_kv_tokens,omitempty"`
	KVWeight    *float64 `yaml:"kv_weight,omitempty"`

	// Per-node overrides for class realism knobs. A zero value means
	// "inherit from class".
	PrefillRateMultiplier *float64 `yaml:"prefill_rate_multiplier,omitempty"`
	PrefillBaseOverheadMs *int     `yaml:"prefill_base_overhead_ms,omitempty"`
	PrefillJitterPercent  *int     `yaml:"prefill_jitter_percent,omitempty"`
	PrefillMaxMs          *int     `yaml:"prefill_max_ms,omitempty"`
	StreamVariabilityPct  *int     `yaml:"stream_variability_percent,omitempty"`
	StreamJitterPct       *int     `yaml:"stream_jitter_percent,omitempty"`
	StreamStallProbPct    *int     `yaml:"stream_stall_probability_percent,omitempty"`
	StreamStallMinMs      *int     `yaml:"stream_stall_min_ms,omitempty"`
	StreamStallMaxMs      *int     `yaml:"stream_stall_max_ms,omitempty"`
	MaxDegradation        *int     `yaml:"max_degradation,omitempty"`
}

// ClassSpec is the per-class template section of nodes.yaml.
type ClassSpec struct {
	FLoadShape            string  `yaml:"f_load_shape"`
	MaxDegradation        int     `yaml:"max_degradation"`
	PrefillRateMultiplier float64 `yaml:"prefill_rate_multiplier"`
	PrefillBaseOverheadMs int     `yaml:"prefill_base_overhead_ms"`
	PrefillJitterPercent  int     `yaml:"prefill_jitter_percent"`
	PrefillMaxMs          int     `yaml:"prefill_max_ms"`
	StreamVariabilityPct  int     `yaml:"stream_variability_percent"`
	StreamJitterPct       int     `yaml:"stream_jitter_percent"`
	StreamStallProbPct    int     `yaml:"stream_stall_probability_percent"`
	StreamStallMinMs      int     `yaml:"stream_stall_min_ms"`
	StreamStallMaxMs      int     `yaml:"stream_stall_max_ms"`

	// MaxKVTokens / KVWeight are the class-level defaults for the KV
	// admission axis. A node with its own non-zero MaxKVTokens
	// overrides; KVWeight defaults to 1.0 when class and node both
	// leave it unset. (\u00a74.4 of docs/design-memory-pressure.md.)
	MaxKVTokens int64   `yaml:"max_kv_tokens"`
	KVWeight    float64 `yaml:"kv_weight"`
}

// RouterSpec is the routing policy section of nodes.yaml. Phase 2.2 will
// consume this; Phase 2.1 only persists it.
type RouterSpec struct {
	Policy   string `yaml:"policy"`   // class-pinned | least-loaded | chained
	Fallback string `yaml:"fallback"` // any | none
}

// Load reads configs/nodes.yaml using the same resolution rules as
// loadTenants:
//
//  1. CLLM_NODES_FILE if set (explicit override; missing file is an error).
//  2. ./configs/nodes.yaml relative to CWD.
//  3. configs/nodes.yaml relative to the running binary's directory.
//
// Returns (nil, nil) when no file is found, so the caller can fall back to
// a single synthesized node from the existing flat config — this keeps the
// behavior change zero for deployments that have not yet adopted the file.
func Load() (*FileSpec, error) {
	if explicit := strings.TrimSpace(os.Getenv("CLLM_NODES_FILE")); explicit != "" {
		return readFile(explicit, true)
	}
	for _, candidate := range searchPaths() {
		spec, err := readFile(candidate, false)
		if err != nil {
			return nil, err
		}
		if spec != nil {
			return spec, nil
		}
	}
	return nil, nil
}

func searchPaths() []string {
	paths := []string{filepath.Join("configs", "nodes.yaml")}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "configs", "nodes.yaml"))
	}
	return paths
}

func readFile(path string, required bool) (*FileSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil, nil
		}
		return nil, fmt.Errorf("read nodes file %q: %w", path, err)
	}
	var spec FileSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse nodes file %q: %w", path, err)
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("validate nodes file %q: %w", path, err)
	}
	return &spec, nil
}

// Validate checks that every node references a known class (or leaves
// class blank, which uses an implicit "default") and that capacity values
// are non-negative.
func (s *FileSpec) Validate() error {
	if s == nil {
		return nil
	}
	for id, n := range s.Nodes {
		if id == "" {
			return fmt.Errorf("node id must be non-empty")
		}
		if n.Class != "" {
			if _, ok := s.Classes[n.Class]; !ok {
				return fmt.Errorf("node %q references unknown class %q", id, n.Class)
			}
		}
		if n.MaxTokensInFlight < 0 {
			return fmt.Errorf("node %q: max_tokens_in_flight must be >= 0", id)
		}
		if n.MaxTokensPerSecond < 0 {
			return fmt.Errorf("node %q: max_tokens_per_second must be >= 0", id)
		}
		if n.MaxWaitingRequests < 0 {
			return fmt.Errorf("node %q: max_waiting_requests must be >= 0", id)
		}
	}
	return nil
}

// Build materializes the file spec into concrete *Node values, applying
// class defaults and per-node overrides. The returned slice is sorted by
// node ID for stable iteration.
//
// Capacity must be > 0 for the resulting Node to be usable; a node whose
// capacity values are zero in the file inherits the supplied fallback
// values (typically the global flat-config defaults).
func (s *FileSpec) Build(fallback Capacity) []*Node {
	if s == nil || len(s.Nodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.Nodes))
	for id := range s.Nodes {
		ids = append(ids, id)
	}
	// Stable order: lexical by ID.
	sortStrings(ids)

	out := make([]*Node, 0, len(ids))
	for _, id := range ids {
		spec := s.Nodes[id]
		class, _ := s.Classes[spec.Class]

		cap := Capacity{
			MaxTokensInFlight:  pickInt64(spec.MaxTokensInFlight, fallback.MaxTokensInFlight),
			MaxTokensPerSecond: pickInt(spec.MaxTokensPerSecond, fallback.MaxTokensPerSecond),
			MaxWaitingRequests: pickInt(spec.MaxWaitingRequests, fallback.MaxWaitingRequests),
			MaxKVTokens:        pickInt64(spec.MaxKVTokens, class.MaxKVTokens),
			KVWeight:           derefFloat64(spec.KVWeight, class.KVWeight),
		}
		// Only normalize KVWeight when KV modeling is enabled; leave
		// it zero otherwise so a node with KV disabled has a Capacity
		// value identical to its operator-supplied fallback.
		if cap.MaxKVTokens > 0 && cap.KVWeight <= 0 {
			cap.KVWeight = 1.0
		}

		realism := Realism{
			PrefillRateMultiplier: derefFloat64(spec.PrefillRateMultiplier, class.PrefillRateMultiplier),
			PrefillBaseOverheadMs: derefInt(spec.PrefillBaseOverheadMs, class.PrefillBaseOverheadMs),
			PrefillJitterPercent:  derefInt(spec.PrefillJitterPercent, class.PrefillJitterPercent),
			PrefillMaxMs:          derefInt(spec.PrefillMaxMs, class.PrefillMaxMs),
			StreamVariabilityPct:  derefInt(spec.StreamVariabilityPct, class.StreamVariabilityPct),
			StreamJitterPct:       derefInt(spec.StreamJitterPct, class.StreamJitterPct),
			StreamStallProbPct:    derefInt(spec.StreamStallProbPct, class.StreamStallProbPct),
			StreamStallMinMs:      derefInt(spec.StreamStallMinMs, class.StreamStallMinMs),
			StreamStallMaxMs:      derefInt(spec.StreamStallMaxMs, class.StreamStallMaxMs),
		}

		degradation := Degradation{
			Shape:          firstNonEmpty(class.FLoadShape, "piecewise_linear"),
			MaxDegradation: derefInt(spec.MaxDegradation, class.MaxDegradation),
		}

		var upstream *Upstream
		if spec.Upstream != "" {
			upstream = &Upstream{URL: spec.Upstream, Token: spec.Token, Model: spec.Model}
		}

		n := &Node{
			ID:          id,
			Class:       spec.Class,
			Budget:      NewTokenBudget(cap.MaxTokensInFlight, cap.MaxWaitingRequests),
			Estimator:   NewCompletionEstimator(256, 50),
			Capacity:    cap,
			Degradation: degradation,
			Realism:     realism,
			Upstream:    upstream,
		}
		if cap.MaxKVTokens > 0 {
			n.KV = NewKVBudget(cap.MaxKVTokens)
		}
		out = append(out, n)
	}
	return out
}

func pickInt64(v, fallback int64) int64 {
	if v > 0 {
		return v
	}
	return fallback
}

func pickInt(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

func derefInt(p *int, fallback int) int {
	if p != nil {
		return *p
	}
	return fallback
}

func derefFloat64(p *float64, fallback float64) float64 {
	if p != nil {
		return *p
	}
	return fallback
}

func firstNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// sortStrings is a tiny inlined sort to avoid a wider import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
