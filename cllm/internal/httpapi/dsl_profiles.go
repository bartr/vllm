package httpapi

import "strings"

// DefaultDSLProfiles is intentionally empty. All profile bundles are
// loaded from configs/profiles.yaml at startup (see config.loadDSLProfiles)
// and installed via Handler.SetDSLProfiles. Keeping the symbol around lets
// callers that want a hard-coded default still pass a non-nil map.
var DefaultDSLProfiles = map[string][]string{}

// cloneDSLProfiles returns a defensive copy so callers can't mutate the
// shared default map.
func cloneDSLProfiles(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, v := range src {
		cp := make([]string, len(v))
		copy(cp, v)
		out[strings.ToLower(k)] = cp
	}
	return out
}
