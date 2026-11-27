"""Adds the compiled Anthropic server tools to each Messages API turn request.

Hermes 0.23.4 builds the Anthropic ``tools`` list from function tools only
(agent/anthropic_message_convert.py convert_tools_to_anthropic), so this
llm_request middleware appends the server tool entries the kagent adapter
wrote into ``server_tools.json`` beside this file. Anthropic runs these tools;
the Actor makes no request to their targets.

Hermes replays a turn's text and thinking but drops server tool blocks
(``_sanitize_replay_block`` keeps known types only). That edits the history
before later thinking blocks, so each request also asks the API to drop a
mismatched thinking block instead of failing with HTTP 400.
"""

import json
from pathlib import Path

_CONFIG = json.loads(Path(__file__).with_name("server_tools.json").read_text())
_TOOLS = _CONFIG["tools"]
_BINDING_BETA = "thinking-binding-controls-2026-08-01"


def _merge_beta(headers, beta):
    headers = dict(headers or {})
    key = next((k for k in headers if k.lower() == "anthropic-beta"), "anthropic-beta")
    betas = [b.strip() for b in str(headers.get(key, "")).split(",") if b.strip()]
    if beta not in betas:
        betas.append(beta)
    headers[key] = ",".join(betas)
    return headers


def add_server_tools(request=None, api_mode=None, **_):
    if api_mode != "anthropic_messages" or not isinstance(request, dict):
        return None
    request = dict(request)
    tools = [t for t in request.get("tools") or [] if isinstance(t, dict)]
    names = {t.get("name") for t in tools}
    request["tools"] = tools + [dict(t) for t in _TOOLS if t["name"] not in names]
    thinking = request.get("thinking")
    if thinking is None or (isinstance(thinking, dict) and thinking.get("type") == "adaptive"):
        thinking = dict(thinking or {"type": "adaptive"})
        thinking["block_binding"] = {"prefix_mismatch_behavior": "drop_block"}
        request["thinking"] = thinking
        request["extra_headers"] = _merge_beta(request.get("extra_headers"), _BINDING_BETA)
    return {"request": request, "source": "kagent-server-tools", "reason": "add Anthropic server tools"}


def register(ctx):
    ctx.register_middleware("llm_request", add_server_tools)
