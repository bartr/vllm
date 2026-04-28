package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClassSpec mirrors the YAML/JSON shape for a single workload-class
// entry. Phase 14A stores Priority and MaxQueueMs but does not act on
// them; later phases (14B/14C/13) consume the fields. The httpapi
// package imports a converted form to avoid a package cycle.
type ClassSpec struct {
	// Priority is an informational tier. Recognized values today are
	// "low", "medium", and "high"; unknown values are accepted and
	// stored verbatim so future phases can extend the vocabulary
	// without a config-loader change. Empty defaults to "medium".
	Priority string `yaml:"priority" json:"priority"`
	// MaxQueueMs is the per-class cap on admission queue wait, in
	// milliseconds. Zero or negative disables the cap (request waits
	// the full global queue budget). Phase 14A does not enforce.
	MaxQueueMs int `yaml:"max_queue_ms" json:"max_queue_ms"`
	// InitialTokens is the phase-A boundary, in output tokens, for
	// phase-aware token allocation (item 13). 0 disables phase A
	// (request streams entirely at SustainedTPS / handler base TPS).
	InitialTokens int `yaml:"initial_tokens" json:"initial_tokens"`
	// InitialTPS is the per-class rate during the responsiveness
	// (phase A) portion of the stream. 0 inherits the handler base
	// TPS for that phase.
	InitialTPS int `yaml:"initial_tps" json:"initial_tps"`
	// SustainedTPS is the per-class rate during the sustained
	// (phase B) portion of the stream, after InitialTokens have been
	// emitted. 0 inherits the handler base TPS for that phase.
	SustainedTPS int `yaml:"sustained_tps" json:"sustained_tps"`
}

// loadClasses reads workload-class configurations from a YAML or JSON
// file.
//
// Resolution order:
//  1. CLLM_CLASSES_FILE if set (explicit override; missing file is an error).
//  2. ./configs/classes.yaml relative to CWD.
//  3. configs/classes.yaml relative to the running binary's directory.
//
// File shape:
//
//	classes:
//	  default:
//	    priority: medium
//	    max_queue_ms: 0
//	  interactive:
//	    priority: high
//	    max_queue_ms: 500
//
// Returns nil when no file is found. The "default" key is always
// preserved if present in the file; it is auto-injected by the
// httpapi handler if missing.
func loadClasses() (map[string]ClassSpec, error) {
	if explicit := strings.TrimSpace(os.Getenv("CLLM_CLASSES_FILE")); explicit != "" {
		return readClassesFile(explicit, true)
	}
	for _, candidate := range classesSearchPaths() {
		classes, err := readClassesFile(candidate, false)
		if err != nil {
			return nil, err
		}
		if classes != nil {
			return classes, nil
		}
	}
	return nil, nil
}

func classesSearchPaths() []string {
	paths := []string{"configs/classes.yaml"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "configs", "classes.yaml"))
	}
	return paths
}

type classesFile struct {
	Classes map[string]ClassSpec `yaml:"classes" json:"classes"`
}

func readClassesFile(path string, required bool) (map[string]ClassSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read classes %q: %w", path, err)
	}
	var parsed classesFile
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse classes %q: %w", path, err)
	}
	out := make(map[string]ClassSpec, len(parsed.Classes))
	for name, spec := range parsed.Classes {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if spec.MaxQueueMs < 0 {
			return nil, fmt.Errorf("class %q in %s: max_queue_ms must be >= 0", name, path)
		}
		if spec.InitialTokens < 0 {
			return nil, fmt.Errorf("class %q in %s: initial_tokens must be >= 0", name, path)
		}
		if spec.InitialTPS < 0 {
			return nil, fmt.Errorf("class %q in %s: initial_tps must be >= 0", name, path)
		}
		if spec.SustainedTPS < 0 {
			return nil, fmt.Errorf("class %q in %s: sustained_tps must be >= 0", name, path)
		}
		// Reject misconfigured envelopes: a non-zero phase boundary
		// with both rates inheriting the handler base would emit
		// phase-transition metrics with no observable effect on
		// timing — confusing. Force the operator to set at least one
		// rate explicitly when opting into the two-phase shape.
		if spec.InitialTokens > 0 && spec.InitialTPS == 0 && spec.SustainedTPS == 0 {
			return nil, fmt.Errorf("class %q in %s: initial_tokens > 0 requires initial_tps or sustained_tps to be set", name, path)
		}
		spec.Priority = strings.ToLower(strings.TrimSpace(spec.Priority))
		out[strings.ToLower(name)] = spec
	}
	return out, nil
}
