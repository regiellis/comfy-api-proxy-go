"""Run the reference suite's black-box HTTP tests against the Go binary.

Loaded with ``pytest -p conformance_plugin`` from inside the reference checkout
(see ``task conformance``). The reference tests run unmodified: the plugin
swaps the ``subprocess`` name inside the reference ``conftest`` for a shim that
rewrites ``python -m comfy_api_proxy.cli <flags>`` into ``$COMFY_PROXY_BIN
<flags>``. Every other spawn, including the fake ComfyUI, passes through.

Only tests whose fixture closure includes a stack fixture are kept. Those talk
to a spawned proxy over HTTP and nothing else. The rest import the Python
package's internals and have no meaning against a Go binary.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from collections import Counter
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

# Matched by prefix: some stack fixtures (stack_with_cors_and_token) call the
# conftest's _make_stack directly instead of going through make_stack, so
# they never show up in a closure by any other name.
HTTP_FIXTURE_PREFIX = "stack"
HTTP_FIXTURES = {"make_stack", "fake_comfyui"}


def _is_http_test(item: pytest.Item) -> bool:
    names = set(getattr(item, "fixturenames", ()))
    return bool(HTTP_FIXTURES & names) or any(
        n.startswith(HTTP_FIXTURE_PREFIX) for n in names
    )


_PROXY_MODULE = ["-m", "comfy_api_proxy.cli"]


def _binary() -> str:
    path = os.environ.get("COMFY_PROXY_BIN", "")
    if not path or not Path(path).is_file():
        raise pytest.UsageError(
            f"COMFY_PROXY_BIN must point at the built Go binary, got {path!r}"
        )
    return path


class _SubprocessShim:
    """Stands in for the ``subprocess`` module inside the reference conftest."""

    def __init__(self, binary: str) -> None:
        self._binary = binary
        self.spawned: list[subprocess.Popen[bytes]] = []

    def __getattr__(self, name: str) -> Any:
        return getattr(subprocess, name)

    def Popen(self, args: list[str], **kwargs: Any) -> Any:  # noqa: N802
        if list(args[1:3]) == _PROXY_MODULE:
            args = [self._binary, *args[3:]]
        proc = subprocess.Popen(args, **kwargs)
        self.spawned.append(proc)
        return proc

    def reap(self) -> None:
        """Stop anything a test left running. The reference conftest fails
        out of _make_stack before returning its cleanup when the proxy never
        starts, which orphans the fake ComfyUI it already launched."""
        for proc in self.spawned:
            if proc.poll() is None:
                proc.terminate()
        for proc in self.spawned:
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)


def pytest_configure(config: pytest.Config) -> None:
    config._conformance_binary = _binary()  # type: ignore[attr-defined]
    # The spawned binary must see only the flags the test passes: no real
    # config file and no COMFY_PROXY_* settings from the caller's shell.
    os.environ["XDG_CONFIG_HOME"] = tempfile.mkdtemp(prefix="conformance-xdg-")
    for key in [
        k for k in os.environ if k.startswith("COMFY_PROXY_") and k != "COMFY_PROXY_BIN"
    ]:
        del os.environ[key]


def pytest_collection_modifyitems(
    config: pytest.Config, items: list[pytest.Item]
) -> None:
    keep, drop = [], []
    for item in items:
        (keep if _is_http_test(item) else drop).append(item)
    if drop:
        config.hook.pytest_deselected(items=drop)
    items[:] = keep


def _reference_conftest() -> Any:
    for module in list(sys.modules.values()):
        path = getattr(module, "__file__", None)
        if path and Path(path).name == "conftest.py" and hasattr(module, "_make_stack"):
            return module
    raise RuntimeError("reference conftest with _make_stack is not loaded")


@pytest.fixture(autouse=True)
def _spawn_go_binary(
    request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch
) -> Iterator[None]:
    shim = _SubprocessShim(request.config._conformance_binary)  # type: ignore[attr-defined]
    monkeypatch.setattr(_reference_conftest(), "subprocess", shim)
    yield
    shim.reap()


def pytest_terminal_summary(terminalreporter: Any) -> None:
    """Per-file pass counts, the porting scoreboard."""
    passed: Counter[str] = Counter()
    total: Counter[str] = Counter()
    for status in ("passed", "failed", "error", "skipped", "xfailed", "xpassed"):
        for report in terminalreporter.stats.get(status, []):
            if getattr(report, "when", "call") not in ("call", "setup"):
                continue
            if status == "error" or report.when == "call" or report.skipped:
                name = report.nodeid.split("::", 1)[0]
                total[name] += 1
                passed[name] += status == "passed"
    if not total:
        return
    terminalreporter.section("conformance scoreboard")
    width = max(len(n) for n in total)
    for name in sorted(total):
        terminalreporter.write_line(
            f"{name:<{width}}  {passed[name]:>3}/{total[name]:<3}"
        )
    terminalreporter.write_line(
        f"{'total':<{width}}  {sum(passed.values()):>3}/{sum(total.values()):<3}"
    )
