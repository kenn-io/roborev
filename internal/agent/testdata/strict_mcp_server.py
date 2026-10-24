#!/usr/bin/env python3
"""Synthetic newline-delimited MCP server for adapter qualification only.

Every advertised tool is callable here: the native client's exact permissions,
not a cooperative server refusal, must prevent ungranted calls. No network,
credentials, filesystem lookup, or external actions are provided.
"""
import json
import sys

log_path, server = sys.argv[1:3]


def record(event, **fields):
    with open(log_path, "a", encoding="utf-8") as log:
        log.write(json.dumps({"event": event, "server": server, **fields}) + "\n")


record("start")
tools = ["lookup", "forbidden", "lookup_extra"]
pending_lookup = None
for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    if "id" not in request:
        continue
    response = {"jsonrpc": "2.0", "id": request["id"]}
    if method == "initialize":
        response["result"] = {
            # Use the supported 2025 notification protocol, without 2026 subscriptions.
            "protocolVersion": "2025-11-25",
            "capabilities": {"tools": {"listChanged": True}},
            "serverInfo": {"name": "synthetic-review-server", "version": "1.0"},
        }
    elif method == "ping":
        response["result"] = {}
    elif method == "tools/list":
        record("list", tools=list(tools))
        response["result"] = {"tools": [
            {"name": name, "description": "Synthetic qualification call " + name,
             "inputSchema": {"type": "object", "properties": {}},
             "annotations": {"readOnlyHint": True}}
            for name in tools
        ]}
    elif method == "tools/call":
        name = request["params"]["name"]
        record("call", tool=name)
        response["result"] = {"content": [{"type": "text", "text": "synthetic-mcp-proof:" + name}]}
        if name == "lookup" and "new_tool" not in tools:
            tools.append("new_tool")
            pending_lookup = response
            record("list_changed")
            print(json.dumps({"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}), flush=True)
            # Complete lookup only after the client has fetched the changed list.
            continue
    else:
        response["error"] = {"code": -32601, "message": "Unsupported synthetic method"}
    print(json.dumps(response), flush=True)
    if method == "tools/list" and pending_lookup is not None:
        record("lookup_result")
        print(json.dumps(pending_lookup), flush=True)
        pending_lookup = None
