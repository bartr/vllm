import os

CLLM_BASE_URL = os.environ.get("CLLM_BASE_URL", "http://192.168.68.63:8088").rstrip("/")
CLLM_GRAFANA_URL = os.environ.get(
    "CLLM_GRAFANA_URL",
    "http://192.168.68.63:3000/d/cllm-overview/cllm-overview",
)
CLLM_BENCH_LOG = os.environ.get("CLLM_BENCH_LOG", "~/logs/bench.log")
CLLM_READ_ONLY = os.environ.get("CLLM_READ_ONLY", "false").lower() == "true"
CLLM_PROTECT_REAL_NODE = os.environ.get("CLLM_PROTECT_REAL_NODE", "true").lower() == "true"
CLLM_REQUEST_TIMEOUT_SECONDS = float(os.environ.get("CLLM_REQUEST_TIMEOUT_SECONDS", "10"))
CLLM_BENCH_WARMUP_SECONDS = int(os.environ.get("CLLM_BENCH_WARMUP_SECONDS", "15"))
CLLM_AUDIT_LOG = os.environ.get("CLLM_AUDIT_LOG", "~/logs/cllm-audit.log")

_HERE = os.path.dirname(os.path.abspath(__file__))
BENCHMARK_DIR = os.environ.get(
    "CLLM_BENCHMARK_DIR",
    os.path.normpath(os.path.join(_HERE, "..", "benchmark")),
)
BENCHMARK_SCENARIOS_DIR = os.path.join(BENCHMARK_DIR, "scenarios")
BENCHMARK_REPORTS_DIR  = os.path.join(BENCHMARK_DIR, "reports")
BENCHMARK_LOGS_DIR     = os.path.join(BENCHMARK_DIR, "logs")

PROTECTED_NODES: frozenset[str] = frozenset({"vllm"}) if CLLM_PROTECT_REAL_NODE else frozenset()
