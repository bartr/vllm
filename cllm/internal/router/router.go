// Package router selects a *node.Node for an inbound completion request.
//
// Routing is the only piece of multi-node logic that has policy choice;
// admission, cost, and capacity live on the node itself. Routers are
// stateless and take a snapshot of the node list, so the handler can
// rebuild the slice on config reload without tearing down the router.
package router

import (
	"context"
	"errors"
	"strings"

	"cllm/internal/node"
)

// ErrNoMatch is returned when no node satisfies the request constraints.
// The handler maps this to HTTP 400 (or 503 depending on fallback policy).
var ErrNoMatch = errors.New("no node match")

// Request carries the routing-relevant request hints. It is intentionally
// small: the router only needs what is required to pick a node, not the
// full HTTP payload.
type Request struct {
	// Node, if non-empty, pins the request to a specific node ID
	// (typically set by the :dsl node= directive).
	Node string

	// Class, if non-empty, pins the request to a specific node class
	// (typically set by the :dsl node-class= directive).
	Class string

	// Cost is the total token cost the request would charge. Routers
	// may use it to refuse nodes whose remaining budget can't admit it.
	Cost int64
}

// Decision is the result of a routing call. Reason is a short tag suitable
// for logging and metric labels.
type Decision struct {
	Node   *node.Node
	Reason string
}

// Router selects one node from the provided slice. Implementations must be
// safe for concurrent use; they may not retain the nodes slice past the
// call.
type Router interface {
	Pick(ctx context.Context, req *Request, nodes []*node.Node) (Decision, error)
}

// ClassPinned honors an explicit pin (Request.Node or Request.Class). It
// returns ErrNoMatch when neither field is set, so it composes naturally
// with Chained{ClassPinned, LeastLoaded}.
//
// When Class is set and multiple nodes share that class, ClassPinned picks
// the least-loaded one within the class.
type ClassPinned struct{}

func (ClassPinned) Pick(_ context.Context, req *Request, nodes []*node.Node) (Decision, error) {
	if req == nil || (req.Node == "" && req.Class == "") {
		return Decision{}, ErrNoMatch
	}
	if req.Node != "" {
		for _, n := range nodes {
			if n != nil && n.ID == req.Node {
				return Decision{Node: n, Reason: "node-pinned"}, nil
			}
		}
		return Decision{}, ErrNoMatch
	}
	// Class pin: filter to nodes of the class, then least-loaded.
	want := strings.ToLower(req.Class)
	var class []*node.Node
	for _, n := range nodes {
		if n != nil && strings.ToLower(n.Class) == want {
			class = append(class, n)
		}
	}
	if len(class) == 0 {
		return Decision{}, ErrNoMatch
	}
	picked, ok := pickLeastLoaded(class)
	if !ok {
		return Decision{}, ErrNoMatch
	}
	return Decision{Node: picked, Reason: "class-pinned"}, nil
}

// LeastLoaded picks the node with the lowest in_flight/capacity ratio. It
// ignores any pin in Request and considers every healthy node. A node
// whose budget would overflow if charged Cost is skipped.
type LeastLoaded struct{}

func (LeastLoaded) Pick(_ context.Context, req *Request, nodes []*node.Node) (Decision, error) {
	cost := int64(0)
	if req != nil {
		cost = req.Cost
	}
	picked, ok := pickLeastLoadedWithCost(nodes, cost)
	if !ok {
		return Decision{}, ErrNoMatch
	}
	return Decision{Node: picked, Reason: "least-loaded"}, nil
}

// Chained tries each child router in order and returns the first non-error
// decision. The typical config is Chained{ClassPinned{}, LeastLoaded{}}:
// pinned requests are honored when they fit, everything else falls back to
// load balancing. If every child returns ErrNoMatch, Chained returns
// ErrNoMatch.
type Chained struct {
	Routers []Router
}

func (c Chained) Pick(ctx context.Context, req *Request, nodes []*node.Node) (Decision, error) {
	if len(c.Routers) == 0 {
		return Decision{}, ErrNoMatch
	}
	for _, r := range c.Routers {
		d, err := r.Pick(ctx, req, nodes)
		if err == nil && d.Node != nil {
			return d, nil
		}
		if err != nil && !errors.Is(err, ErrNoMatch) {
			return Decision{}, err
		}
	}
	return Decision{}, ErrNoMatch
}

// FromPolicy constructs the canonical router for a policy name as it
// appears in nodes.yaml. Unknown policies fall back to LeastLoaded after
// honoring class pins.
func FromPolicy(policy string) Router {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "class-pinned":
		return Chained{Routers: []Router{ClassPinned{}}}
	case "least-loaded", "":
		return Chained{Routers: []Router{ClassPinned{}, LeastLoaded{}}}
	case "chained":
		return Chained{Routers: []Router{ClassPinned{}, LeastLoaded{}}}
	default:
		return Chained{Routers: []Router{ClassPinned{}, LeastLoaded{}}}
	}
}

// pickLeastLoaded picks the node with the lowest in_flight / capacity
// ratio. Ties are broken by lexical ID for determinism.
func pickLeastLoaded(nodes []*node.Node) (*node.Node, bool) {
	return pickLeastLoadedWithCost(nodes, 0)
}

func pickLeastLoadedWithCost(nodes []*node.Node, cost int64) (*node.Node, bool) {
	var best *node.Node
	var bestRatio float64 = 2.0 // > any in-range ratio
	for _, n := range nodes {
		if n == nil || n.Budget == nil {
			continue
		}
		capacity, inFlight, _, _ := n.Budget.Stats()
		if capacity <= 0 {
			continue
		}
		// Reject nodes whose remaining budget can't admit the cost.
		// cost==0 disables this filter (used by ClassPinned where the
		// caller may want a pin even if it has to queue).
		if cost > 0 && inFlight+cost > capacity {
			continue
		}
		ratio := float64(inFlight) / float64(capacity)
		if ratio < bestRatio || (ratio == bestRatio && best != nil && n.ID < best.ID) {
			best = n
			bestRatio = ratio
		}
	}
	return best, best != nil
}
