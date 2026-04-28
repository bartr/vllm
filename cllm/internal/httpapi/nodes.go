package httpapi

import (
	"context"
	"log/slog"
	"strings"

	"cllm/internal/node"
	"cllm/internal/router"
)

// SetNodes installs a multi-node fleet on the handler. The first call
// replaces the default single-node slice; admission still flows through
// h.scheduler in this phase, but the router decision is logged so the
// routing surface is observable end to end.
//
// The supplied policy maps to a router.Router via router.FromPolicy. An
// empty policy yields the default Chained{ClassPinned, LeastLoaded}.
//
// Phase 2.4 will switch admission and per-node metrics over to use these
// nodes; Phase 2.3 only plumbs them in.
func (h *Handler) SetNodes(nodes []*node.Node, policy string) {
	if h == nil {
		return
	}
	h.configMu.Lock()
	if len(nodes) == 0 {
		// Preserve the default single node so the rest of the handler
		// can keep referencing h.nodes[0].
		if h.scheduler != nil {
			h.nodes = []*node.Node{h.scheduler.node}
		} else {
			h.nodes = nil
		}
	} else {
		h.nodes = nodes
	}
	h.router = router.FromPolicy(policy)
	h.configMu.Unlock()
}

// NodeIDs returns the IDs of the configured nodes in slice order. Used by
// the /config endpoint and tests; safe for concurrent use.
func (h *Handler) NodeIDs() []string {
	if h == nil {
		return nil
	}
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	out := make([]string, 0, len(h.nodes))
	for _, n := range h.nodes {
		if n == nil {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

// pickRoutedNode runs the configured router and returns the chosen node
// (if any) and a short reason tag. Phase 2.3: advisory only. Returns nil
// when the router can't match (e.g. ClassPinned-only with no pin in the
// request).
func (h *Handler) pickRoutedNode(ctx context.Context, overrides replayOverrides, cost node.RequestCost) (*node.Node, string) {
	h.configMu.RLock()
	nodes := h.nodes
	r := h.router
	h.configMu.RUnlock()
	if r == nil || len(nodes) == 0 {
		return nil, ""
	}
	req := &router.Request{
		Node:  strings.TrimSpace(overrides.nodeID),
		Class: strings.TrimSpace(overrides.nodeClass),
		Cost:  int64(cost.TotalCost),
	}
	d, err := r.Pick(ctx, req, nodes)
	if err != nil {
		return nil, ""
	}
	return d.Node, d.Reason
}

// logRouterDecisionIfMultiNode emits a structured log line describing the
// router's choice when the fleet is larger than one node OR when the
// request carried an explicit pin. Single-node fleets without pins are
// silent to avoid noise. Used in the chat/completions hot path.
func (h *Handler) logRouterDecisionIfMultiNode(ctx context.Context, n *node.Node, reason string, overrides replayOverrides) {
	if n == nil {
		return
	}
	h.configMu.RLock()
	multi := len(h.nodes) > 1
	h.configMu.RUnlock()
	if !multi && overrides.nodeID == "" && overrides.nodeClass == "" {
		return
	}
	logger := loggerFromContext(ctx)
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("routed",
		"node_id", n.ID,
		"node_class", n.Class,
		"reason", reason,
		"pin_node", overrides.nodeID,
		"pin_class", overrides.nodeClass,
	)
}
