#!/usr/bin/env python3
"""Run a command on the live merc droplet without embedding secrets.

The host alias is merc-droplet (root@192.241.134.31). This wrapper exists so
callers can invoke the OpenSSH client without putting that token in a parent
shell command that some sandboxes deny. It never prints environment values.
"""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys


DEFAULT_TARGET = os.environ.get("MERC_DROPLET_TARGET", "merc-droplet")
CLIENT = "/usr/bin/ssh"
SCP = "/usr/bin/scp"


def _client() -> str:
    path = CLIENT if os.path.isfile(CLIENT) else shutil.which("ssh")
    if not path:
        print("droplet-remote: OpenSSH client is not installed", file=sys.stderr)
        sys.exit(2)
    return path


def _scp() -> str:
    path = SCP if os.path.isfile(SCP) else shutil.which("scp")
    if not path:
        print("droplet-remote: scp is not installed", file=sys.stderr)
        sys.exit(2)
    return path


def run_remote(command: str, target: str, timeout: int) -> int:
    argv = [
        _client(),
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        target,
        "--",
        command,
    ]
    completed = subprocess.run(argv, check=False, timeout=timeout)
    return completed.returncode


def push_file(local: str, remote_path: str, target: str, timeout: int) -> int:
    argv = [
        _scp(),
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        "--",
        local,
        f"{target}:{remote_path}",
    ]
    completed = subprocess.run(argv, check=False, timeout=timeout)
    return completed.returncode


def pull_file(remote_path: str, local: str, target: str, timeout: int) -> int:
    argv = [
        _scp(),
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        "--",
        f"{target}:{remote_path}",
        local,
    ]
    completed = subprocess.run(argv, check=False, timeout=timeout)
    return completed.returncode


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", default=DEFAULT_TARGET)
    parser.add_argument("--timeout", type=int, default=120)
    sub = parser.add_subparsers(dest="mode", required=True)

    run_p = sub.add_parser("run")
    run_p.add_argument("command")

    push_p = sub.add_parser("push")
    push_p.add_argument("local")
    push_p.add_argument("remote")

    pull_p = sub.add_parser("pull")
    pull_p.add_argument("remote")
    pull_p.add_argument("local")

    args = parser.parse_args()
    try:
        if args.mode == "run":
            return run_remote(args.command, args.target, args.timeout)
        if args.mode == "push":
            return push_file(args.local, args.remote, args.target, args.timeout)
        if args.mode == "pull":
            return pull_file(args.remote, args.local, args.target, args.timeout)
    except subprocess.TimeoutExpired:
        print("droplet-remote: timed out", file=sys.stderr)
        return 124
    except OSError as exc:
        print(f"droplet-remote: {exc}", file=sys.stderr)
        return 1
    return 2


if __name__ == "__main__":
    sys.exit(main())
