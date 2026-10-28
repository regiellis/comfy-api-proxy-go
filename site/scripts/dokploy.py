#!/usr/bin/env python3
"""Control this site's Dokploy compose service; credentials come from process env.

Required: DOKPLOY_HOST, DOKPLOY_API, and CP_DOKPLOY_COMPOSE_ID for every command
except `create`, which needs CP_DOKPLOY_ENVIRONMENT_ID instead and prints the new
compose ID to put in CP_DOKPLOY_COMPOSE_ID. The target must be named
www-comfy-api-proxy-go, or CP_DOKPLOY_SERVICE_NAME. Secret env-set arguments must be
bare variable names, never KEY=secret.

Adapted from the HyperChi site's dokploy.py (one service, no CMS).
"""

from __future__ import annotations

import contextlib
import fcntl
import functools
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import OrderedDict
from pathlib import Path
from typing import Any

COMPOSE_ID = os.environ.get("CP_DOKPLOY_COMPOSE_ID", "")
SERVICE_NAME = os.environ.get("CP_DOKPLOY_SERVICE_NAME", "www-comfy-api-proxy-go")
DOMAIN = os.environ.get("CP_DOMAIN", "")
COMPOSE_FILE = Path(__file__).resolve().parent.parent / "docker-compose.deploy.yml"
TIMEOUT = 60


# All commands on the documented release host share this advisory lock. Keep it
# on disk after unlock: unlinking permits two processes to lock different inodes.
_lock_depth = 0


@contextlib.contextmanager
def release_lock():
    global _lock_depth
    if _lock_depth:
        yield
        return
    root = COMPOSE_FILE.parent / ".release"
    root.mkdir(mode=0o700, exist_ok=True)
    identity = hashlib.sha256(COMPOSE_ID.encode()).hexdigest()
    with (root / f"{identity}.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            sys.exit("Another release command is active on this host")
        _lock_depth = 1
        try:
            yield
        finally:
            _lock_depth = 0
            fcntl.flock(lock, fcntl.LOCK_UN)


def serialized(fn):
    @functools.wraps(fn)
    def run(*args, **kwargs):
        with release_lock():
            return fn(*args, **kwargs)
    return run


def require_idle(service):
    if service.get("composeStatus") in ("running", "queued", "building"):
        sys.exit("Another deployment is active; wait for it to finish")


def public_origin():
    if not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?", DOMAIN):
        sys.exit("CP_DOMAIN must be a hostname without a scheme, path, or credentials")
    return f"https://{DOMAIN}"


def verify_site(tag: str, timeout: float = 120) -> None:
    """Check the public TLS route and release identity, without API credentials."""
    origin = public_origin()
    opener = urllib.request.build_opener(NoRedirect())
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        healthy = True
        for path in ("/healthz", "/"):
            req = urllib.request.Request(origin + path + "?release=" + urllib.parse.quote(tag),
                                         headers={"Cache-Control": "no-cache"})
            try:
                with opener.open(req, timeout=max(0.1, min(10, deadline-time.monotonic()))) as response:
                    body = response.read(1_000_001)
                    valid_body = body == b"ok\n" if path == "/healthz" else (
                        b"<title>comfy-api-proxy-go" in body and b'id="main"' in body and len(body) <= 1_000_000)
                    if response.status != 200 or response.headers.get("X-Release") != tag or not valid_body:
                        healthy = False
            except (urllib.error.URLError, OSError, TimeoutError):
                healthy = False
            if not healthy:
                break
        if healthy:
            print(f"Verified public homepage and health for release {tag}")
            return
        time.sleep(min(2, max(0, deadline-time.monotonic())))
    sys.exit("Deployment completed but public health/release verification failed; inspect before retrying")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _config(need_compose: bool = True) -> tuple[str, str]:
    host = os.environ.get("DOKPLOY_HOST", "").rstrip("/")
    key = os.environ.get("DOKPLOY_API", "")
    if not host or not key:
        sys.exit(
            "DOKPLOY_HOST and DOKPLOY_API must both be set.\n"
            "DOKPLOY_HOST is the base URL including /api, e.g. https://host/api"
        )
    if not host.startswith(("http://", "https://")):
        host = f"https://{host}"
    parsed = urllib.parse.urlsplit(host)
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        sys.exit("DOKPLOY_HOST must not contain credentials, query strings, or fragments")
    if parsed.scheme != "https" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
        sys.exit("DOKPLOY_HOST must use HTTPS outside loopback")
    if need_compose and not re.fullmatch(r"[A-Za-z0-9_-]+", COMPOSE_ID):
        sys.exit("Set CP_DOKPLOY_COMPOSE_ID to this site's compose service ID")
    return host, key


def _call(route: str, payload: dict[str, Any] | None = None, need_compose: bool = True) -> Any:
    """POST when given a payload, GET otherwise. Exits on a non-2xx reply."""
    host, key = _config(need_compose)
    url = f"{host}/{route.lstrip('/')}"
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
    req.add_header("x-api-key", key)
    if data:
        req.add_header("content-type", "application/json")
    try:
        with urllib.request.build_opener(NoRedirect()).open(req, timeout=TIMEOUT) as resp:
            body = resp.read().decode()
    except urllib.error.HTTPError as err:
        sys.exit(f"Dokploy request failed: HTTP {err.code} (response body omitted)")
    except urllib.error.URLError:
        sys.exit("Dokploy is unreachable (check host and connectivity)")
    return json.loads(body) if body.strip() else None


def _service() -> dict[str, Any]:
    service = _call(f"compose.one?composeId={COMPOSE_ID}")
    if not isinstance(service, dict) or service.get("name") != SERVICE_NAME:
        sys.exit("Dokploy target name does not match this site; refusing access")
    return service


def _manifest() -> str:
    if not COMPOSE_FILE.is_file():
        sys.exit(f"missing {COMPOSE_FILE}")
    wanted = COMPOSE_FILE.read_text()
    # A real `build:` key, not the word inside this file's own header comment.
    for number, line in enumerate(wanted.splitlines(), start=1):
        bare = line.split("#", 1)[0].strip()
        if bare in ("build:", "build:{}") or bare.startswith("build: "):
            sys.exit(
                f"{COMPOSE_FILE.name}:{number} declares a build: key. That file must "
                "only pull, or a failed pull silently builds on the production host."
            )
    return wanted


def cmd_create() -> None:
    """Create this site's compose service and its domain, once.

    Needs CP_DOKPLOY_ENVIRONMENT_ID (the comfy-api-proxy-go project's production
    environment). Refuses if a service of this name already exists there.
    Prints the compose ID; export it as CP_DOKPLOY_COMPOSE_ID afterwards.
    """
    public_origin()
    environment = os.environ.get("CP_DOKPLOY_ENVIRONMENT_ID", "")
    if not re.fullmatch(r"[A-Za-z0-9_-]+", environment):
        sys.exit("Set CP_DOKPLOY_ENVIRONMENT_ID to the target environment's ID")
    for project in _call("project.all", need_compose=False) or []:
        for env in project.get("environments") or []:
            if env.get("environmentId") != environment:
                continue
            names = [c.get("name") for c in env.get("compose") or []]
            if SERVICE_NAME in names:
                sys.exit(f"{SERVICE_NAME} already exists in {project.get('name')}/{env.get('name')}")
    created = _call(
        "compose.create",
        {
            "name": SERVICE_NAME,
            "appName": SERVICE_NAME,
            "environmentId": environment,
            "composeType": "docker-compose",
            "sourceType": "raw",
            "composeFile": _manifest(),
            "description": "comfy-api-proxy-go documentation site, pull-only image",
        },
        need_compose=False,
    )
    compose_id = (created or {}).get("composeId") if isinstance(created, dict) else None
    if not compose_id:
        sys.exit("Dokploy did not return a composeId; inspect the project in the UI")
    _call(
        "domain.create",
        {
            "host": DOMAIN,
            "port": 8080,
            "https": True,
            "certificateType": "letsencrypt",
            "composeId": compose_id,
            "serviceName": "web",
            "domainType": "compose",
        },
        need_compose=False,
    )
    print(f"created {SERVICE_NAME} with domain https://{DOMAIN} -> web:8080")
    print(f"export CP_DOKPLOY_COMPOSE_ID={compose_id}")


@serialized
def cmd_sync() -> None:
    """Copy the committed production compose file into Dokploy's raw field."""
    wanted = _manifest()
    current = _service()
    require_idle(current)
    if current.get("composeFile") == wanted and current.get("sourceType") == "raw":
        print("compose file already in sync")
        return
    _call("compose.update", {"composeId": COMPOSE_ID, "sourceType": "raw", "composeFile": wanted})
    print(f"pushed {COMPOSE_FILE.name} ({len(wanted)} bytes) to Dokploy")


def cmd_env() -> None:
    """Print the keys Dokploy holds, masking every value."""
    raw = _service().get("env") or ""
    if not raw.strip():
        print("no environment variables set on the service")
        return
    for key, value in _parse_env(raw).items():
        print(f"  {key}={'<set>' if value else '<empty>'}")


def _parse_env(raw: str) -> "OrderedDict[str, str]":
    # Preserve quoted multiline values verbatim when changing unrelated keys.
    # Reject malformed input before any write; never print its secret contents.
    pairs: OrderedDict[str, str] = OrderedDict()
    lines = iter((raw or "").splitlines())
    quoted = re.compile(r"(?s)(?:'(?:\\.|[^'])*'|\"(?:\\.|[^\"])*\")(?:[ \t]+#[^\n]*)?[ \t]*")
    for line in lines:
        line = line.lstrip()
        if not line or line.startswith("#"):
            continue
        match = re.fullmatch(r"(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=(.*)", line)
        if not match:
            sys.exit("Cannot safely parse existing service environment; no changes made")
        key, value = match.groups()
        value = value.lstrip()
        if value.startswith(("'", '\"')):
            while not quoted.fullmatch(value):
                try:
                    value += "\n" + next(lines)
                except StopIteration:
                    sys.exit("Invalid quoted service environment; no changes made")
        if key in pairs:
            sys.exit("Duplicate service environment key; no changes made")
        pairs[key] = value
    return pairs


@serialized
def cmd_env_set(*args: str) -> None:
    """Merge keys into the service environment, leaving every other key alone."""
    if not args:
        sys.exit(
            "usage: dokploy.py env-set KEY=VALUE | KEY ...\n"
            "  KEY=VALUE  set a literal\n"
            "  KEY        take the value from this process's environment"
        )
    service = _service()
    require_idle(service)
    pairs = _parse_env(service.get("env") or "")
    changed: list[str] = []
    for arg in args:
        key, sep, value = arg.partition("=")
        key = key.strip()
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            sys.exit("Invalid environment key")
        if sep and any(part in key.upper() for part in ("TOKEN", "SECRET", "CREDENTIAL", "PASSWORD", "API_KEY")):
            sys.exit("Pass credential keys without a value; values are read from process env")
        if not sep:
            if key not in os.environ:
                sys.exit(f"{key} is not set in this environment, and no value was given")
            value = os.environ[key]
        if "\n" in value or "\r" in value:
            sys.exit("Multiline environment values must be encoded first")
        # Quote new values: Compose must not expand dollar signs in credentials.
        pairs[key] = "'" + value.replace("'", "\\'") + "'"
        changed.append(key)
    blob = "\n".join(f"{k}={v}" for k, v in pairs.items())
    _call("compose.update", {"composeId": COMPOSE_ID, "env": blob})
    print(f"set {', '.join(changed)} ({len(pairs)} keys on the service)")


@serialized
def cmd_deploy(expected_tag: str | None = None) -> None:
    service = _service()
    require_idle(service)
    tag = _parse_env(service.get("env") or "").get("TAG", "").strip("'\"")
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag):
        sys.exit("Service TAG must name an existing release")
    if expected_tag is not None and tag != expected_tag:
        sys.exit("Service TAG changed during release; refusing deployment")
    public_origin()
    route = f"deployment.allByCompose?composeId={COMPOSE_ID}"
    previous = {item["deploymentId"] for item in _call(route)}
    _call("compose.deploy", {"composeId": COMPOSE_ID})
    print("Deploy queued; waiting for its deployment record", flush=True)
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        records = [item for item in _call(route) if item["deploymentId"] not in previous]
        if len(records) > 1:
            sys.exit("Multiple deployments appeared; inspect Dokploy to confirm the intended release")
        if records:
            status = records[0].get("status")
            if status == "error":
                sys.exit("Dokploy deployment failed; inspect deployment logs")
            if status == "done":
                verify_site(tag)
                print("Dokploy deployment completed and verified")
                return
        time.sleep(2)
    sys.exit("Could not confirm this deployment completed; inspect Dokploy before retrying")


def cmd_status() -> None:
    svc = _service()
    print(f"  name       {svc.get('name')}")
    print(f"  status     {svc.get('composeStatus')}")
    print(f"  source     {svc.get('sourceType')}")
    print(f"  appName    {svc.get('appName')}")
    domains = _call(f"domain.byComposeId?composeId={COMPOSE_ID}") or []
    for domain in domains:
        scheme = "https" if domain.get("https") else "http"
        print(f"  domain     {scheme}://{domain.get('host')} -> {domain.get('serviceName')}:{domain.get('port')}")
    if not domains:
        print("  domain     (none set)")


COMMANDS = {
    "create": cmd_create,
    "sync": cmd_sync,
    "env": cmd_env,
    "env-set": cmd_env_set,
    "deploy": cmd_deploy,
    "status": cmd_status,
}


def main() -> None:
    if len(sys.argv) < 2 or sys.argv[1] not in COMMANDS:
        sys.exit(f"usage: {Path(sys.argv[0]).name} {{{'|'.join(COMMANDS)}}}")
    name = sys.argv[1]
    handler = COMMANDS[name]
    if name == "env-set":
        handler(*sys.argv[2:])
    else:
        handler()


if __name__ == "__main__":
    main()
