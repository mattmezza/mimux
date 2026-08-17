# SPDX-License-Identifier: AGPL-3.0-only
#
# mimux inbox triage — the mimux pro MCP server driven by Claude.
#
# Reads unread mail, stars what needs a human reply, archives newsletters and
# notifications, and drafts (never sends) replies where one is obvious. The
# send_draft tool is stripped from the tool list before the model ever sees it,
# so nothing can go out without a human pressing Send in mimux.
#
# Run:  MIMUX_TOKEN=mimux_pat_... ANTHROPIC_API_KEY=sk-ant-... uv run triage.py
#
# /// script
# requires-python = ">=3.11"
# dependencies = ["anthropic>=0.60", "mcp>=1.9"]
# ///
import asyncio
import json
import os
import sys

from anthropic import Anthropic
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

MIMUX_URL = os.environ.get("MIMUX_URL", "http://localhost:8083")
MODEL = "claude-opus-5"

SYSTEM = """You are triaging the user's mailbox through mimux's MCP tools.

Work through the unread mail (search_mail with `is:unread`, then read_message
for anything that needs more than the snippet) and for each message do exactly
one of:

- star_message: a human needs to read or reply to this personally
  (real correspondence, questions, anything time-sensitive).
- move_message with target=archive: newsletters, notifications, receipts,
  automated mail nobody will act on. When unsure, leave it alone instead.
- draft_reply: ONLY when the right reply is obvious and low-stakes
  (confirmations, scheduling acknowledgements). Keep drafts short and plain.
  Never call send_draft — drafts wait for the human.

Never move anything to trash or spam. When you are done, summarise what you
did as a short list: starred / archived / drafted / left alone, with one-line
reasons."""


def to_anthropic_tools(mcp_tools):
    """MCP tool declarations -> Messages API tool dicts, minus send_draft."""
    tools = []
    for t in mcp_tools:
        if t.name == "send_draft":  # belt and braces: never even offered
            continue
        tools.append({
            "name": t.name,
            "description": t.description or "",
            "input_schema": t.inputSchema,
        })
    return tools


def result_text(result):
    """Flatten an MCP tool result into text for the tool_result block."""
    if result.structuredContent is not None:
        return json.dumps(result.structuredContent)
    parts = [c.text for c in result.content if getattr(c, "text", None)]
    return "\n".join(parts) or "(empty result)"


async def main():
    token = os.environ.get("MIMUX_TOKEN")
    if not token:
        sys.exit("Set MIMUX_TOKEN to a mimux API token (Settings → API) "
                 "with mail:read, mail:send and mail:modify scopes.")

    client = Anthropic()  # ANTHROPIC_API_KEY from the environment

    async with streamablehttp_client(
        f"{MIMUX_URL}/api/mcp",
        headers={"Authorization": f"Bearer {token}"},
    ) as (read, write, _):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = to_anthropic_tools((await session.list_tools()).tools)
            print(f"Connected to {MIMUX_URL} — {len(tools)} tools\n")

            messages = [{"role": "user", "content": "Triage my inbox."}]
            while True:
                response = client.beta.messages.create(
                    model=MODEL,
                    max_tokens=16000,
                    system=SYSTEM,
                    tools=tools,
                    messages=messages,
                    # Safety classifiers can decline a request; fall back to
                    # Anthropic's recommended substitute instead of stopping.
                    betas=["server-side-fallback-2026-07-01"],
                    extra_body={"fallbacks": "default"},
                )

                for block in response.content:
                    if block.type == "text" and block.text.strip():
                        print(block.text.strip(), "\n")

                if response.stop_reason == "refusal":
                    print("The model declined this request; nothing was changed.")
                    return
                if response.stop_reason != "tool_use":
                    return  # end_turn: triage finished

                messages.append({"role": "assistant", "content": response.content})
                results = []
                for block in response.content:
                    if block.type != "tool_use":
                        continue
                    print(f"  -> {block.name} {json.dumps(block.input)}")
                    r = await session.call_tool(block.name, block.input)
                    results.append({
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": result_text(r),
                        "is_error": bool(r.isError),
                    })
                messages.append({"role": "user", "content": results})


if __name__ == "__main__":
    asyncio.run(main())
