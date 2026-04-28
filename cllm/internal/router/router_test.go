package router

import (
	"context"
	"errors"
	"testing"

	"cllm/internal/node"
)

func makeNode(t *testing.T, id, class string, capacity, inFlight int64) *node.Node {
	t.Helper()
	b := node.NewTokenBudget(capacity, 100)
	if inFlight > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, ok := b.Acquire(ctx, inFlight); !ok {
			t.Fatalf("preload acquire(%d) on capacity %d failed", inFlight, capacity)
		}
	}
	return &node.Node{
		ID:        id,
		Class:     class,
		Budget:    b,
		Estimator: node.NewCompletionEstimator(256, 50),
		Capacity:  node.Capacity{MaxTokensInFlight: capacity, MaxWaitingRequests: 100},
	}
}

func TestClassPinnedNoHints(t *testing.T) {
	r := ClassPinned{}
	nodes := []*node.Node{makeNode(t, "a", "H100", 1000, 0)}
	if _, err := r.Pick(context.Background(), &Request{}, nodes); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch with no hints, got %v", err)
	}
}

func TestClassPinnedByID(t *testing.T) {
	r := ClassPinned{}
	a := makeNode(t, "a", "H100", 1000, 0)
	b := makeNode(t, "b", "A10", 1000, 0)
	d, err := r.Pick(context.Background(), &Request{Node: "b"}, []*node.Node{a, b})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node != b {
		t.Fatalf("got %s, want b", d.Node.ID)
	}
	if d.Reason != "node-pinned" {
		t.Fatalf("reason: %q", d.Reason)
	}
}

func TestClassPinnedByClassChoosesLeastLoaded(t *testing.T) {
	r := ClassPinned{}
	loaded := makeNode(t, "a10-busy", "A10", 1000, 800)
	idle := makeNode(t, "a10-idle", "A10", 1000, 100)
	other := makeNode(t, "h100-0", "H100", 1000, 0)
	d, err := r.Pick(context.Background(), &Request{Class: "A10"}, []*node.Node{loaded, idle, other})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node.ID != "a10-idle" {
		t.Fatalf("got %s, want a10-idle", d.Node.ID)
	}
	if d.Reason != "class-pinned" {
		t.Fatalf("reason: %q", d.Reason)
	}
}

func TestClassPinnedClassUnknown(t *testing.T) {
	r := ClassPinned{}
	nodes := []*node.Node{makeNode(t, "a", "H100", 1000, 0)}
	if _, err := r.Pick(context.Background(), &Request{Class: "TPU"}, nodes); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
}

func TestLeastLoadedPicksLowestRatio(t *testing.T) {
	r := LeastLoaded{}
	a := makeNode(t, "a", "H100", 1000, 900)  // 90%
	b := makeNode(t, "b", "A10", 1000, 100)   // 10%
	c := makeNode(t, "c", "rtx", 1000, 500)   // 50%
	d, err := r.Pick(context.Background(), &Request{}, []*node.Node{a, b, c})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node.ID != "b" {
		t.Fatalf("got %s, want b", d.Node.ID)
	}
}

func TestLeastLoadedSkipsOverflowingNodes(t *testing.T) {
	r := LeastLoaded{}
	tight := makeNode(t, "tight", "A10", 1000, 950)   // 5% headroom < cost
	loose := makeNode(t, "loose", "H100", 1000, 800)  // 20% headroom >= cost
	d, err := r.Pick(context.Background(), &Request{Cost: 100}, []*node.Node{tight, loose})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node.ID != "loose" {
		t.Fatalf("got %s, want loose", d.Node.ID)
	}
}

func TestLeastLoadedAllOverflowReturnsNoMatch(t *testing.T) {
	r := LeastLoaded{}
	a := makeNode(t, "a", "A10", 100, 100)
	b := makeNode(t, "b", "A10", 100, 100)
	if _, err := r.Pick(context.Background(), &Request{Cost: 50}, []*node.Node{a, b}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
}

func TestChainedFallsThroughOnNoMatch(t *testing.T) {
	r := Chained{Routers: []Router{ClassPinned{}, LeastLoaded{}}}
	a := makeNode(t, "a", "H100", 1000, 0)
	// No pin: ClassPinned -> ErrNoMatch, LeastLoaded -> a.
	d, err := r.Pick(context.Background(), &Request{}, []*node.Node{a})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node.ID != "a" || d.Reason != "least-loaded" {
		t.Fatalf("got %s/%s, want a/least-loaded", d.Node.ID, d.Reason)
	}
}

func TestChainedHonorsPinFirst(t *testing.T) {
	r := Chained{Routers: []Router{ClassPinned{}, LeastLoaded{}}}
	loaded := makeNode(t, "pinned", "A10", 1000, 900)
	idle := makeNode(t, "idle", "H100", 1000, 0)
	d, err := r.Pick(context.Background(), &Request{Node: "pinned"}, []*node.Node{loaded, idle})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.Node.ID != "pinned" || d.Reason != "node-pinned" {
		t.Fatalf("got %s/%s, want pinned/node-pinned", d.Node.ID, d.Reason)
	}
}

func TestChainedEmptyReturnsNoMatch(t *testing.T) {
	r := Chained{}
	if _, err := r.Pick(context.Background(), &Request{}, []*node.Node{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
}

func TestFromPolicy(t *testing.T) {
	cases := []struct {
		policy string
		ok     bool
	}{
		{"least-loaded", true},
		{"class-pinned", true},
		{"chained", true},
		{"", true},
		{"unknown", true}, // falls back to chained default
	}
	a := makeNode(t, "a", "H100", 1000, 0)
	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			r := FromPolicy(tc.policy)
			if r == nil {
				t.Fatal("FromPolicy returned nil")
			}
			// Sanity: with a single available node and no pin, all
			// policies that include LeastLoaded should pick a;
			// class-pinned without a pin returns ErrNoMatch.
			d, err := r.Pick(context.Background(), &Request{}, []*node.Node{a})
			if tc.policy == "class-pinned" {
				if !errors.Is(err, ErrNoMatch) {
					t.Fatalf("class-pinned w/o pin should ErrNoMatch, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pick: %v", err)
			}
			if d.Node.ID != "a" {
				t.Fatalf("got %s", d.Node.ID)
			}
		})
	}
}
