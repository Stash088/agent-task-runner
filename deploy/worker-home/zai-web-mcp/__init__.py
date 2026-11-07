"""Z.AI Web Search + Web Reader MCP backend plugin.

Registers the ``zai-mcp`` web provider, which serves Hermes' built-in
``web_search`` (Z.AI ``web_search_prime``) and ``web_extract`` (Z.AI ``webReader``).
"""

from .provider import ZaiMCPWebProvider


def register(ctx) -> None:
    ctx.register_web_search_provider(ZaiMCPWebProvider())
