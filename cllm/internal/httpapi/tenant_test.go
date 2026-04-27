package httpapi

import (
	"strings"
	"testing"
	"time"
)

func TestTenantBucketDisabledAlwaysAdmits(t *testing.T) {
	b := newTenantBucket(0, 0)
	for i := 0; i < 100; i++ {
		if !b.tryReserve(1_000_000) {
			t.Fatalf("disabled bucket rejected at iter %d", i)
		}
	}
}

func TestTenantBucketBurstThenRefills(t *testing.T) {
	b := newTenantBucket(10, 100) // 10 tok/s, burst 100
	if !b.tryReserve(100) {
		t.Fatal("initial burst should fit")
	}
	if b.tryReserve(1) {
		t.Fatal("bucket should be empty")
	}
	// Force a refill by faking time advance.
	b.mu.Lock()
	b.lastTick = time.Now().Add(-2 * time.Second) // 20 tokens worth
	b.mu.Unlock()
	if !b.tryReserve(15) {
		t.Fatal("should admit after refill")
	}
	if b.tryReserve(10) {
		t.Fatal("should not exceed refilled amount")
	}
}

func TestTenantBucketRefund(t *testing.T) {
	b := newTenantBucket(10, 100)
	if !b.tryReserve(80) {
		t.Fatal("initial reserve")
	}
	b.refund(50)
	// 100 - 80 + 50 = 70 available (capped at burst).
	if !b.tryReserve(70) {
		t.Fatal("refund should restore tokens")
	}
	if b.tryReserve(1) {
		t.Fatal("bucket should be drained again")
	}
}

func TestTenantBucketRefundCappedAtBurst(t *testing.T) {
	b := newTenantBucket(10, 100)
	b.refund(1_000_000) // should clamp to burst
	_, _, tokens := b.snapshot()
	if tokens != 100 {
		t.Fatalf("tokens after over-refund = %v; want 100", tokens)
	}
}

func TestTenantBucketReconfigureClampsTokens(t *testing.T) {
	b := newTenantBucket(10, 100)
	// Bucket is full at 100. Shrink burst to 30; tokens must clamp.
	b.reconfigure(5, 30)
	_, _, tokens := b.snapshot()
	if tokens != 30 {
		t.Fatalf("tokens after shrink = %v; want 30", tokens)
	}
}

func TestResolveTenantHeader(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "default"},
		{"   ", "default"},
		{"acme", "acme"},
		{"  ACME  ", "acme"},
		{"a-b_c1", "a-b_c1"},
		{"bad space", "default"},
		{"bad/slash", "default"},
		{"bad.dot", "default"},
		{strings.Repeat("a", 65), "default"}, // too long
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		if got := resolveTenantHeader(tc.in); got != tc.want {
			t.Errorf("resolveTenantHeader(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestTenantRegistryDefaultsAndUnknown(t *testing.T) {
	r := newTenantRegistry(TenantConfig{Rate: 100, Burst: 200}, 256, 50)
	def := r.resolve("")
	if def == nil || def.name != "default" {
		t.Fatalf("default resolve = %+v", def)
	}
	// Unknown tenant -> default fallback.
	unknown := r.resolve("acme")
	if unknown != def {
		t.Fatalf("unknown should fall back to default")
	}
}

func TestTenantRegistryConfigure(t *testing.T) {
	r := newTenantRegistry(TenantConfig{Rate: 100, Burst: 200}, 256, 50)
	r.configure(map[string]TenantConfig{
		"acme":    {Rate: 1000, Burst: 5000},
		"contoso": {Rate: 500, Burst: 1000},
	})
	acme := r.resolve("acme")
	if acme.name != "acme" {
		t.Fatalf("acme not registered: %+v", acme)
	}
	if rate, burst, _ := acme.bucket.snapshot(); rate != 1000 || burst != 5000 {
		t.Fatalf("acme bucket = (%v,%v); want (1000,5000)", rate, burst)
	}
	// Default still present (auto-injected).
	def := r.resolve("")
	if def == nil || def.name != "default" {
		t.Fatalf("default missing after configure")
	}
	// Reconfigure: drop contoso, change acme rate.
	r.configure(map[string]TenantConfig{
		"acme": {Rate: 2000, Burst: 4000},
	})
	if c := r.resolve("contoso"); c.name != "default" {
		t.Fatalf("contoso should fall through to default after removal")
	}
	if rate, _, _ := r.resolve("acme").bucket.snapshot(); rate != 2000 {
		t.Fatalf("acme rate after reconfigure = %v; want 2000", rate)
	}
}

func TestEstimateRequestCostForTenantUsesTenantWhenWarm(t *testing.T) {
	tenant := &tenantState{
		name:      "acme",
		estimator: newCompletionEstimator(100, 5),
	}
	global := newCompletionEstimator(100, 5)
	for i := 0; i < 10; i++ {
		tenant.estimator.observe(50)
		global.observe(500)
	}
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 1000,
	}
	cost := estimateRequestCostForTenant(payload, tenant, global)
	if cost.estimatedTokens != 50 {
		t.Fatalf("estimated = %d; want 50 (tenant wins)", cost.estimatedTokens)
	}
}

func TestEstimateRequestCostForTenantFallsBackToGlobal(t *testing.T) {
	tenant := &tenantState{
		name:      "newbie",
		estimator: newCompletionEstimator(100, 50),
	}
	// Tenant cold (no observations); global warm.
	global := newCompletionEstimator(100, 5)
	for i := 0; i < 10; i++ {
		global.observe(80)
	}
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 1000,
	}
	cost := estimateRequestCostForTenant(payload, tenant, global)
	if cost.estimatedTokens != 80 {
		t.Fatalf("estimated = %d; want 80 (global fallback)", cost.estimatedTokens)
	}
}

func TestEstimateRequestCostForTenantFallsBackToMaxTokensWhenAllCold(t *testing.T) {
	tenant := &tenantState{
		name:      "newbie",
		estimator: newCompletionEstimator(100, 50),
	}
	global := newCompletionEstimator(100, 50) // cold
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 200,
	}
	cost := estimateRequestCostForTenant(payload, tenant, global)
	if cost.estimatedTokens != 200 {
		t.Fatalf("estimated = %d; want 200 (max_tokens cold-start)", cost.estimatedTokens)
	}
}
