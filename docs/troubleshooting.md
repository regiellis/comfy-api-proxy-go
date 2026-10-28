---
title: Troubleshooting
section: Reference
order: 30
description: What the common startup failures and error codes mean, and what to change. Every API error carries a code; start with the one you got.
---

## The proxy will not start {#startup}

**The port is taken.** The proxy exits 1 and names the address, where the port value came from,
and how to change it. It never moves to another port, because clients are configured with
this one. Stop whatever holds the port, or set another with `--port`,
`COMFY_PROXY_PORT`, or `port` in the config file.

```sh
ss -ltnp 'sport = :8189'        # what holds the port (Linux)
comfy-api-proxy status          # is it a background proxy you started earlier?
```

**"refusing to bind non-loopback host".** Exit 2. Binding anything but loopback needs a token,
so the API is never open on a network unauthenticated. Set `token`, or set
`allow_insecure_bind` when the network itself is trusted. See
[Remote access](remote-access.md).

**An unknown key or a wrong type in the config file.** Exit 2, naming the file and the key.
`comfy-api-proxy config` prints the resolved configuration and the source of every value.

## Health says unreachable {#unreachable}

`GET /api/v2/health` reports whether the proxy can reach ComfyUI. Job calls answer
`503 upstream_unreachable` when ComfyUI is down, and also when it answers with something that is
not ComfyUI's JSON (a login page from a gateway, for example). Uploads and downloads answer
`upstream_error` instead.

- Check the URL: `comfy-api-proxy config` shows `comfyui` and where it came from.
- ComfyUI behind an authenticating gateway: set `comfyui_token`, which is sent as a bearer
  token on every call to ComfyUI.
- The proxy never uses `HTTP_PROXY` or `HTTPS_PROXY` for ComfyUI, because those calls can carry
  a partner API key. A ComfyUI only reachable through an HTTP proxy will not work.

## Error codes {#codes}

Every error is `{"error": {"code", "message", "details"}}`. The codes you are most likely to
see:

| Status | Code | Meaning and fix |
|---:|---|---|
| 401 | `unauthorized` | A token is set and the request has no valid `Authorization: Bearer` header. `GET /api/v2/health` is the only route without auth. |
| 403 | `forbidden_origin` | A browser request from another site. Add the page's origin with `--enable-cors-header` or `cors_origins`. |
| 404 | `not_found` | No such job or asset. A tampered or truncated output id also reads as not found. |
| 404 | `output_unavailable` | The output is listed but its file is gone from ComfyUI's output folder. |
| 413 | `payload_too_large` | The upload exceeds `max_upload_mb` (100 by default). |
| 422 | `workflow_format_ui` | The workflow is in the editor's save format. Export it with **Export (API)**. |
| 422 | `invalid_workflow` | ComfyUI rejected the graph. `details` carries ComfyUI's `node_errors`. |
| 422 | `missing_asset` | The workflow references an asset id the proxy does not know. |
| 422 | `idempotency_key_reuse` | That `Idempotency-Key` was already used. A reused key is refused, not replayed, so send a new key with each new job. |
| 422 | `invalid_request` | A field failed validation; the message names it. |
| 429 | `too_many_streams` | Eight event streams are already open. Close one, or poll instead. |
| 503 | `upstream_unreachable` | Job calls: ComfyUI is down or answered with something that is not ComfyUI. See [above](#unreachable). |
| 500, 502 | `upstream_error` | Uploads and downloads: ComfyUI could not be reached, refused the upload, or answered an output request with an unexpected status. |

[API reference](api.md) lists every route's codes.

## A browser client is blocked {#browser}

Browsers send `Origin` and `Sec-Fetch-Site`, and the proxy's origin guard refuses cross-site
requests unless the origin is allowlisted. Allowlist the exact origin, scheme and port
included:

```sh
comfy-api-proxy --enable-cors-header http://localhost:5173
```

CORS headers are added before auth runs, so an allowlisted page can read a `401` instead of
seeing an opaque network error.

## Large uploads fail {#uploads}

Each upload is written to a temp file while it is hashed, before it goes on to ComfyUI or the
model folder. The file goes in the system temp directory, `$TMPDIR` or `/tmp` on Linux and
macOS, so concurrent uploads need that much free space there. On a machine with a small or
RAM-backed `/tmp`, point `TMPDIR` at a disk with room before starting the proxy:

```sh
TMPDIR=/var/tmp comfy-api-proxy start
```

An upload larger than `max_upload_mb` is refused with `413 payload_too_large`. The proxy stops
reading one byte past the limit, so a never-ending upload cannot fill the disk, and the partial
temp file is removed.

## A job never finishes {#stuck}

- Check ComfyUI's own console. A job waiting on a missing model or custom node sits in
  ComfyUI's queue; the proxy reports what ComfyUI reports.
- `GET /api/v2/jobs/{id}` reads ComfyUI's history and queue on every call, so it always shows
  ComfyUI's current state, even if an event stream missed a message.
- Cancel with `POST /api/v2/jobs/{id}/cancel`. The job reads `canceling` and then `canceled`.

## State is lost after a restart {#state}

Without `--state-dir`, the proxy keeps its own records in memory. A job's status still reads
after a restart, because every read asks ComfyUI, and output ids still resolve, because they are
signed rather than stored. What a restart loses is what only the proxy knew: each job's
submitted workflow and labels, the idempotency keys it has seen, and its index of uploaded
assets. Set `state_dir` to keep them in SQLite; see
[State and background service](state-and-service.md).
