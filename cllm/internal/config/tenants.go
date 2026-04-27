package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TenantSpec mirrors the YAML/JSON shape for a single tenant entry. It
// is exported to keep the loader free of package cycles (httpapi
// consumes a converted form).
type TenantSpec struct {
	// Rate is sustainable token-cost rate in tokens per second.
	// Zero or negative disables the per-tenant rate limit (gated only
	// by the global cost budget).
	Rate float64 `yaml:"rate" json:"rate"`
	// Burst is the maximum token cost the tenant can spend in one
	// burst before the rate limit applies. If zero, defaults to Rate.
	Burst float64 `yaml:"burst" json:"burst"`
}

// loadTenants reads tenant configurations from a YAML or JSON file.
//
// Resolution order:
//  1. CLLM_TENANTS_FILE if set (explicit override; missing file is an error).
//  2. ./configs/tenants.yaml relative to CWD.
//  3. configs/tenants.yaml relative to the running binary's directory.
//
// File shape:
//
//	tenants:
//	  default:
//	    rate: 5000
//	    burst: 50000
//	  acme:
//	    rate: 50000
//	    burst: 200000
//
// Returns nil when no file is found. The "default" key is always
// preserved if present in the file; it is auto-injected by the
// httpapi handler if missing.
func loadTenants() (map[string]TenantSpec, error) {
	if explicit := strings.TrimSpace(os.Getenv("CLLM_TENANTS_FILE")); explicit != "" {
		return readTenantsFile(explicit, true)
	}
	for _, candidate := range tenantsSearchPaths() {
		tenants, err := readTenantsFile(candidate, false)
		if err != nil {
			return nil, err
		}
		if tenants != nil {
			return tenants, nil
		}
	}
	return nil, nil
}

func tenantsSearchPaths() []string {
	paths := []string{"configs/tenants.yaml"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "configs", "tenants.yaml"))
	}
	return paths
}

type tenantsFile struct {
	Tenants map[string]TenantSpec `yaml:"tenants" json:"tenants"`
}

func readTenantsFile(path string, required bool) (map[string]TenantSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tenants %q: %w", path, err)
	}
	var parsed tenantsFile
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse tenants %q: %w", path, err)
	}
	out := make(map[string]TenantSpec, len(parsed.Tenants))
	for name, spec := range parsed.Tenants {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if spec.Rate < 0 {
			return nil, fmt.Errorf("tenant %q in %s: rate must be >= 0", name, path)
		}
		if spec.Burst < 0 {
			return nil, fmt.Errorf("tenant %q in %s: burst must be >= 0", name, path)
		}
		out[strings.ToLower(name)] = spec
	}
	return out, nil
}
