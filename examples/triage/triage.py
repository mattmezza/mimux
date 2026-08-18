# SPDX-License-Identifier: AGPL-3.0-only
#
# mimux inbox triage — the mimux pro MCP server driven by DeepSeek.
#
# Reads unread mail, stars what needs a human reply, archives newsletters and
# notifications, and drafts (never sends) replies where one is obvious. The
# send_draft tool is stripped from the tool list before the model ever sees it,
# so nothing can go out without a human pressing Send in mimux.
#
# Run:  MIMUX_TOKEN=mimux_pat_... DEEPSEEK_API_KEY=sk-... uv run triage.py
#
# /// script
# requires-python = ">=3.11"
# dependencies = ["openai>=1.40", "mcp>=1.9"]
# ///
import asyncio
import json
import os
import sys

from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client
from openai import OpenAI

MIMUX_URL = os.environ.get("MIMUX_URL", "http://localhost:8083")
# DeepSeek's API is OpenAI-compatible; any OpenAI-compatible provider and
# model works here — edit MODEL and the base_url below to swap.
MODEL = "deepseek-v4-flash"

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


def to_openai_tools(mcp_tools):
    """MCP tool declarations -> chat-completions tool dicts, minus send_draft."""
    tools = []
    for t in mcp_tools:
        if t.name == "send_draft":  # belt and braces: never even offered
            continue
        tools.append({
            "type": "function",
            "function": {
                "name": t.name,
                "description": t.description or "",
                "parameters": t.inputSchema,
            },
        })
    return tools


def result_text(result):
    """Flatten an MCP tool result into text for the tool message."""
    if result.structuredContent is not None:
        return json.dumps(result.structuredContent)
    parts = [c.text for c in result.content if getattr(c, "text", None)]
    return "\n".join(parts) or "(empty result)"


async def main():
    token = os.environ.get("MIMUX_TOKEN")
    if not token:
        sys.exit("Set MIMUX_TOKEN to a mimux API token (Settings → API) "
                 "with mail:read, mail:send and mail:modify scopes.")
    if not os.environ.get("DEEPSEEK_API_KEY"):
        sys.exit("Set DEEPSEEK_API_KEY (an OpenAI-compatible key works with "
                 "a matching base_url).")

    client = OpenAI(
        base_url="https://api.deepseek.com",
        api_key=os.environ["DEEPSEEK_API_KEY"],
    )

    async with streamablehttp_client(
        f"{MIMUX_URL}/api/mcp",
        headers={"Authorization": f"Bearer {token}"},
    ) as (read, write, _):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = to_openai_tools((await session.list_tools()).tools)
            print(f"Connected to {MIMUX_URL} — {len(tools)} tools\n")

            messages = [
                {"role": "system", "content": SYSTEM},
                {"role": "user", "content": "Triage my inbox."},
            ]
            while True:
                response = client.chat.completions.create(
                    model=MODEL,
                    max_tokens=16000,
                    tools=tools,
                    messages=messages,
                )
                msg = response.choices[0].message

                if msg.content and msg.content.strip():
                    print(msg.content.strip(), "\n")

                if not msg.tool_calls:
                    return  # triage finished

                messages.append({
                    "role": "assistant",
                    "content": msg.content,
                    "tool_calls": [tc.model_dump() for tc in msg.tool_calls],
                })
                for tc in msg.tool_calls:
                    args = json.loads(tc.function.arguments or "{}")
                    print(f"  -> {tc.function.name} {json.dumps(args)}")
                    r = await session.call_tool(tc.function.name, args)
                    messages.append({
                        "role": "tool",
                        "tool_call_id": tc.id,
                        "content": result_text(r),
                    })


if __name__ == "__main__":
    asyncio.run(main())
