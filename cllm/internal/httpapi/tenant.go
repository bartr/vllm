package httpapi

import (
	"strings"
	"sync"
	"time"

	"cllm/internal/node"
)

// Tenant resolution is purely advisory: the X-Tenant-Id header is trusted
// the same way other clusters trust X-Forwarded-For. Real auth (JWT/OIDC)
// is out of scope; see docs/spec-cost-admission.md.
const (
	tenantHeader      = "X-Tenant-Id"
	defaultTenantName = "default"
	maxTenantNameLen  = 64
)

// TenantConfig is the per-tenant rate-limit configuration loaded from YAML
// or set programmatically. Fields are exported because they are also used
// by the loader and tests.
type TenantConfig struct {
	// Rate is the long-run sustainable token-cost rate, in tokens per
	// second. Zero or negative disables the rate limit (tenant is gated
	// only by the global budget).
	Rate float64
	// Burst is the maximum token cost the tenant can spend in a single
	// burst before the rate limit kicks in. If zero, defaults to Rate
	// (one second of bursting).
	Burst float64
}

// tenantBucket is a classic token-bucket rate limiter over int64 cost
// units. Unlike node.TokenBudget (which is a semaphore that permits/refunds),
// tenantBucket gates RATE: tokens refill at `rate` per second up to
// `burst`. A request reserves cost atomically; if the bucket lacks
// tokens, tryReserve returns false (no waiting).
//
// All methods are safe for concurrent use.
type tenantBucket struct {
	mu       sync.Mutex
	rate     float64 // tokens/sec; 0 disables the bucket (always admits)
	burst    float64 // capacity
	tokens   float64 // current available tokens
	lastTick time.Time
}

func newTenantBucket(rate, burst float64) *tenantBucket {
	if rate < 0 {
		rate = 0
	}
	if burst < rate {
		burst = rate
	}
	return &tenantBucket{
		rate:     rate,
		burst:    burst,
		tokens:   burst,
		lastTick: time.Now(),
	}
}

// refillLocked advances `tokens` based on wall-clock time since lastTick.
// Caller must hold b.mu.
func (b *tenantBucket) refillLocked(now time.Time) {
	if b.rate <= 0 {
		// Disabled: bucket is irrelevant; keep tokens at burst.
		b.tokens = b.burst
		b.lastTick = now
		return
	}
	elapsed := now.Sub(b.lastTick).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.lastTick = now
}

// tryReserve attempts to debit cost from the bucket without blocking.
// Returns true on success. Returns true unconditionally when rate is 0
// (bucket disabled).
func (b *tenantBucket) tryReserve(cost float64) bool {
	if cost < 0 {
		cost = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return true
	}
	b.refillLocked(time.Now())
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// refund credits cost back to the bucket, capped at burst. Used when a
// downstream stage (the global budget) rejects a request that this
// bucket already admitted, so the tenant doesn't lose rate quota for
// work that never ran.
func (b *tenantBucket) refund(cost float64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return
	}
	b.tokens += cost
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// reconfigure updates rate/burst. The current token level is preserved
// up to the new burst cap; if rate grows, future refills accelerate.
func (b *tenantBucket) reconfigure(rate, burst float64) {
	if rate < 0 {
		rate = 0
	}
	if burst < rate {
		burst = rate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rate = rate
	b.burst = burst
	if b.tokens > burst {
		b.tokens = burst
	}
	b.lastTick = time.Now()
}

// snapshot returns the current bucket state. Primarily for metrics and
// tests.
func (b *tenantBucket) snapshot() (rate, burst, tokens float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	return b.rate, b.burst, b.tokens
}

// tenantState bundles a tenant's rate-limit bucket with its own
// completion-token p95 estimator. The estimator is queried first; if
// cold, callers fall back to the global estimator.
type tenantState struct {
	name      string
	bucket    *tenantBucket
	estimator *node.CompletionEstimator
}

// tenantRegistry maps tenant IDs (lower-case) to their state. The
// "default" tenant is always present and serves any request that omits
// the X-Tenant-Id header or names an unknown tenant.
type tenantRegistry struct {
	mu             sync.RWMutex
	tenants        map[string]*tenantState
	defaultConfig  TenantConfig
	estimatorMax   int // p95 window size for new tenants
	estimatorMin   int // minSamples threshold for new tenants
}

func newTenantRegistry(defaultConfig TenantConfig, estimatorMax, estimatorMin int) *tenantRegistry {
	r := &tenantRegistry{
		tenants:       make(map[string]*tenantState),
		defaultConfig: defaultConfig,
		estimatorMax:  estimatorMax,
		estimatorMin:  estimatorMin,
	}
	r.tenants[defaultTenantName] = r.newTenantState(defaultTenantName, defaultConfig)
	return r
}

func (r *tenantRegistry) newTenantState(name string, cfg TenantConfig) *tenantState {
	return &tenantState{
		name:      name,
		bucket:    newTenantBucket(cfg.Rate, cfg.Burst),
		estimator: node.NewCompletionEstimator(r.estimatorMax, r.estimatorMin),
	}
}

// resolveHeader normalizes a header value into a tenant id. Returns
// defaultTenantName for empty/oversize values. Names are lower-cased and
// trimmed; only [a-z0-9_-] are allowed (any other character routes to
// default to avoid metric label explosion from typos or attacks).
func resolveTenantHeader(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > maxTenantNameLen {
		return defaultTenantName
	}
	v = strings.ToLower(v)
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			continue
		default:
			return defaultTenantName
		}
	}
	return v
}

// resolve returns the tenantState for a header value. Unknown tenants
// fall back to the default tenant (they share its bucket and estimator).
func (r *tenantRegistry) resolve(headerValue string) *tenantState {
	name := resolveTenantHeader(headerValue)
	r.mu.RLock()
	t, ok := r.tenants[name]
	r.mu.RUnlock()
	if ok {
		return t
	}
	r.mu.RLock()
	def := r.tenants[defaultTenantName]
	r.mu.RUnlock()
	return def
}

// configure replaces the registry's tenants with a new set. The default
// tenant is always present after this call (created with defaultConfig
// if not supplied). Existing tenants whose names appear in `tenants`
// have their buckets reconfigured in place so in-flight rate state is
// preserved; tenants no longer present are removed.
func (r *tenantRegistry) configure(tenants map[string]TenantConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenants == nil {
		tenants = map[string]TenantConfig{}
	}
	if _, ok := tenants[defaultTenantName]; !ok {
		tenants[defaultTenantName] = r.defaultConfig
	}

	// Remove tenants no longer in the new set.
	for name := range r.tenants {
		if _, keep := tenants[name]; !keep {
			delete(r.tenants, name)
		}
	}
	// Apply / create.
	for name, cfg := range tenants {
		if existing, ok := r.tenants[name]; ok {
			existing.bucket.reconfigure(cfg.Rate, cfg.Burst)
			continue
		}
		r.tenants[name] = r.newTenantState(name, cfg)
	}
}

// names returns a sorted-ish snapshot of registered tenant names. Used
// for metrics and /config rendering. Order is not guaranteed; callers
// that need deterministic order should sort.
func (r *tenantRegistry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tenants))
	for name := range r.tenants {
		out = append(out, name)
	}
	return out
}

// estimateRequestCostForTenant computes admission cost using the
// tenant's estimator first, falling back to the global estimator, then
// to max_tokens. This is a thin wrapper around estimateRequestCost that
// implements the per-tenant fallback chain.
func estimateRequestCostForTenant(payload chatCompletionRequest, tenant *tenantState, global *node.CompletionEstimator) node.RequestCost {
	if tenant != nil && tenant.estimator != nil {
		if _, ok := tenant.estimator.P95(); ok {
			return estimateRequestCost(payload, tenant.estimator)
		}
	}
	return estimateRequestCost(payload, global)
}
