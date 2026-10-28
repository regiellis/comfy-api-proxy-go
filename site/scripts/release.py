#!/usr/bin/env python3
"""Build locally, push over the VPS SSH relay, and deploy this site's image.

Adapted from the HyperChi site's release script: one image (web), no CMS.
Run from site/. The image builds from the repository root (site/Dockerfile).
Environment: CP_RELEASE_VPS (user@host), optional CP_RELEASE_TAG (defaults to the
commit's short SHA), optional CP_RELEASE_RELAY_PORT (default 19500), plus what
dokploy.py needs (DOKPLOY_HOST, DOKPLOY_API, CP_DOKPLOY_COMPOSE_ID).
"""
from __future__ import annotations

import argparse
import contextlib
import os
import re
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

import dokploy

ROOT = Path(__file__).resolve().parent.parent
NAMESPACE = "comfy-proxy"
IMAGE = "comfy-proxy-site"


def run(*args: str, capture: bool = False) -> str:
    result = subprocess.run(args, cwd=ROOT, check=True, text=True,
                            stdout=subprocess.PIPE if capture else None)
    return result.stdout.strip() if capture else ""


def settings() -> tuple[str, str, str]:
    tag = os.environ.get("CP_RELEASE_TAG") or run("git", "rev-parse", "--short=12", "HEAD", capture=True)
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag):
        sys.exit("Invalid CP_RELEASE_TAG")
    vps = os.environ.get("CP_RELEASE_VPS", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+@[A-Za-z0-9.-]+", vps):
        sys.exit("Set CP_RELEASE_VPS to user@host for the Dokploy VPS")
    port = os.environ.get("CP_RELEASE_RELAY_PORT", "19500")
    if not port.isdecimal() or not 1024 <= int(port) <= 65535:
        sys.exit("CP_RELEASE_RELAY_PORT must be 1024–65535")
    return tag, vps, port


def image(port: str, tag: str) -> str:
    return f"localhost:{port}/{NAMESPACE}/{IMAGE}:{tag}"


def build(tag: str, port: str) -> None:
    # A clean source tree makes the release tag identify exactly what was built.
    if run("git", "status", "--porcelain", capture=True):
        sys.exit("Commit the current work before building a release")
    run("task", "site:test")
    run("task", "site:lint")
    run("task", "security")
    run("task", "site:sync")
    run("docker", "build", "--pull", "--build-arg", f"RELEASE={tag}", "-t", image(port, tag), "-f", "Dockerfile", "..")


@contextlib.contextmanager
def relay(vps: str, port: str):
    scratch = ROOT / ".release"
    scratch.mkdir(mode=0o700, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="relay-", dir=scratch) as temporary:
        socket = str(Path(temporary) / "ssh.sock")
        run("ssh", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
            "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15",
            "-fN", "-M", "-S", socket, "-L", f"127.0.0.1:{port}:127.0.0.1:5000", vps)
        try:
            # The registry already exists on the VPS; this workflow never recreates it.
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/v2/", timeout=10) as response:
                if response.status != 200:
                    sys.exit("The VPS relay registry did not answer successfully")
            yield
        finally:
            subprocess.run(["ssh", "-S", socket, "-O", "exit", vps],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def deploy(tag: str) -> None:
    with dokploy.release_lock():
        dokploy.require_idle(dokploy._service())
        dokploy.public_origin()
        dokploy.cmd_sync()
        dokploy.cmd_env_set(f"REGISTRY=localhost:5000/{NAMESPACE}", f"TAG={tag}", f"PUBLIC_ORIGIN={dokploy.public_origin()}")
        dokploy.cmd_deploy(expected_tag=tag)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "build", "stack", "deploy"))
    command = parser.parse_args().command
    if command == "deploy":
        tag = os.environ.get("CP_RELEASE_TAG", "")
        if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag):
            sys.exit("Set CP_RELEASE_TAG to an existing pushed release")
        dokploy._service()
        deploy(tag)
        return
    tag, vps, port = settings()
    if command == "plan":
        print(f"Target: {dokploy.SERVICE_NAME}; compose ID configured: {bool(dokploy.COMPOSE_ID)}")
        print(f"Release: {tag}; image: localhost:5000/{NAMESPACE}/{IMAGE}:{tag}")
        print(f"Transport: SSH to {vps}; local relay port: {port}; no changes made")
        return
    # Validate the service identity before building or pushing anything.
    dokploy._service()
    if command in ("build", "stack"):
        build(tag, port)
    if command == "stack":
        with relay(vps, port):
            run("docker", "push", image(port, tag))
        deploy(tag)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError:
        sys.exit("Release command failed; deployment was not confirmed")
    except (OSError, ValueError):
        sys.exit("Release could not complete; check configuration and connectivity")
