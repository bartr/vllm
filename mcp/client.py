import httpx
import settings

_HEADERS = {"Accept": "application/json"}


async def get(path: str) -> dict:
    url = settings.CLLM_BASE_URL + path
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(url, headers=_HEADERS)
        r.raise_for_status()
        return r.json()


async def post(path: str, body: dict) -> dict:
    url = settings.CLLM_BASE_URL + path
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.post(url, json=body, headers=_HEADERS)
        r.raise_for_status()
        return r.json()


async def put(path: str, body: dict) -> dict:
    url = settings.CLLM_BASE_URL + path
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.put(url, json=body, headers=_HEADERS)
        r.raise_for_status()
        return r.json()


async def get_text(path: str) -> str:
    url = settings.CLLM_BASE_URL + path
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as http:
        r = await http.get(url)
        r.raise_for_status()
        return r.text
