package node

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleNodesYAML = `
nodes:
  rtx-2000-0:
    class: rtx-2000
    upstream: http://vllm:8000/v1
    max_tokens_per_second: 30
    max_tokens_in_flight: 8192
    max_waiting_requests: 100
  h100-0:
    class: H100
    max_tokens_per_second: 96
    max_tokens_in_flight: 65536
    max_waiting_requests: 200
    prefill_rate_multiplier: 14.0

classes:
  rtx-2000:
    f_load_shape: piecewise_linear
    max_degradation: 10
    prefill_rate_multiplier: 4
  H100:
    f_load_shape: piecewise_linear
    max_degradation: 15
    prefill_rate_multiplier: 12

router:
  policy: least-loaded
  fallback: any
`

func writeTempYAML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestLoadFromCLLMNodesFile(t *testing.T) {
	path := writeTempYAML(t, sampleNodesYAML)
	t.Setenv("CLLM_NODES_FILE", path)

	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec == nil {
		t.Fatal("Load returned nil spec")
	}
	if len(spec.Nodes) != 2 {
		t.Fatalf("nodes: got %d, want 2", len(spec.Nodes))
	}
	if len(spec.Classes) != 2 {
		t.Fatalf("classes: got %d, want 2", len(spec.Classes))
	}
	if spec.Router.Policy != "least-loaded" {
		t.Fatalf("router.policy: got %q, want least-loaded", spec.Router.Policy)
	}
}

func TestLoadMissingFileNoOverride(t *testing.T) {
	t.Setenv("CLLM_NODES_FILE", "")
	// Run from a temp dir so configs/nodes.yaml relative-to-CWD isn't found.
	dir := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected nil spec when no file present, got %+v", spec)
	}
}

func TestLoadMissingFileWithOverrideErrors(t *testing.T) {
	t.Setenv("CLLM_NODES_FILE", "/no/such/path/nodes.yaml")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing override file, got nil")
	}
}

func TestValidateRejectsUnknownClass(t *testing.T) {
	bad := `
nodes:
  a:
    class: unknown
    max_tokens_in_flight: 1
`
	path := writeTempYAML(t, bad)
	t.Setenv("CLLM_NODES_FILE", path)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown class, got nil")
	}
}

func TestBuildAppliesClassDefaultsAndOverrides(t *testing.T) {
	path := writeTempYAML(t, sampleNodesYAML)
	t.Setenv("CLLM_NODES_FILE", path)
	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	nodes := spec.Build()
	if len(nodes) != 2 {
		t.Fatalf("Build: got %d nodes, want 2", len(nodes))
	}

	// Stable lexical order: H100 < rtx-2000 by ASCII.
	if nodes[0].ID != "h100-0" || nodes[1].ID != "rtx-2000-0" {
		t.Fatalf("unexpected order: %s, %s", nodes[0].ID, nodes[1].ID)
	}

	h100 := nodes[0]
	if h100.Class != "H100" {
		t.Fatalf("h100 class: %q", h100.Class)
	}
	if h100.Capacity.MaxTokensInFlight != 65536 {
		t.Fatalf("h100 max_tokens_in_flight: %d", h100.Capacity.MaxTokensInFlight)
	}
	// Per-node override wins.
	if h100.Realism.PrefillRateMultiplier != 14.0 {
		t.Fatalf("h100 prefill_rate_multiplier: got %v, want 14.0", h100.Realism.PrefillRateMultiplier)
	}
	if h100.Degradation.MaxDegradation != 15 {
		t.Fatalf("h100 max_degradation (from class): %d", h100.Degradation.MaxDegradation)
	}
	if h100.Upstream != nil {
		t.Fatalf("h100 should have nil upstream (not set in YAML), got %+v", h100.Upstream)
	}
	if h100.Budget == nil || h100.Estimator == nil {
		t.Fatalf("h100 missing budget or estimator")
	}

	rtx := nodes[1]
	// Class default applies (no per-node override).
	if rtx.Realism.PrefillRateMultiplier != 4.0 {
		t.Fatalf("rtx prefill_rate_multiplier (from class): got %v, want 4", rtx.Realism.PrefillRateMultiplier)
	}
	if rtx.Upstream == nil || rtx.Upstream.URL != "http://vllm:8000/v1" {
		t.Fatalf("rtx upstream: %+v", rtx.Upstream)
	}
}

func TestBuildUsesFallbackWhenNodeCapacityZero(t *testing.T) {
	yaml := `
nodes:
  bare:
    class: ""
`
	path := writeTempYAML(t, yaml)
	t.Setenv("CLLM_NODES_FILE", path)
	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nodes := spec.Build()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	// With the fallback parameter retired, a node with no capacity
	// fields gets a zero Capacity. The Node is still constructed
	// (Budget is non-nil) but admission will reject because the
	// budget is zero. Operators must set capacity per node.
	if nodes[0].Capacity.MaxTokensInFlight != 0 {
		t.Fatalf("expected zero capacity, got %+v", nodes[0].Capacity)
	}
}

// TestBuildKVCompletionFactorInheritance confirms that
// kv_completion_factor inherits from class when the node leaves it
// unset, the per-node override wins when both are present, and that
// nodes with KV modeling enabled get a non-nil KVEstimator.
func TestBuildKVCompletionFactorInheritance(t *testing.T) {
	yaml := `
nodes:
  inherits-class:
    class: H100
    max_tokens_in_flight: 1024
  overrides-class:
    class: H100
    max_tokens_in_flight: 1024
    kv_completion_factor: 0.25
  no-kv:
    class: A10
    max_tokens_in_flight: 1024

classes:
  H100:
    max_kv_tokens: 4096
    kv_completion_factor: 0.5
  A10:
    max_kv_tokens: 0
`
	path := writeTempYAML(t, yaml)
	t.Setenv("CLLM_NODES_FILE", path)
	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nodes := spec.Build()
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(nodes))
	}

	byID := map[string]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}

	if got := byID["inherits-class"].Capacity.KVCompletionFactor; got != 0.5 {
		t.Errorf("inherits-class factor = %v, want 0.5 (class default)", got)
	}
	if byID["inherits-class"].KVEstimator == nil {
		t.Errorf("inherits-class missing KVEstimator (KV is enabled)")
	}
	if got := byID["overrides-class"].Capacity.KVCompletionFactor; got != 0.25 {
		t.Errorf("overrides-class factor = %v, want 0.25 (node override)", got)
	}
	if byID["overrides-class"].KVEstimator == nil {
		t.Errorf("overrides-class missing KVEstimator")
	}
	if got := byID["no-kv"].Capacity.KVCompletionFactor; got != 0 {
		t.Errorf("no-kv factor = %v, want 0 (KV disabled, no inheritance)", got)
	}
	if byID["no-kv"].KVEstimator != nil {
		t.Errorf("no-kv has KVEstimator but KV modeling is disabled")
	}
}

// TestBuildBypassCache confirms that `bypass_cache: true` on a node
// surfaces on Capacity.BypassCache and defaults to false elsewhere.
// Per-node only — there is no class fallback.
func TestBuildBypassCache(t *testing.T) {
	yaml := `
nodes:
  bypassed:
    class: passthrough
    max_tokens_in_flight: 1024
    bypass_cache: true
  cached:
    class: passthrough
    max_tokens_in_flight: 1024
classes:
  passthrough: {}
`
	path := writeTempYAML(t, yaml)
	t.Setenv("CLLM_NODES_FILE", path)
	spec, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nodes := spec.Build()
	byID := map[string]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if !byID["bypassed"].Capacity.BypassCache {
		t.Errorf("bypassed.BypassCache = false, want true")
	}
	if byID["cached"].Capacity.BypassCache {
		t.Errorf("cached.BypassCache = true, want false (default)")
	}
}
