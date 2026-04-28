// Package node holds the per-node primitives that model a single vLLM-like
// instance inside cLLM: an admission-stock token budget, a rolling p95
// completion-token estimator, the request-cost type that flows through them,
// and a Node struct that packages capacity, realism, and optional upstream
// configuration.
//
// In Phase 1 (the refactor) these types are extracted from the httpapi
// package without changing handler wiring; the handler still holds them as
// individual fields. Phase 2 introduces a []*Node list and a Router.
//
// See multi-node-design.md for the full design.
package node
