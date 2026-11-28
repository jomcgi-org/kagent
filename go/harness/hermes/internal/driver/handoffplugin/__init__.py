"""Writes the session's latest compaction summary for the kagent handoff note.

After each answered turn (``post_llm_call``), this finds the newest Hermes
compaction summary in the turn's working messages and writes its body to
``$HERMES_HOME/kagent-handoff.md`` when it changed. The kagent adapter sends
that file to the memory MCP server's ``handoff_write`` after the turn, so a
later session of the same conversation starts from it. The plugin writes
nothing else and sends nothing.
"""

import os
from pathlib import Path

_FILE = "kagent-handoff.md"
# The notes vault takes at most 16 KiB; leave room for UTF-8 clipping.
_MAX_BYTES = 16 * 1024
_PREFIXES = ("[CONTEXT COMPACTION", "[CONTEXT SUMMARY]:")

try:  # Hermes 0.23.4: agent/context_compressor.py
    from agent.context_compressor import ContextCompressor as _Compressor
except Exception:  # pragma: no cover - another Hermes layout
    _Compressor = None


def _text(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for part in content:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                parts.append(part["text"])
            elif isinstance(part, str):
                parts.append(part)
        return "".join(parts)
    return ""


def _summary(content):
    """Return the summary body of a compaction message, or None."""
    text = _text(content).strip()
    if not text:
        return None
    if _Compressor is not None:
        try:
            if _Compressor.classify_summary_content(content) is None:
                return None
            return _Compressor._strip_summary_prefix(text).strip() or None
        except Exception:
            pass
    if not text.startswith(_PREFIXES):
        return None
    return text


def _clip(text):
    raw = text.encode("utf-8")
    if len(raw) <= _MAX_BYTES:
        return text
    return raw[:_MAX_BYTES].decode("utf-8", errors="ignore")


def write_handoff(conversation_history=None, **_):
    if not isinstance(conversation_history, list):
        return None
    summary = None
    for message in reversed(conversation_history):
        if isinstance(message, dict):
            summary = _summary(message.get("content"))
            if summary:
                break
    if not summary:
        return None
    home = os.environ.get("HERMES_HOME")
    if not home:
        return None
    path = Path(home) / _FILE
    summary = _clip(summary)
    try:
        if path.read_text(encoding="utf-8") == summary:
            return None
    except OSError:
        pass
    tmp = path.with_name(_FILE + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write(summary)
    os.replace(tmp, path)
    return None


def register(ctx):
    ctx.register_hook("post_llm_call", write_handoff)
