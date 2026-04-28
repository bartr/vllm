package node

import "sync"

// KVBudget is a non-blocking checked counter representing per-node
// KV-cache occupancy in KV tokens. Unlike TokenBudget, KVBudget has no
// FIFO: ordering is delegated to the sibling TokenBudget so admission
// only consults KVBudget once a compute slot is held.
//
// The "no queue" choice is deliberate. A second blocking semaphore would
// open a two-mutex deadlock window: compute frees, KV is still tight, the
// next FIFO waiter blocks on KV with no one to wake it. KVBudget instead
// rejects fast (kv_pressure) and lets the caller refund the compute slot.
//
// A KVBudget with capacity == 0 represents "KV modeling disabled" and is
// never constructed on a Node; admission paths check Node.KV == nil.
type KVBudget struct {
	mu       sync.Mutex
	capacity int64
	inFlight int64
}

// NewKVBudget returns a KVBudget with the given capacity in KV tokens.
// Capacity is clamped to a minimum of 1.
func NewKVBudget(capacity int64) *KVBudget {
	if capacity < 1 {
		capacity = 1
	}
	return &KVBudget{capacity: capacity}
}

// TryCharge attempts to charge cost against the budget. The returned
// reason is one of:
//
//	""             ok == true; the cost was charged.
//	"kv_oversize"  cost alone exceeds total capacity; never admittable.
//	"kv_pressure"  cost would fit eventually but the budget is currently
//	               full; caller should release compute and 429.
func (b *KVBudget) TryCharge(cost int64) (ok bool, reason string) {
	if cost < 1 {
		cost = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cost > b.capacity {
		return false, "kv_oversize"
	}
	if b.inFlight+cost > b.capacity {
		return false, "kv_pressure"
	}
	b.inFlight += cost
	return true, ""
}

// Release returns cost to the budget. Calls with cost <= 0 are no-ops.
func (b *KVBudget) Release(cost int64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight -= cost
	if b.inFlight < 0 {
		b.inFlight = 0
	}
}

// Reconfigure changes capacity. Already-charged occupancy keeps its
// place; if capacity shrinks below current inFlight, no eviction is
// performed (admitted requests run to completion, matching TokenBudget
// behavior).
func (b *KVBudget) Reconfigure(capacity int64) {
	if capacity < 1 {
		capacity = 1
	}
	b.mu.Lock()
	b.capacity = capacity
	b.mu.Unlock()
}

// Stats returns a snapshot of (capacity, inFlight).
func (b *KVBudget) Stats() (capacity, inFlight int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity, b.inFlight
}
