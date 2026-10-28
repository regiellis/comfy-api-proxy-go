---
title: Differences from the reference
section: Reference
order: 10
next: benchmarks
description: Every way this binary behaves differently from Comfy-Org's Python comfy-api-proxy, with the reason for each, and the parts that match it on purpose.
---

## The reference {#reference}

This binary is a port of [`comfy-api-proxy`](https://github.com/Comfy-Org/comfy-api-proxy), Comfy-Org's Python and aiohttp proxy. The Python proxy is the behavioral spec: where this page lists no difference, the two behave the same, and the reference's README and docs describe this binary too.

## Fixes for bugs the reference shares {#fixes}

These three were found by running against a real ComfyUI with custom nodes, which behaves differently from the reference's fake one. Each is pinned by a test.

| Change | Why |
|---|---|
| A job missing from `/queue` is looked up in `/history` once more before it is reported. | Status reads `/history`, then `/queue`. A job that finished between the two reads was in neither and read as `expired`, a terminal state, so fast jobs looked lost under tight polling. ComfyUI writes history and clears the running slot under one lock, so the second read settles it. |
| The event stream takes a second snapshot right after its WebSocket connects, and reconciles on a fixed 15-second schedule that incoming frames never reset. | A job that ended between the first snapshot and the connect sent its terminal message to nobody, and ComfyUI's broadcast frames (its own `status`, custom nodes such as crystools' monitor) kept resetting the reconcile timer, so the stream could hang forever. See [Event stream](events.md#reconcile). |
| Outputs are always sent before the terminal `status` event. | Leftover outputs used to follow the terminal status, and clients, both SDKs included, stop reading there, so those outputs were never seen. |

## Other differences {#other}

| Change | Why |
|---|---|
| Uploads are spooled to a temp file while hashed, then streamed to ComfyUI or into the model directory. | The reference reads the whole upload into memory. Spooling keeps memory flat as uploads grow. |
| `assets.expires_at` is stored in the `--state-dir` database, and a state database written by the Python proxy gains the column when opened. | The reference never persisted it, so expiry was lost on restart. |
| Expired assets are cleaned up with or without `--state-dir`. | The reference skipped cleanup when a state directory was set. |
| `public_url` sets the origin of every link in a response. | Behind a TLS-terminating reverse proxy the reference emits `http://` links, and clients drop the token on them ([Comfy-Org/comfy-api-proxy#31](https://github.com/Comfy-Org/comfy-api-proxy/issues/31)). |
| Outputs report their real `size_bytes`, read from ComfyUI's `/view` with one `HEAD` per output and remembered, on the job and on `GET /assets/{id}` alike. | The reference reports `0` for every output ([Comfy-Org/comfy-api-proxy#26](https://github.com/Comfy-Org/comfy-api-proxy/issues/26)). |
| Workflows are decoded with exact numbers, but re-encoded with object keys in sorted order. | A 64-bit seed survives intact. The graph is the same, and ComfyUI does not depend on key order. |
| Configuration layers flag over environment over config file over default. Bind scopes (`local`, `global`), the `config` command, the access log, and the health line in `status` are new. | The reference CLI takes flags only. See [Configuration](configuration.md) and [CLI](cli.md). |
| `comfyui_token` is sent as a bearer token on every request to ComfyUI, the WebSocket handshake included, and never to clients. | It lets the proxy front a ComfyUI behind an authenticating gateway. |
| A reply from ComfyUI that is not JSON, or is invalid JSON, where JSON was expected, is `503 upstream_unreachable`. | The reference raised a 500. The server on the other end is not behaving as ComfyUI, which a client should treat like an outage. |
| Shutdown ends open event streams at once. | Streams never go idle, so a graceful shutdown would otherwise wait on them until its timeout. See [Event stream](events.md#shutdown). |
| `expires_in` past the year 9999 is `422 invalid_request`. | The reference raised a 500 on the out-of-range date. |
| `file_path` or `content_type` containing a control character is `422 invalid_request`. | Those values go into the multipart headers of the upload sent to ComfyUI. |
| An upload may carry at most 64 multipart parts, else `422 invalid_request`. | A body of many tiny fields cannot pile up in memory. |
| On Linux, `start`, `stop`, and `status` identify the background process by its executable (`/proc/<pid>/exe`), not by a substring of its command line. Elsewhere they compare the command line's first word and the `run` subcommand. | A substring match can pick up an unrelated process whose command line happens to contain the name. See [State and service](state-and-service.md). |
| When the state directory cannot be restricted to 0700, or its database to 0600, the proxy prints a warning and keeps running. The reference ignores the failure silently. | The database holds the secret that signs output ids, so the owner should know when other local users may be able to read it. A filesystem without POSIX modes still must not stop the proxy. |

## What matches on purpose {#matches}

| Area | What holds |
|---|---|
| Routes | The same fourteen routes, including the proxy-local extensions. See [API reference](api.md#routes). |
| Wire shape | Status codes, error `code` strings, field presence, and `null` versus absent match, because SDKs and the conformance suite key on them. Errors always use the `{"error": {code, message, details}}` envelope. |
| State model | `GET /jobs/{id}` reads ComfyUI on every call. The proxy keeps only what ComfyUI cannot tell it: job records, `Idempotency-Key` claims, the asset index, and the signing secret. |
| Bounds | The same constants: 24-hour retention, 255-character idempotency keys with 10,000 held, 1024-byte metadata, priority within plus or minus 1,000,000, list limit 100 with a 500-record scan, 8 concurrent streams. See [Bounds](api.md#bounds). |
| Output asset ids | The same format, minted byte for byte as Python's `json.dumps` writes the payload. A `--state-dir` moved between the two proxies keeps every id valid. See [Output asset ids](assets.md#output-ids). |
| State directory | A state directory written by the Python proxy opens here. |
| Security defaults | Loopback bind by default, a token or explicit opt-out for a non-loopback bind, the origin guard ported from ComfyUI core with its string-comparison quirks (each fails closed), safetensors-only model placement, allowlisted roots, symlink-resolved traversal checks, and `O_EXCL` no-clobber writes. |
| Model file mode | Placed model files are 0644, so a ComfyUI running as another user can load them. |
| Environment proxies | Calls to ComfyUI never go through an `HTTP_PROXY` from the environment, because they can carry the partner API key. |
| CLI flags | The Go CLI accepts every flag the reference's test harness passes. |

## Conformance {#conformance}

The reference ships a black-box HTTP test suite that starts a fake ComfyUI and a proxy as subprocesses and drives them over plain HTTP. All 82 of those tests pass against this binary. `task conformance` runs the suite unmodified, through a small pytest plugin that swaps the spawned Python proxy for `bin/comfy-api-proxy`. The first run fetches the reference at the commit it was last passed against:

```sh
task conformance
```

New upstream tests arrive with each update of the vendored reference, so the count can grow. The suite runs against a fake ComfyUI, which is quieter and slower to finish jobs than a real one. The three fixes above were found against a real ComfyUI and are covered by this repository's own tests, not by the reference suite.

## Tested against a real ComfyUI {#real-comfyui}

Beyond the conformance suite, the binary has run against ComfyUI 0.41.2 with custom nodes, on an
RTX 4000 Ada:

- `EmptyImage` jobs, polled and streamed, with full and ranged downloads.
- An uploaded input resolved through `core/ASSET` into `LoadImage` and `ImageInvert`, and the
  same bytes deduplicated on a second upload.
- A workflow ComfyUI rejects, with its `node_errors` passed back.
- An 8-step Z-Image Turbo generation streamed live: 8 progress events, 4 previews, the output,
  then `succeeded`, with a 64-bit seed reaching ComfyUI exactly.
- A 40-step generation cancelled at step 4 (`canceling`, then `canceled`).
- A partner (API) node, `Flux2ProImageNode`, authenticated by the Comfy API key in
  `extra_data.api_key_comfy_org`, with the key found in neither the proxy's stored workflow nor
  ComfyUI's history.
- The Python SDK driving the same proxy.
