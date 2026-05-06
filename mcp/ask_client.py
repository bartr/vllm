"""HTTP client for the askd (ask --serve) Kubernetes pod.

The MCP server no longer runs `ask --bench` as a local subprocess. All
benchmark control is delegated to the askd HTTP control plane that
lives in the cllm namespace. Tools call `ensure_deployed()` first so
operators get a clean, actionable error when the pod is not deployed.
"""

import httpx

import settings


class AskNotDeployedError(RuntimeError):
    """Raised when askd cannot be reached at CLLM_ASK_BASE_URL."""


def _deploy_hint() -> str:
    return (
        f"ask service unreachable at {settings.CLLM_ASK_BASE_URL}. "
        "Deploy with:  kubectl apply -k clusters/z01/ask/  "
        "(or wait for Flux to reconcile the 'ask' Kustomization)."
    )


async def ensure_deployed() -> None:
    """Probe GET /ready. Raise AskNotDeployedError on connect/HTTP failure."""
    url = settings.CLLM_ASK_BASE_URL + "/ready"
    try:
        async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
            r = await http.get(url)
            r.raise_for_status()
    except (httpx.ConnectError, httpx.ConnectTimeout, httpx.ReadTimeout) as e:
        raise AskNotDeployedError(_deploy_hint()) from e
    except httpx.HTTPStatusError as e:
        raise AskNotDeployedError(
            f"ask service at {settings.CLLM_ASK_BASE_URL} returned HTTP "
            f"{e.response.status_code}; not ready. {_deploy_hint()}"
        ) from e


async def get_status() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(settings.CLLM_ASK_BASE_URL + "/bench")
        r.raise_for_status()
        return r.json()


async def start_bench(spec: dict) -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/bench", json=spec)
        r.raise_for_status()
        return r.json()


async def pause_bench() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/bench/pause")
        r.raise_for_status()
        return r.json()


async def start_or_resume_bench() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/bench/start")
        r.raise_for_status()
        return r.json()


async def stop_bench() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/bench/stop")
        r.raise_for_status()
        return r.json()


async def restart_bench() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/bench/restart")
        r.raise_for_status()
        return r.json()


async def list_logs() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(settings.CLLM_ASK_BASE_URL + "/logs")
        r.raise_for_status()
        return r.json()


async def get_version() -> dict:
    """GET /version. Used by status tools; does NOT call ensure_deployed
    so it can be used as the deploy probe itself."""
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(settings.CLLM_ASK_BASE_URL + "/version")
        r.raise_for_status()
        return r.json()


async def tail_log_text(name: str, tail_bytes: int | None = None) -> str:
    """Return the raw text of a single per-run log file."""
    await ensure_deployed()
    params = {}
    if tail_bytes is not None and tail_bytes > 0:
        params["tail"] = str(tail_bytes)
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(settings.CLLM_ASK_BASE_URL + f"/logs/{name}", params=params)
        r.raise_for_status()
        return r.text


async def get_config() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(settings.CLLM_ASK_BASE_URL + "/config")
        r.raise_for_status()
        return r.json()


async def update_config(patch: dict) -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.put(settings.CLLM_ASK_BASE_URL + "/config", json=patch)
        r.raise_for_status()
        return r.json()


async def reset_config() -> dict:
    await ensure_deployed()
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(settings.CLLM_ASK_BASE_URL + "/config/reset")
        r.raise_for_status()
        return r.json()
