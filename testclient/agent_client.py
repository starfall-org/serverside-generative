#!/usr/bin/env python3
"""Client for separate structured coding agent sessions over paired TLS."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import stat
import sys
import tempfile

import client


def load_trusted_state(state: Path) -> tuple[Path, dict]:
    """Read only a private, owned state file within a trusted local directory.

    Agent CLI arguments may be supplied by an automated caller. Never let a
    relative traversal, symlink or shared file become reconnect credentials.
    """
    requested = state.expanduser()
    if ".." in requested.parts:
        raise ValueError("parent directory traversal is forbidden in --state")
    roots = (Path.cwd().resolve(), Path.home().resolve(), Path(tempfile.gettempdir()).resolve())
    candidate = requested if requested.is_absolute() else roots[0] / requested
    if candidate.is_symlink():
        raise ValueError("--state must not be a symlink")
    path = candidate.resolve(strict=True)
    if not any(path.is_relative_to(root) for root in roots):
        raise ValueError("--state must be inside the working directory, home or temp directory")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        details = os.fstat(fd)
        if not stat.S_ISREG(details.st_mode):
            raise ValueError("--state must refer to a regular file")
        if hasattr(os, "getuid") and details.st_uid != os.getuid():
            raise PermissionError("--state must be owned by the current user")
        if details.st_mode & 0o077:
            raise PermissionError("--state contains credentials and must be private (chmod 600)")
        if details.st_size > 65536:
            raise ValueError("--state is too large")
        with os.fdopen(fd, "r", encoding="utf-8") as handle:
            fd = -1
            data = json.load(handle)
    finally:
        if fd != -1:
            os.close(fd)
    if not isinstance(data, dict):
        raise ValueError("--state must contain a JSON object")
    return path, data


def call(state: Path, payload: dict) -> dict:
    path, auth = load_trusted_state(state)
    auth["_state_path"] = str(path)
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
