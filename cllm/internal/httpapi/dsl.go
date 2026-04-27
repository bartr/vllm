package httpapi

import (
	"strconv"
	"strings"
)

// dslMarker is the literal token that, when present in a message's content,
// switches the prompt parser into DSL mode. Matching is case-insensitive,
// so `:dsl`, `:DSL`, and `:Dsl` are all valid. All whitespace-separated
// tokens following the marker are interpreted as replay directives and
// stripped from the message before the request reaches the downstream
// model or the cache key. Multiple marker occurrences across messages are
// allowed; tokens are processed in document order with
// first-occurrence-of-each-class-wins semantics. The directive `no-delay`
// is a macro shorthand for `no-prefill no-jitter no-variability no-stall`;
// it does NOT disable TPS pacing. Use `no-tps` to skip TPS pacing.
const dslMarker = ":dsl"

// replayOverrides is the resolved set of per-request adjustments derived
// from DSL directives. nil/zero fields mean "no override".
//
// Cache semantics are controlled by two booleans that share a single
// directive class ("cache") for first-wins precedence:
//
//	noCache  skip cache lookup AND skip cache write (pure passthrough).
//	reCache  skip cache lookup but DO write the fresh response back to
//	         the cache, replacing any stale entry.
type replayOverrides struct {
	noCache              bool
	reCache              bool
	noTPS                bool
	noPrefill            bool
	tpsOverride          int     // <=0 means use handler value
	maxTokensOverride    int     // <=0 means use request value
	prefillDurationScale float64 // 1.0 means no change

	// delayScaleFn returns a fresh per-segment multiplicative scale applied
	// on top of variability and jitter. nil means no override.
	delayScaleFn func() float64

	// jitterFn / variabilityFn / stallFn replace the corresponding handler
	// percent at apply time. nil means use the handler value unchanged.
	jitterFn      func(handler int) int
	variabilityFn func(handler int) int
	stallFn       func(handler int) int

	directives []string
}

func newReplayOverrides() replayOverrides {
	return replayOverrides{prefillDurationScale: 1}
}

func (o replayOverrides) active() bool { return len(o.directives) > 0 }

func (o replayOverrides) resolveJitterPercent(handler int) int {
	if o.jitterFn == nil {
		return handler
	}
	return o.jitterFn(handler)
}

func (o replayOverrides) resolveVariabilityPercent(handler int) int {
	if o.variabilityFn == nil {
		return handler
	}
	return o.variabilityFn(handler)
}

func (o replayOverrides) resolveStallPercent(handler int) int {
	if o.stallFn == nil {
		return handler
	}
	return o.stallFn(handler)
}

// dslDirectiveKeys maps the keyword form of each numeric directive to its
// internal class. All entries accept either `key=N`, `key=A:B`, `key N`, or
// `key A:B` (the latter two as adjacent whitespace-separated tokens). N, A,
// and B are signed integers; ranges with mixed signs (e.g. `-20:20`) are
// allowed and lo/hi are normalized so lo<=hi.
var dslDirectiveKeys = map[string]string{
	"jitter":      "jitter",
	"variability": "variability",
	"stall":       "stall",
	"prefill":     "prefill",
	"segment":     "delay",
	"tps":         "tps",
	"max-tokens":  "max-tokens",
}

// parseDSL strips :dsl directives from each message's content and returns
// the cleaned messages plus the resolved overrides. The provided draw
// function (typically Handler.jitterSource) supplies values in [-1, 1) for
// any random ranges (e.g. "segment=30:50") used by once-per-request
// directives. Per-segment directives like the bare delay scale capture the
// draw function in a closure and call it fresh for each segment.
func parseDSL(messages []chatCompletionMessage, draw func() float64) ([]chatCompletionMessage, replayOverrides) {
	return parseDSLWithProfiles(messages, draw, DefaultDSLProfiles)
}

// parseDSLWithProfiles is parseDSL with a caller-supplied profile map.
// A nil or empty profiles map disables profile expansion.
func parseDSLWithProfiles(messages []chatCompletionMessage, draw func() float64, profiles map[string][]string) ([]chatCompletionMessage, replayOverrides) {
	return parseDSLWithDefaultProfile(messages, draw, profiles, "")
}

// parseDSLWithDefaultProfile is parseDSLWithProfiles with an additional
// default-profile name. When defaultProfile is non-empty AND no message in
// the request contains the `:dsl` marker, the named profile bundle is
// expanded as if the prompt had said `:dsl profile=defaultProfile`.
// Explicit `:dsl …` tokens (including an explicit `profile=`) suppress the
// default entirely.
func parseDSLWithDefaultProfile(messages []chatCompletionMessage, draw func() float64, profiles map[string][]string, defaultProfile string) ([]chatCompletionMessage, replayOverrides) {
	overrides := newReplayOverrides()
	if draw == nil {
		draw = func() float64 { return 0 }
	}

	seen := map[string]bool{}
	var profileName string

	// Pre-scan: cache directives have the highest precedence. Profile
	// selection is also collected here so the chosen bundle can be
	// expanded after the main loop. The "cache" class is shared by
	// no-cache and re-cache; first-wins.
	for _, m := range messages {
		_, dslPart, hadMarker := splitAtDSLMarker(m.Content)
		if !hadMarker {
			continue
		}
		for _, raw := range strings.Fields(dslPart) {
			tok := strings.ToLower(raw)
			if tok == "no-cache" && !seen["cache"] {
				seen["cache"] = true
				overrides.noCache = true
				overrides.directives = append(overrides.directives, "no-cache")
				continue
			}
			if tok == "re-cache" && !seen["cache"] {
				seen["cache"] = true
				overrides.reCache = true
				overrides.directives = append(overrides.directives, "re-cache")
				continue
			}
			if name, ok := strings.CutPrefix(tok, "profile="); ok && !seen["profile"] {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if _, exists := profiles[name]; !exists {
					continue
				}
				seen["profile"] = true
				profileName = name
				overrides.directives = append(overrides.directives, "profile="+name)
			}
		}
	}

	out := make([]chatCompletionMessage, len(messages))
	anyMarker := false
	for i, m := range messages {
		cleaned, dslPart, hadMarker := splitAtDSLMarker(m.Content)
		out[i] = chatCompletionMessage{Role: m.Role, Content: cleaned}
		if !hadMarker {
			continue
		}
		anyMarker = true
		fields := strings.Fields(dslPart)
		applyDSLTokenList(&overrides, fields, seen, draw, true)
	}

	// If no message carried a `:dsl` marker, fall back to the configured
	// default profile (if any). It is recorded as `profile=NAME` in the
	// directives slice so dashboards and lifecycle events can attribute the
	// behavior. The default is suppressed entirely whenever the prompt
	// included any DSL directives — including its own `profile=`.
	if !anyMarker && defaultProfile != "" && !seen["profile"] {
		name := strings.ToLower(strings.TrimSpace(defaultProfile))
		if _, exists := profiles[name]; exists {
			seen["profile"] = true
			profileName = name
			overrides.directives = append(overrides.directives, "profile="+name)
		}
	}

	// Expand the chosen profile last. The shared `seen` map ensures any
	// directive class already set by an explicit prompt token is preserved
	// (first-wins), so profiles act as a baseline rather than an override.
	if profileName != "" {
		// Bundle entries may themselves contain whitespace-separated tokens
		// (e.g. "stall=50 jitter=20" as a single string), so split each.
		var bundle []string
		for _, raw := range profiles[profileName] {
			bundle = append(bundle, strings.Fields(strings.ToLower(strings.TrimSpace(raw)))...)
		}
		applyDSLTokenList(&overrides, bundle, seen, draw, false)
	}

	return out, overrides
}

// applyDSLTokenList processes a slice of pre-tokenized DSL tokens applying
// first-wins-per-class semantics. The inPrompt flag is reserved for
// future use; today it has no effect on parsing (no-delay is a macro and
// no longer terminal). Profile expansion calls this with inPrompt=false.
func applyDSLTokenList(o *replayOverrides, fields []string, seen map[string]bool, draw func() float64, inPrompt bool) {
	for i := 0; i < len(fields); i++ {
		raw := strings.ToLower(fields[i])
		if raw == "" {
			continue
		}
		if raw == "no-cache" {
			// Already recorded by the pre-scan (or, for profile expansion,
			// handled directly below).
			if !inPrompt && !seen["cache"] {
				seen["cache"] = true
				o.noCache = true
				o.directives = append(o.directives, "no-cache")
			}
			continue
		}
		if raw == "re-cache" {
			if !inPrompt && !seen["cache"] {
				seen["cache"] = true
				o.reCache = true
				o.directives = append(o.directives, "re-cache")
			}
			continue
		}
		if strings.HasPrefix(raw, "profile=") {
			// Profiles are pre-scanned in the prompt, never nested in bundles.
			continue
		}

		// key=value form
		if eq := strings.IndexByte(raw, '='); eq > 0 {
			key, val := raw[:eq], raw[eq+1:]
			if _, isKey := dslDirectiveKeys[key]; isKey {
				if _, ok := applyKeyedDirective(o, key, val, seen, draw); ok {
					o.directives = append(o.directives, key+"="+val)
				}
				continue
			}
		}

		// Bare keyword followed by a value token (no `=`).
		if _, isKey := dslDirectiveKeys[raw]; isKey && i+1 < len(fields) {
			nextRaw := strings.ToLower(fields[i+1])
			if _, _, ok := parseDSLValue(nextRaw); ok {
				if _, ok2 := applyKeyedDirective(o, raw, nextRaw, seen, draw); ok2 {
					o.directives = append(o.directives, raw+"="+nextRaw)
					i++ // consume value token
					continue
				}
				// Recognized key but value out of range / class already
				// taken: still consume the value token to avoid having it
				// re-interpreted as a standalone directive.
				i++
				continue
			}
		}

		// Bare directives that take no argument.
		if applyDSLBareToken(o, raw, seen) {
			o.directives = append(o.directives, raw)
		}
	}
}

// splitAtDSLMarker returns (before, after, hadMarker). before has the
// marker and any trailing whitespace removed. The marker match is
// case-insensitive. The DSL line is bounded at the first newline after
// the marker: anything past that newline is prompt body and is
// concatenated onto `before` so it is not consumed as DSL tokens.
func splitAtDSLMarker(content string) (string, string, bool) {
	idx := strings.Index(strings.ToLower(content), dslMarker)
	if idx < 0 {
		return content, "", false
	}
	before := strings.TrimRight(content[:idx], " \t\r\n")
	rest := content[idx+len(dslMarker):]
	dslPart := rest
	var afterBody string
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		dslPart = rest[:nl]
		afterBody = rest[nl+1:]
	}
	if afterBody != "" {
		if before == "" {
			before = strings.TrimRight(afterBody, " \t\r\n")
		} else {
			before = before + "\n" + strings.TrimRight(afterBody, " \t\r\n")
		}
	}
	return before, dslPart, true
}

// applyDSLBareToken applies a no-argument directive. Returns true when at
// least one underlying class was claimed (so the token is recorded in the
// directives slice). The `no-delay` token is a macro: it applies each of
// no-prefill / no-jitter / no-variability / no-stall, skipping any
// classes already claimed by an earlier directive (first-wins).
func applyDSLBareToken(o *replayOverrides, tok string, seen map[string]bool) bool {
	switch tok {
	case "no-delay":
		applied := false
		if !seen["prefill"] {
			seen["prefill"] = true
			o.noPrefill = true
			applied = true
		}
		if !seen["jitter"] {
			seen["jitter"] = true
			o.jitterFn = func(int) int { return 0 }
			applied = true
		}
		if !seen["variability"] {
			seen["variability"] = true
			o.variabilityFn = func(int) int { return 0 }
			applied = true
		}
		if !seen["stall"] {
			seen["stall"] = true
			o.stallFn = func(int) int { return 0 }
			applied = true
		}
		return applied
	case "no-tps":
		if seen["tps"] {
			return false
		}
		seen["tps"] = true
		o.noTPS = true
		return true
	case "no-prefill":
		if seen["prefill"] {
			return false
		}
		seen["prefill"] = true
		o.noPrefill = true
		return true
	case "no-jitter":
		if seen["jitter"] {
			return false
		}
		seen["jitter"] = true
		o.jitterFn = func(int) int { return 0 }
		return true
	case "no-variability":
		if seen["variability"] {
			return false
		}
		seen["variability"] = true
		o.variabilityFn = func(int) int { return 0 }
		return true
	case "no-stall":
		if seen["stall"] {
			return false
		}
		seen["stall"] = true
		o.stallFn = func(int) int { return 0 }
		return true
	}
	return false
}

// applyKeyedDirective applies a single `key=value` directive (or its
// equivalent two-token form). Returns the resolved internal class on
// success and false if the value is malformed, out of range, or the class
// has already been claimed by an earlier directive.
func applyKeyedDirective(o *replayOverrides, key, val string, seen map[string]bool, draw func() float64) (string, bool) {
	lo, hi, ok := parseDSLValue(val)
	if !ok {
		return "", false
	}
	class, recognized := dslDirectiveKeys[key]
	if !recognized {
		return "", false
	}
	if seen[class] {
		return "", false
	}

	switch key {
	case "tps":
		n := resolveDelta(lo, hi, draw)
		if n < 1 || n > 2048 {
			return "", false
		}
		seen[class] = true
		o.tpsOverride = n
	case "max-tokens":
		n := resolveDelta(lo, hi, draw)
		if n < 1 {
			return "", false
		}
		seen[class] = true
		o.maxTokensOverride = n
	case "segment":
		seen[class] = true
		o.delayScaleFn = func() float64 { return resolveScalar(lo, hi, draw) }
	case "prefill":
		seen[class] = true
		o.prefillDurationScale = resolveScalar(lo, hi, draw)
	case "jitter":
		delta := resolveDelta(lo, hi, draw)
		seen[class] = true
		o.jitterFn = func(handler int) int { return clampPercent(handler + delta) }
	case "variability":
		delta := resolveDelta(lo, hi, draw)
		seen[class] = true
		o.variabilityFn = func(handler int) int { return clampPercent(handler + delta) }
	case "stall":
		delta := resolveDelta(lo, hi, draw)
		seen[class] = true
		o.stallFn = func(handler int) int { return clampPercent(handler + delta) }
	default:
		return "", false
	}
	return class, true
}

// parseDSLValue accepts either a single signed integer ("20", "-20") or a
// signed range ("20:50", "-50:-20", "-20:20"). lo/hi are normalized so
// lo<=hi. The empty string is rejected.
func parseDSLValue(s string) (int, int, bool) {
	if s == "" {
		return 0, 0, false
	}
	if idx := strings.Index(s, ":"); idx > 0 && idx < len(s)-1 {
		a, errA := strconv.Atoi(s[:idx])
		b, errB := strconv.Atoi(s[idx+1:])
		if errA != nil || errB != nil {
			return 0, 0, false
		}
		if a > b {
			a, b = b, a
		}
		return a, b, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	return n, n, true
}

// resolveScalar returns a multiplicative duration factor for a signed
// percent in [lo, hi]. Negative pct shrinks the duration (1 + pct/100 < 1)
// and positive pct grows it. The result is clamped to [0, +Inf) so a pct
// of -100 or below collapses to zero rather than going negative.
func resolveScalar(lo, hi int, draw func() float64) float64 {
	pct := float64(lo)
	if hi > lo {
		r := (draw() + 1) / 2
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		pct = float64(lo) + r*float64(hi-lo)
	}
	s := 1 + pct/100
	if s < 0 {
		s = 0
	}
	return s
}

// resolveDelta returns a signed integer in [lo, hi]. Used once per request
// for directives whose value is added to a percent counter (jitter,
// variability, stall) or used directly as a count (tps, max-tokens).
func resolveDelta(lo, hi int, draw func() float64) int {
	if hi <= lo {
		return lo
	}
	r := (draw() + 1) / 2
	if r < 0 {
		r = 0
	}
	if r > 1 {
		r = 1
	}
	return lo + int(r*float64(hi-lo)+0.5)
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// dslDirectiveFamily returns a low-cardinality label for a parsed DSL
// directive token, suitable for use as a Prometheus label value. Tokens
// with embedded numeric arguments collapse to a stable family name based
// on the keyword left of `=` (e.g. "tps=512" -> "tps", "segment=30:50" ->
// "segment", "jitter=-20:20" -> "jitter"). Profile selections are returned
// verbatim because the set of profile names is bounded by configuration.
func dslDirectiveFamily(tok string) string {
	tok = strings.ToLower(strings.TrimSpace(tok))
	if tok == "" {
		return ""
	}
	if strings.HasPrefix(tok, "profile=") {
		return tok
	}
	switch tok {
	case "no-cache", "re-cache", "no-delay", "no-tps", "no-prefill", "no-jitter", "no-variability", "no-stall":
		return tok
	}
	if eq := strings.IndexByte(tok, '='); eq > 0 {
		key := tok[:eq]
		if _, ok := dslDirectiveKeys[key]; ok {
			return key
		}
	}
	return "other"
}

// dslFamilies returns the unique families for the given directive tokens.
// The order is the first-seen order in directives. When the slice is empty
// it returns []string{"none"} so dashboards always have a baseline series.
func dslFamilies(directives []string) []string {
	if len(directives) == 0 {
		return []string{"none"}
	}
	seen := make(map[string]bool, len(directives))
	out := make([]string, 0, len(directives))
	for _, d := range directives {
		fam := dslDirectiveFamily(d)
		if fam == "" || seen[fam] {
			continue
		}
		seen[fam] = true
		out = append(out, fam)
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}
