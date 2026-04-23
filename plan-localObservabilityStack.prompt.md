## Plan: Local Observability Stack

Add a first-phase in-cluster observability stack on K3s using Prometheus and Grafana, then layer in node, GPU, and infrastructure-cost telemetry. The recommended path is to follow the repo’s current install pattern: Helm-managed cluster add-ons with values files checked into the repo, plus minimal changes to the existing vLLM manifests so Prometheus can scrape application metrics when available.

**Steps**
1. Phase 1: Define the observability footprint and install pattern.
2. Standardize on an in-cluster `monitoring` namespace and keep access local-first via `NodePorts` initially; only add ingress after the stack is stable. This keeps the first phase simpler and avoids coupling Grafana/Prometheus exposure to Traefik auth work.
3. Reuse the existing repo convention from the NVIDIA device plugin installer: add one or more install scripts under the existing `scripts/` flow and commit Helm values under `helm/` for reproducible installs.
4. Phase 2: Add Prometheus and Grafana.
5. Introduce a Prometheus/Grafana bundle, preferably via `kube-prometheus-stack`, because it gives Prometheus, Grafana, Alertmanager, kube-state-metrics, and node-exporter in one install path and reduces custom YAML surface. This phase depends on step 1.
6. Scope the initial values to single-node, local use: short retention, modest PVC sizes, and conservative resource requests so the monitoring stack does not compete too aggressively with the vLLM pod for memory/CPU.
7. Configure Grafana with a prewired Prometheus datasource and create a minimal dashboard baseline for node CPU/memory/disk, Kubernetes pod health, and vLLM pod resource usage. This can run in parallel with step 8 once the base chart choice is fixed.
8. Phase 3: Add GPU telemetry.
9. Install NVIDIA DCGM exporter in-cluster so Prometheus can scrape GPU utilization, memory used, temperature, power draw, and clocks. This is the missing layer the current NVIDIA device plugin does not provide. This phase depends on step 5.
10. Ensure Prometheus discovers the DCGM exporter target either through ServiceMonitor resources from the monitoring stack or through static/annotation-based scrape config, depending on the final chart choices.
11. Add a Grafana dashboard focused on the single-GPU node: GPU utilization, framebuffer memory use, power draw, temperature, and correlation with the active vLLM pod.
12. Phase 4: Add infrastructure cost visibility.
13. Install OpenCost after Prometheus is in place so it can estimate node, CPU, memory, storage, and GPU infrastructure cost from cluster metrics. This phase depends on step 5 and step 9.
14. Start with infrastructure cost only, not per-request inference cost. Use OpenCost to show daily/monthly cluster cost and validate that GPU pricing can be configured explicitly for the local node if default cloud pricing is not meaningful.
15. If OpenCost’s built-in pricing assumptions are not useful for a local laptop node, document a simple custom pricing configuration for CPU, RAM, GPU, and storage so the dashboards reflect a realistic local operating-cost model.
16. Phase 5: Hook in vLLM application metrics.
17. Verify whether the pinned `vllm/vllm-openai:v0.19.1` image exposes Prometheus metrics directly and whether an explicit runtime flag is required. If built-in metrics are available, expose and annotate the service or add the required ServiceMonitor. If they are not, defer application-level latency/token dashboards and rely on pod, node, and GPU telemetry for phase 1.
18. Update the shared vLLM service/deployment pattern used by the model manifests so the active profile remains scrapeable without changing the service identity. This step depends on the result of step 17.
19. Phase 6: Document and validate.
20. Extend the README with observability install, access, and verification steps, including how to open Grafana locally, where to find the GPU and cost dashboards, and what metrics are expected to appear first.
21. Add verification steps that prove each layer independently: Prometheus targets up, node-exporter metrics present, DCGM metrics present, OpenCost reporting non-zero cost data, and vLLM metrics present if supported.

**Relevant files**
- `/home/bartr/vllm/README.md` — extend setup docs with install/access/verification steps for monitoring.
- `/home/bartr/vllm/manifests/vllm-small.yaml` — template for any vLLM service annotations, metrics port exposure, or pod labels needed for scraping.
- `/home/bartr/vllm/manifests/vllm-medium.yaml` — keep the shared deployment shape aligned with the small profile changes.
- `/home/bartr/vllm/manifests/vllm-large.yaml` — keep the shared deployment shape aligned with the large profile changes.

**Verification**
1. Install the monitoring stack and confirm all monitoring pods in the `monitoring` namespace become Ready.
2. Port-forward Grafana and Prometheus locally and confirm Prometheus target health for node-exporter, kube-state-metrics, Prometheus itself, and DCGM exporter.
3. Confirm GPU metrics such as utilization, memory used, and power draw are queryable in Prometheus.
4. Confirm OpenCost surfaces infrastructure cost data for the node and workload after Prometheus has collected data for a few minutes.
5. If vLLM metrics are supported by the pinned image, confirm the active `vllm` workload appears as a healthy scrape target and basic request/latency metrics populate during a test query via `ask.sh`.
6. Run one manual workload test and verify Grafana shows correlated changes in pod CPU, memory, GPU utilization, and estimated cost.

**Decisions**
- Include: local-first, in-cluster Prometheus/Grafana, node metrics, GPU metrics, Kubernetes workload metrics, and infrastructure cost visibility.
- Exclude from phase 1: remote metrics shipping, alert routing, long-term retention, multi-node federation, and precise per-token/request chargeback.
- Recommendation: use `kube-prometheus-stack` rather than hand-rolled Prometheus/Grafana manifests because it lowers maintenance and gives a better baseline for future expansion.
- Recommendation: use `kubectl port-forward` first for Grafana/Prometheus access; add ingress only after the dashboards and scrape targets are stable.

**Further Considerations**
1. vLLM metrics support is the main technical unknown. If the pinned image does not expose Prometheus metrics cleanly, phase 1 should still proceed with system, GPU, and cost observability while app-level metrics are deferred.
2. OpenCost on a local single-node cluster may need explicit pricing overrides to make GPU cost realistic; that should be treated as expected, not exceptional.
3. If you later want auth-protected shared access to Grafana, reuse the existing Traefik basic-auth pattern after the local-first setup is validated.
