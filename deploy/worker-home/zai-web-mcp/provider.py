"""Z.AI Web Search MCP + Web Reader MCP — Hermes web_search / web_extract backend.

Routes the built-in ``web_search`` and ``web_extract`` tools to Z.AI's remote MCP
servers (JSON-RPC over Streamable HTTP, Bearer auth):

* ``web_search_prime``  — https://api.z.ai/api/mcp/web_search_prime/mcp
* ``webReader``         — https://api.z.ai/api/mcp/web_reader/mcp

Config: ``web.search_backend: zai-mcp`` / ``web.extract_backend: zai-mcp``
(or ``web.backend: zai-mcp`` for both).

Env: ``ZAI_MCP_API_KEY``, falling back to ``GLM_API_KEY`` then ``Z_AI_API_KEY``.
Optional: ``ZAI_MCP_SEARCH_URL`` / ``ZAI_MCP_READER_URL`` / ``ZAI_MCP_TIMEOUT`` /
``ZAI_MCP_LOCATION`` (``cn`` | ``us``) / ``ZAI_MCP_CONTENT_SIZE`` (``medium`` | ``high``).
"""

from __future__ import annotations

import json
import logging
import os
import re
import threading
from typing import Any, Dict, List, Optional

from agent.web_search_provider import WebSearchProvider, get_provider_env

logger = logging.getLogger(__name__)

SEARCH_URL = os.getenv("ZAI_MCP_SEARCH_URL", "https://api.z.ai/api/mcp/web_search_prime/mcp")
READER_URL = os.getenv("ZAI_MCP_READER_URL", "https://api.z.ai/api/mcp/web_reader/mcp")
SEARCH_TOOL = "web_search_prime"
READER_TOOL = "webReader"

_KEY_ENVS = ("ZAI_MCP_API_KEY", "GLM_API_KEY", "Z_AI_API_KEY")
_PROTOCOL_VERSION = "2025-06-18"
_CLIENT_INFO = {"name": "hermes-agent", "version": "1.0"}
_DEFAULT_TIMEOUT = 60.0

# Response shape helpers — the wire contract is fixed (see agent/web_search_provider.py).
def search_ok(web_results: List[Dict[str, Any]]) -> Dict[str, Any]:
    return {"success": True, "data": {"web": web_results}}


def search_fail(error: str) -> Dict[str, Any]:
    return {"success": False, "error": error}


def web_hit(url: str, title: str, description: str, position: int) -> Dict[str, Any]:
    return {"url": url, "title": title, "description": description, "position": position}


def document(url: str, title: str, content: str) -> Dict[str, Any]:
    return {
        "url": url,
        "title": title,
        "content": content,
        "raw_content": content,
        "metadata": {"sourceURL": url, "title": title},
    }


def page_error(url: str, error: str) -> Dict[str, Any]:
    return {"url": url, "title": "", "content": "", "error": error}


_NAV_JUNK = re.compile(r"!\[[^\]]*\]\([^)\s]*\)")   # inline markdown images (logos, sprites) — pure token bloat


class ZaiMCPError(RuntimeError):
    """A Z.AI MCP call failed (transport, HTTP, JSON-RPC or tool-level error)."""


# --- MCP transport -----------------------------------------------------------
_SESSIONS: Dict[str, str] = {}
_SESSIONS_LOCK = threading.Lock()
_RPC_LOCK = threading.Lock()          # Z.AI binds auth to a single session id; serialize calls
_NEXT_ID = [0]


def _api_key() -> str:
    for env_name in _KEY_ENVS:
        try:
            value = get_provider_env(env_name)
        except Exception:  # scoped-secret edge cases must not break is_available()
            value = os.getenv(env_name, "")
        if value:
            return value.strip()
    return ""


def _next_id() -> int:
    with _SESSIONS_LOCK:
        _NEXT_ID[0] += 1
        return _NEXT_ID[0]


def _keep_images() -> bool:
    """Reader output keeps inline markdown images only when ZAI_MCP_KEEP_IMAGES is truthy."""
    return (os.getenv("ZAI_MCP_KEEP_IMAGES", "") or "").strip().lower() in ("1", "true", "yes", "on")


def _timeout() -> float:
    try:
        return float(os.getenv("ZAI_MCP_TIMEOUT", "") or _DEFAULT_TIMEOUT)
    except ValueError:
        return _DEFAULT_TIMEOUT


def _headers(session: Optional[str]) -> Dict[str, str]:
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "User-Agent": "hermes-agent/zai-mcp",
        "Authorization": f"Bearer {_api_key()}",
    }
    if session:
        headers["Mcp-Session-Id"] = session
    return headers


def _post(url: str, payload: Dict[str, Any], session: Optional[str], timeout: float) -> tuple[str, Optional[str]]:
    """POST one JSON-RPC message; return (body text, new Mcp-Session-Id or None)."""
    import requests

    try:
        response = requests.post(url, json=payload, headers=_headers(session), timeout=timeout)
    except requests.RequestException as exc:
        raise ZaiMCPError(f"request failed: {exc}") from exc

    body = response.content.decode("utf-8", errors="replace")
    if response.status_code >= 400:
        raise ZaiMCPError(f"HTTP {response.status_code}: {body[:300]}")
    return body, response.headers.get("Mcp-Session-Id") or response.headers.get("mcp-session-id")


def _envelope(body: str) -> Dict[str, Any]:
    """First JSON-RPC envelope carrying result/error (plain JSON or SSE ``data:`` frames)."""
    candidates = [body.strip()] if body.strip().startswith("{") else []
    candidates += [
        line[len("data:"):].strip()
        for line in re.split(r"\r\n|\r|\n", body)
        if line.startswith("data:")
    ]
    for candidate in candidates:
        try:
            payload = json.loads(candidate)
        except json.JSONDecodeError:
            continue
        if isinstance(payload, dict) and ("result" in payload or "error" in payload):
            return payload
    raise ZaiMCPError("unrecognized MCP response shape")


def _session_for(url: str, timeout: float) -> str:
    with _SESSIONS_LOCK:
        cached = _SESSIONS.get(url)
    if cached:
        return cached
    body, session = _post(
        url,
        {
            "jsonrpc": "2.0",
            "id": _next_id(),
            "method": "initialize",
            "params": {
                "protocolVersion": _PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": _CLIENT_INFO,
            },
        },
        None,
        timeout,
    )
    envelope = _envelope(body)
    if envelope.get("error"):
        raise ZaiMCPError(str((envelope["error"] or {}).get("message") or envelope["error"]))
    if not session:
        raise ZaiMCPError("initialize returned no Mcp-Session-Id")
    with _SESSIONS_LOCK:
        _SESSIONS[url] = session
    logger.debug("Z.AI MCP session established for %s", url)
    return session


def _drop_session(url: str, session: Optional[str]) -> None:
    with _SESSIONS_LOCK:
        if _SESSIONS.get(url) == session:
            _SESSIONS.pop(url, None)


def _tool_text(body: str) -> str:
    envelope = _envelope(body)
    error = envelope.get("error")
    if error:
        raise ZaiMCPError(str((error or {}).get("message") or error))
    result = envelope.get("result") or {}
    texts = [c.get("text", "") for c in result.get("content") or [] if isinstance(c, dict)]
    joined = " ".join(t for t in texts if t)
    if result.get("isError"):
        raise ZaiMCPError(joined or "MCP tool call failed")
    if not joined:
        raise ZaiMCPError("MCP tool returned no text content")
    return joined


def _call_tool(url: str, tool: str, arguments: Dict[str, Any], timeout: float) -> str:
    """tools/call with one transparent session re-init (Z.AI expires idle sessions)."""
    with _RPC_LOCK:
        for attempt in (1, 2):
            session = _session_for(url, timeout)
            payload = {
                "jsonrpc": "2.0",
                "id": _next_id(),
                "method": "tools/call",
                "params": {"name": tool, "arguments": arguments},
            }
            try:
                body = _post(url, payload, session, timeout)[0]
                return _tool_text(body)
            except ZaiMCPError as exc:
                message = str(exc).lower()
                stale = any(marker in message for marker in ("session", "http 400", "http 404", "http 410"))
                if attempt == 2 or not stale:
                    raise
                logger.info("Z.AI MCP session for %s looks stale (%s); re-initializing", url, exc)
                _drop_session(url, session)
    raise ZaiMCPError("unreachable")


def _unwrap(text: str) -> Any:
    """Z.AI returns its JSON payload string-encoded (sometimes twice) inside the MCP text item."""
    value: Any = text
    for _ in range(4):
        if not isinstance(value, str):
            return value
        stripped = value.strip()
        if not stripped or stripped[0] not in '[{"':
            return value
        try:
            value = json.loads(stripped)
        except json.JSONDecodeError:
            return value
    return value


# --- Provider ----------------------------------------------------------------
class ZaiMCPWebProvider(WebSearchProvider):
    """Search via Z.AI ``web_search_prime`` MCP, extract via Z.AI ``webReader`` MCP."""

    @property
    def name(self) -> str:
        return "zai-mcp"

    @property
    def display_name(self) -> str:
        return "Z.AI MCP (search + reader)"

    def is_available(self) -> bool:
        return bool(_api_key())

    def supports_search(self) -> bool:
        return True

    def supports_extract(self) -> bool:
        return True

    def search(self, query: str, limit: int = 5) -> Dict[str, Any]:
        if not _api_key():
            return search_fail("ZAI_MCP_API_KEY (or GLM_API_KEY) is not set")
        arguments: Dict[str, Any] = {
            "search_query": query,
            "content_size": os.getenv("ZAI_MCP_CONTENT_SIZE", "medium") or "medium",
        }
        if location := (os.getenv("ZAI_MCP_LOCATION", "") or "").strip():
            arguments["location"] = location
        try:
            payload = _unwrap(_call_tool(SEARCH_URL, SEARCH_TOOL, arguments, _timeout()))
        except ZaiMCPError as exc:
            return search_fail(f"Z.AI web search failed: {exc}")
        if isinstance(payload, dict):
            payload = payload.get("results") or payload.get("data") or payload.get("web") or []
        if not isinstance(payload, list):
            return search_fail("Z.AI web search returned an unexpected payload")

        rows: List[Dict[str, Any]] = []
        for item in payload:
            if not isinstance(item, dict):
                continue
            url = (item.get("link") or item.get("url") or "").strip()
            if not url:
                continue
            description = item.get("content") or item.get("summary") or item.get("description") or ""
            rows.append(web_hit(url, item.get("title") or "", description, len(rows) + 1))
            if limit and len(rows) >= int(limit):
                break
        logger.info("Z.AI search '%s': %d results", query, len(rows))
        return search_ok(rows)

    def extract(self, urls: List[str], **kwargs: Any) -> List[Dict[str, Any]]:
        if not _api_key():
            return [page_error(u, "ZAI_MCP_API_KEY (or GLM_API_KEY) is not set") for u in urls]
        max_chars = kwargs.get("max_chars")
        try:
            max_chars = int(max_chars) if max_chars else 0
        except (TypeError, ValueError):
            max_chars = 0

        results: List[Dict[str, Any]] = []
        for url in urls:
            try:
                payload = _unwrap(
                    _call_tool(
                        READER_URL,
                        READER_TOOL,
                        {
                            "url": url,
                            "return_format": "markdown",
                            "retain_images": False,
                            "no_cache": False,
                            "timeout": int(min(_timeout(), 60)),
                        },
                        _timeout(),
                    )
                )
            except ZaiMCPError as exc:
                results.append(page_error(url, f"Z.AI web reader failed: {exc}"))
                continue

            if not isinstance(payload, dict):
                results.append(page_error(url, "Z.AI web reader returned an unexpected payload"))
                continue
            if payload.get("error"):
                results.append(page_error(url, f"Z.AI web reader: {payload['error']}"))
                continue

            content = payload.get("content") or ""
            if not _keep_images():
                content = _NAV_JUNK.sub("", content)

            if max_chars and len(content) > max_chars:
                content = content[:max_chars] + "\n\n[max_chars truncated]"
            if not content.strip():
                results.append(page_error(url, "Z.AI web reader returned empty content"))
                continue
            entry = document(payload.get("url") or url, payload.get("title") or "", content)
            if isinstance(payload.get("description"), str) and payload["description"]:
                entry["metadata"]["description"] = payload["description"]
            results.append(entry)
        return results

    def get_setup_schema(self) -> Dict[str, Any]:
        return {
            "name": self.display_name,
            "badge": "coding plan",
            "tag": "Z.AI remote MCP — web_search_prime for search, webReader for extract.",
            "env_vars": [
                {
                    "key": "ZAI_MCP_API_KEY",
                    "prompt": "Z.AI API key (optional; falls back to GLM_API_KEY)",
                    "url": "https://z.ai/manage-apikey/apikey-list",
                }
            ],
        }
