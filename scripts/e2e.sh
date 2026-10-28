#!/usr/bin/env bash
# End to end: the Go proxy in front of the reference's fake ComfyUI, driven
# by both SDKs. Exits non-zero on the first failure.
#
#   scripts/e2e.sh <proxy-bin> <demo-bin> <ref-python> <sdk-python> <ref-dir>
#
# Every process is tracked by PID and stopped on exit. Ports are fixed and
# chosen away from the dev defaults so a running proxy is never disturbed.
set -euo pipefail

proxy_bin=$1 demo_bin=$2 ref_python=$3 sdk_python=$4 ref_dir=$5
comfy_port=18188
proxy_port=18189
work=$(mktemp -d)
pids=()

cleanup() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

wait_port() {
  for _ in $(seq 1 75); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "nothing listening on :$1" >&2
  return 1
}

"$ref_python" "$ref_dir/demo/fake_comfyui.py" --port "$comfy_port" >"$work/fake.log" 2>&1 &
pids+=($!)
wait_port "$comfy_port"

"$proxy_bin" --comfyui "http://127.0.0.1:$comfy_port" --port "$proxy_port" --quiet >"$work/proxy.log" 2>&1 &
pids+=($!)
wait_port "$proxy_port"

base="http://127.0.0.1:$proxy_port"
echo "== Go SDK, polling"
"$demo_bin" -base "$base" -out "$work"
echo "== Go SDK, event stream"
"$demo_bin" -base "$base" -out "$work" -events
echo "== Python SDK (reference demo/run_demo.py)"
PYTHONDONTWRITEBYTECODE=1 "$sdk_python" "$ref_dir/demo/run_demo.py" --base "$base" --out "$work/py.png"
echo "✓ e2e passed"
