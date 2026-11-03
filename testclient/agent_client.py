#!/usr/bin/env python3
"""Client for separate structured coding agent sessions over paired TLS."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

import client


def call(state: Path, payload: dict) -> dict:
    auth = json.loads(state.read_text(encoding="utf-8"))
    auth["_state_path"] = str(state)
    with client.reconnect(auth) as sock:
        client.write_frame(sock, payload)
        reply = client.read_frame(sock)
        if reply.get("type") == "error":
            raise RuntimeError(reply.get("message", "agent request failed"))
        return reply


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("open", "send", "steer", "events", "respond", "interrupt", "status", "list", "close"))
    parser.add_argument("--state", type=Path, default=Path("client-state.json"))
    parser.add_argument("--type", choices=("claude", "codex", "opencode"))
    parser.add_argument("--binary", default="", help="Optional fork name in PATH or absolute executable path")
    parser.add_argument("--cwd", default="")
    parser.add_argument("--agent-session-id")
    parser.add_argument("--prompt", default="")
    parser.add_argument("--approval-id")
    parser.add_argument("--decision", choices=("accept", "decline", "cancel", "acceptForSession", "once", "always", "reject"))
    parser.add_argument("--since", type=int, default=0)
    parser.add_argument("--wait-ms", type=int, default=200)
    args = parser.parse_args()

    req = {"type": "agent_" + args.action}
    if args.action == "open":
        if args.type is None:
            parser.error("--type is required for open")
        req["agent"] = {"type": args.type}
        if args.binary:
            req["agent"]["binary"] = args.binary
        if args.cwd:
            req["agent"]["cwd"] = args.cwd
    if args.action not in ("open", "list"):
        if not args.agent_session_id:
            parser.error("--agent-session-id is required")
        req["agent_session_id"] = args.agent_session_id
    if args.action in ("send", "steer"):
        if not args.prompt:
            parser.error("--prompt is required")
        req["prompt"] = args.prompt
    if args.action == "respond":
        if not args.approval_id or not args.decision:
            parser.error("--approval-id and --decision are required")
        req["approval_id"] = args.approval_id
        req["decision"] = args.decision
    if args.action == "events":
        req.update(since=args.since, wait_ms=args.wait_ms)
    print(json.dumps(call(args.state, req), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError, KeyError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        sys.exit(1)
