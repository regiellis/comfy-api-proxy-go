---
title: API reference
section: API
order: 10
next: events
description: Every route the proxy serves under /api/v2, with request fields, response shapes, status codes, and error codes. Also covers the error envelope, Idempotency-Key, and the fixed bounds.
---

## Routes {#routes}

The proxy serves the Comfy API v2 under `/api/v2`. These are all the routes it registers. A `GET` route also answers `HEAD`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v2/health` | Process probe. Never calls ComfyUI and never needs a token. |
| `POST` | `/api/v2/jobs` | Submit an API-format workflow. |
| `GET` | `/api/v2/jobs` | List jobs this proxy submitted, newest first. |
| `GET` | `/api/v2/jobs/{id}` | The job's current state, read from ComfyUI on every call. |
| `GET` | `/api/v2/jobs/{id}/workflow` | The resolved graph the job ran with. |
| `POST` | `/api/v2/jobs/{id}/cancel` | Request cancellation. |
| `GET` | `/api/v2/jobs/{id}/events` | Live Server-Sent Events stream. See [Event stream](events.md). |
| `POST` | `/api/v2/assets` | Multipart upload. See [Assets](assets.md). |
| `POST` | `/api/v2/assets/from-hash` | Look up an asset by content hash. |
| `POST` | `/api/v2/assets/from-path` | Register a file already under `--comfyui-base-dir`. |
| `HEAD` | `/api/v2/assets/by-hash/{hash}` | Check whether a hash is known. |
| `GET` | `/api/v2/assets/{id}` | Asset metadata. |
| `GET` | `/api/v2/assets/{id}/content` | Asset bytes, with `Range` support. |
| `DELETE` | `/api/v2/assets/{id}` | Remove an asset from the index. |

There are no batch endpoints. One proxy fronts one ComfyUI, and a batch is a series of submits from the client. `GET /api/v2/jobs`, `GET /api/v2/jobs/{id}/workflow`, `POST /api/v2/assets/from-path`, `DELETE /api/v2/assets/{id}`, and `GET /api/v2/health` are proxy-local extensions that the vendored Comfy Cloud contract does not list.

Assets, uploads, and downloads are covered on [Assets](assets.md). This page covers jobs, health, and the conventions every route shares.

## Authentication and origin checks {#auth}

When a token is configured, every `/api/v2/*` request except `GET /api/v2/health` and `OPTIONS` preflights must carry it:

```http
Authorization: Bearer <token>
```

A missing, malformed, or wrong token is `401 unauthorized`. The token is compared in constant time.

An origin guard runs in front of auth on every request. A request with `Sec-Fetch-Site: cross-site` is `403 forbidden_origin`, and so is a request whose `Origin` host does not match a loopback `Host`. Requests with no `Origin` header (curl, SDKs) pass. Origins allowlisted with `cors_origins` skip the guard and receive CORS headers. See [Remote access](remote-access.md) and [Configuration](configuration.md).

## Error envelope {#errors}

Every error the proxy writes has the same shape. `details` is `null` unless the error carries structured data.

```json
{
  "error": {
    "code": "missing_asset",
    "message": "Unresolvable core/ASSET reference(s): nope",
    "details": {"missing_ids": ["nope"]}
  }
}
```

Match on `code`, not on `message`. The codes are the reference's exact strings.

| Code | Status | Raised by |
|---|---|---|
| `invalid_request` | 400 or 422 | Malformed body, bad query parameter, bad field value. |
| `invalid_workflow` | 422 | Missing `workflow`, reserved fields present, or ComfyUI rejected the graph. |
| `workflow_format_ui` | 422 | The workflow is a UI export, not API format. |
| `missing_asset` | 422 | A `core/ASSET` reference did not resolve. |
| `idempotency_key_reuse` | 422 | The `Idempotency-Key` was already used. |
| `not_found` | 404 | Unknown job or asset id, or no stored workflow. |
| `blob_not_found` | 404 | `from-hash` or `from-path` found nothing. |
| `output_unavailable` | 404 | The asset is known but its bytes are gone. |
| `hash_mismatch` | 409 | `expected_hash` did not match the upload, or a model file appeared at the destination mid-write. |
| `payload_too_large` | 413 | Upload or registered file over `max_upload_mb`. |
| `too_many_streams` | 429 | The event stream cap is reached. Sent with `Retry-After: 5`. |
| `unauthorized` | 401 | Missing or wrong bearer token. |
| `forbidden_origin` | 403 | The origin guard refused the request. |
| `method_not_allowed` | 405 | Wrong method on an `assets/by-hash/...` or `assets/{id}/content` path. |
| `upstream_unreachable` | 503 | ComfyUI could not be reached, or answered with invalid JSON. |
| `upstream_error` | 500 or 502 | ComfyUI failed an upload or a content fetch. |
| `internal_error` | 500 | The proxy's own state failed, such as a `--state-dir` write. The cause is logged, never returned. |

## Bounds {#bounds}

These are fixed in the binary and match the reference.

| Bound | Value |
|---|---|
| Job and output URL lifetime (`expires_at`, `url_expires_at`) | 24 hours from creation or from the read |
| `Idempotency-Key` length | 1 to 255 characters |
| `Idempotency-Key` claims held | 10,000, oldest evicted first |
| `metadata` | a string of at most 1024 UTF-8 bytes |
| `priority` | an integer from -1,000,000 to 1,000,000 |
| `GET /jobs` `limit` | 1 to 100, default 50 |
| `GET /jobs` scan | at most 500 job records per request |
| Job records reloaded from `--state-dir` at startup | the newest 500 |
| Concurrent event streams | 8 |
| Upload size | `max_upload_mb`, default 100 MB |
| `POST /jobs` body | `max_upload_mb` plus 1 MiB |
| `from-hash` and `from-path` body | 1 MiB |
| Multipart parts per upload | 64 |

## Health {#health}

`GET /api/v2/health` answers without calling ComfyUI and without a token, so a supervisor can probe it. `upstream` is the configured ComfyUI URL, not a reachability check.

```sh
curl -s http://127.0.0.1:8189/api/v2/health
```

```json
{"status": "healthy", "upstream": "http://127.0.0.1:8188"}
```

## Submit a job {#submit}

`POST /api/v2/jobs` takes a JSON body and returns `201` with the job object.

| Field | Type | Notes |
|---|---|---|
| `workflow` | object | Required. An API-format graph, as ComfyUI's "Export (API)" writes it. |
| `extra_data` | object | Optional. The only accepted key is `api_key_comfy_org`, a string, which authenticates partner (API) nodes. Forwarded to ComfyUI with the prompt and never stored or logged. |
| `metadata` | string | Optional opaque label, at most 1024 bytes. Stored by the proxy and echoed on reads. Not sent to ComfyUI. |
| `priority` | integer | Optional and advisory. Stored and echoed, never passed to ComfyUI's queue. |
| `webhook_url`, `inputs` | | Reserved. Present and non-null is a `422 invalid_workflow`. |

```sh
curl -s http://127.0.0.1:8189/api/v2/jobs \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 7d1f6c2e-5b0a-4e8f-9a51-3c2b8e7d4f10' \
  -d '{"workflow": {...}, "metadata": "demo", "priority": 3}'
```

```json
{
  "id": "813f058f-80fc-4023-82bf-8c20fe3190ca",
  "status": "queued",
  "created_at": "2026-10-28T01:03:01.505579+00:00",
  "started_at": null,
  "completed_at": null,
  "expires_at": "2026-10-29T01:03:01.505579+00:00",
  "queue_position": null,
  "progress": null,
  "outputs": [],
  "error": null,
  "outputs_reused": false,
  "urls": {
    "self": "http://127.0.0.1:8189/api/v2/jobs/813f058f-80fc-4023-82bf-8c20fe3190ca",
    "events": "http://127.0.0.1:8189/api/v2/jobs/813f058f-80fc-4023-82bf-8c20fe3190ca/events",
    "cancel": "http://127.0.0.1:8189/api/v2/jobs/813f058f-80fc-4023-82bf-8c20fe3190ca/cancel"
  },
  "metadata": "demo",
  "priority": 3
}
```

The job id is a fresh UUID that the proxy also sends to ComfyUI as `prompt_id` and `client_id`.

Before the graph goes to ComfyUI, every `core/ASSET` reference in it is replaced with the filename ComfyUI's loaders expect. A reference names an uploaded asset id, an output asset id, or a content hash:

```json
{"__type": "core/ASSET", "info": {"id": "31524f5f-055f-4bfc-9d5e-2418342382c2"}}
```

See [Assets](assets.md#asset-refs) for how each kind resolves.

| Status | Code | When |
|---|---|---|
| 201 | | Accepted by ComfyUI. |
| 400 | `invalid_request` | Body is not a JSON object, `extra_data` has another key or a non-string key value, or `Idempotency-Key` is empty or longer than 255 characters. |
| 422 | `invalid_workflow` | `workflow` missing or not an object, `webhook_url` or `inputs` present, or ComfyUI rejected the graph. A rejection carries ComfyUI's `node_errors` in `details`. |
| 422 | `workflow_format_ui` | The graph has a top-level `nodes` list and `links`. |
| 422 | `invalid_request` | `metadata` is not a string or is too long, `priority` is not an integer or is out of range. |
| 422 | `missing_asset` | A `core/ASSET` reference did not resolve. `details.missing_ids` lists them. |
| 422 | `idempotency_key_reuse` | The key was used before. |
| 503 | `upstream_unreachable` | ComfyUI could not be reached. |

When ComfyUI rejects a graph, `message` is ComfyUI's `error.message` (or `Workflow rejected.` when it sent none) and `details.node_errors` is ComfyUI's `node_errors` object as sent, or `{}`:

```json
{
  "error": {
    "code": "invalid_workflow",
    "message": "...",
    "details": {"node_errors": {...}}
  }
}
```

The proxy decodes workflow numbers exactly, so a 64-bit seed reaches ComfyUI unchanged. Object keys are re-encoded in sorted order.

## Idempotency-Key {#idempotency}

`Idempotency-Key` on `POST /api/v2/jobs` is single-use. The first submit with a key claims it, and any later submit with the same key is `422 idempotency_key_reuse`. There is no replay of the first response: after a reuse error, poll the job or list jobs instead of resubmitting.

The claim is released only when ComfyUI definitely created no job, which is when it answered with a rejection. When ComfyUI could not be reached, the outcome is unknown, so the key stays claimed and a retry with it cannot submit twice.

Claims live in memory, up to 10,000 of them with the oldest evicted first. With `--state-dir` they are written to SQLite and survive a restart. See [State and service](state-and-service.md).

## Get a job {#get}

`GET /api/v2/jobs/{id}` is the authoritative view of a job. Each call reads ComfyUI's `/history`, then `/queue` if the job is not in history, and merges the result with the proxy's own record (`created_at`, `metadata`, `priority`).

```json
{
  "id": "813f058f-80fc-4023-82bf-8c20fe3190ca",
  "status": "succeeded",
  "created_at": "2026-10-28T01:03:01.505579+00:00",
  "started_at": null,
  "completed_at": null,
  "expires_at": "2026-10-29T01:03:01.505579+00:00",
  "queue_position": null,
  "progress": null,
  "outputs": [
    {
      "node_id": "9",
      "name": "out.png",
      "type": "image",
      "content_type": "image/png",
      "size_bytes": 1553743,
      "id": "eyJqIjogIjgxM2Y...In0.A_8vZszyRfDl...",
      "hash": null,
      "url": "http://127.0.0.1:8189/api/v2/assets/eyJqIjogIjgxM2Y...In0.A_8vZszyRfDl.../content",
      "url_expires_at": "2026-10-29T01:03:01.734220+00:00",
      "job_id": "813f058f-80fc-4023-82bf-8c20fe3190ca"
    }
  ],
  "error": null,
  "outputs_reused": false,
  "urls": {"self": "...", "events": "...", "cancel": "..."},
  "metadata": "demo",
  "priority": 3
}
```

### Job fields {#job-fields}

| Field | Notes |
|---|---|
| `status` | `queued`, `running`, `succeeded`, `failed`, `canceled`, or `expired`. A cancel response can also say `canceling`. |
| `created_at`, `expires_at` | From the proxy's record. `expires_at` is `created_at` plus 24 hours. |
| `started_at`, `completed_at` | Always `null`. ComfyUI does not report them. |
| `queue_position` | 1-based position among pending prompts while `queued`, else `null`. |
| `progress` | `{"value": 0, "nodes_done": 0, "nodes_total": 0}` while `running`, else `null`. Live progress is on the [event stream](events.md). |
| `outputs` | Every output in the order ComfyUI listed them. Empty until the job is in history. |
| `error` | `null` unless `failed`. Then `{"code": "node_execution_error", "message", "node_id", "class_type", "traceback"}`, taken from ComfyUI's `execution_error`. `traceback` is `null`. |
| `outputs_reused` | `true` when ComfyUI served at least one node from its cache. |
| `metadata`, `priority` | Present only when set on submit. Absent, not `null`, otherwise. |

A job ComfyUI no longer knows but the proxy recorded reads as `expired`. A job interrupted by a cancel reads as `canceled`.

### Output fields {#output-fields}

| Field | Notes |
|---|---|
| `node_id` | The output node. |
| `name` | The filename ComfyUI wrote. For a [value output](#value-outputs), `<node>_<key>_<index>.txt`. |
| `type` | `image`, `video`, `audio`, `text`, `file`, or `latent`, from ComfyUI's `images`, `gifs`, `audio`, `text`, `files`, `latents` keys. |
| `content_type` | Guessed from the file extension. `text/plain` for a value output. |
| `size_bytes` | The file's size. ComfyUI's history does not carry sizes, so the proxy asks ComfyUI's `/view` once per output (a `HEAD`) and remembers the answer. `0` when ComfyUI cannot say, for example after the file was deleted. |
| `id` | A signed, stateless output asset id. See [Assets](assets.md#output-ids). |
| `hash` | Always `null` for outputs. |
| `url`, `url_expires_at` | The content URL and a time 24 hours after this read. |

The base of every URL in a response is [`public_url`](configuration.md#public-url) when it is set, else the scheme and `Host` the client used. `X-Forwarded-*` headers are ignored, so a client cannot point emitted links at another origin. Behind a TLS-terminating reverse proxy, set `public_url` instead.

### Value outputs {#value-outputs}

Some nodes return a value instead of writing a file: `PreviewAny` returns text, for example,
and `SaveText` returns the text as well as the file it saves. ComfyUI's history holds these as
raw strings under the node's key, and each becomes its own output entry: `type` from the key
(`text` for `text`), `content_type` `text/plain`, `size_bytes` the length of the value in
bytes, `name` `<node>_<key>_<index>.txt`, and an `id` and `url` like any other output. Its
content is served from ComfyUI's history as `text/plain; charset=utf-8` with
`X-Content-Type-Options: nosniff`, so it is never rendered as HTML or run as script. A
non-string value in that list is skipped. The Python proxy drops value outputs entirely
([differences](differences.md#other)).

| Status | Code | When |
|---|---|---|
| 200 | | Known to ComfyUI or to the proxy. |
| 404 | `not_found` | Unknown to both, or the id is not a UUID. |
| 500 | `internal_error` | A `--state-dir` read failed. |
| 503 | `upstream_unreachable` | ComfyUI could not be reached or returned invalid JSON. |

## List jobs {#list}

`GET /api/v2/jobs` lists the jobs this proxy submitted, newest first, each resolved against ComfyUI as `GET /jobs/{id}` would.

| Query | Notes |
|---|---|
| `limit` | 1 to 100. Default 50. |
| `status` | Comma-separated filter: any of `queued`, `running`, `succeeded`, `canceling`, `canceled`, `failed`, `expired`. |

```sh
curl -s 'http://127.0.0.1:8189/api/v2/jobs?limit=20&status=succeeded,failed'
```

```json
{"jobs": [{"id": "813f058f-...", "status": "succeeded", ...}], "truncated": false}
```

There is no cursor. Each request walks the newest records and stops after 500 of them, because each one costs two ComfyUI reads. `truncated: true` means the walk stopped before it ran out of records, so more matches may exist further back. `truncated: false` means there are no more matches in memory.

The list covers the in-memory window only. With `--state-dir`, the newest 500 records reload at startup. Older records still answer `GET /jobs/{id}` but do not appear in the list.

| Status | Code | When |
|---|---|---|
| 200 | | |
| 400 | `invalid_request` | `limit` is not an integer or is out of range, or `status` names an unknown value. |
| 503 | `upstream_unreachable` | ComfyUI could not be reached. |

## Get a job's workflow {#workflow}

`GET /api/v2/jobs/{id}/workflow` returns the graph the job ran with, after `core/ASSET` references were resolved. It is the proxy's own stored copy, never ComfyUI's history copy, which carries the partner API key.

```json
{"format": "api", "workflow": {"1": {"class_type": "EmptyImage", "inputs": {...}}}}
```

| Status | Code | When |
|---|---|---|
| 200 | | |
| 404 | `not_found` | The id is not a UUID, or the proxy holds no graph for it (for example, after a restart without `--state-dir`). |

## Cancel a job {#cancel}

`POST /api/v2/jobs/{id}/cancel` asks ComfyUI to cancel the job and returns the job object, as `GET /jobs/{id}` would. A running job reads `canceling` until the interrupt lands at the next node boundary, then `canceled`. Cancelling a finished job returns its terminal state. Poll the job or watch the [event stream](events.md) for the final status.

A job the proxy does not remember (after a restart without `--state-dir`) can still be cancelled while ComfyUI knows it.

| Status | Code | When |
|---|---|---|
| 200 | | |
| 404 | `not_found` | Unknown to the proxy and to ComfyUI, or the id is not a UUID. |
| 503 | `upstream_unreachable` | ComfyUI could not be reached. |
