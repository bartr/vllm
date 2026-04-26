package config

import (
	"path/filepath"
	"testing"
)

// TestShippedProfilesYAMLParses ensures the profiles.yaml shipped under
// cllm/configs/ stays valid: every entry parses, every directive is
// non-empty, and the expected named profiles are present.
func TestShippedProfilesYAMLParses(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "profiles.yaml")
	profiles, err := readDSLProfilesFile(path, true)
	if err != nil {
		t.Fatalf("readDSLProfilesFile(%q) error: %v", path, err)
	}
	want := []string{
		"interactive", "batch", "stall-heavy", "prefill-heavy",
		"fast", "faster", "fastest", "slow", "slower", "slowest",
		"tps-16", "tps-32", "tps-64", "tps-128", "tps-256",
		"tps-512", "tps-1024", "tps-1536", "tps-2048",
	}
	for _, name := range want {
		tokens, ok := profiles[name]
		if !ok {
			t.Errorf("missing profile %q in shipped profiles.yaml", name)
			continue
		}
		if len(tokens) == 0 {
			t.Errorf("profile %q has no tokens", name)
		}
		for _, tok := range tokens {
			if tok == "" {
				t.Errorf("profile %q has empty token", name)
			}
		}
	}
}
