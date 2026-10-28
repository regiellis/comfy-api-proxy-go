---
title: Assets
section: API
order: 30
next: sdk
description: Uploading inputs and model files, content-hash dedup, downloads with Range, signed output asset ids, and the guards on model placement under --comfyui-base-dir.
---

## Overview {#overview}

An asset is a file the proxy can hand to a workflow or serve back to a client. There are three kinds:

| Kind | Id | Where the bytes live |
|---|---|---|
| Uploaded input | A UUID (or ComfyUI's own asset id, when its asset catalog returns one) | ComfyUI's `input/` directory, via ComfyUI's `/upload/image` |
| Placed or registered file | A UUID | On disk under `--comfyui-base-dir` |
| Job output | A signed output id | ComfyUI's output directory, read through `/view` |

Uploaded and registered assets are kept in an index in memory, written through to SQLite with `--state-dir`. Output ids are not stored anywhere: the id carries its own signed reference.

The proxy fronts a single-user ComfyUI, so there is one namespace. Every client sees every asset.

## Upload {#upload}

`POST /api/v2/assets` takes `multipart/form-data`.

| Part | Notes |
|---|---|
| `file` | Required. The bytes. Its part `Content-Type` is the asset's content type, `application/octet-stream` when absent. |
| `file_path` | Required. Where the file goes. See [Destination paths](#paths). |
| `content_type` | Optional. Overrides the `file` part's content type. |
| `expected_hash` | Optional. `blake3:<hex>`. Compared case-insensitively to the received bytes. A mismatch is `409 hash_mismatch`. |
| `expires_in` | Optional. Seconds until the asset record expires. `0` means never. A value that is not an integer is ignored. |
| `tags` | Optional, repeatable. Stored with the record. |

```sh
curl -s http://127.0.0.1:8189/api/v2/assets \
  -F 'file=@photo.png;type=image/png' \
  -F file_path=photo.png \
  -F expires_in=3600
```

```json
{
  "id": "31524f5f-055f-4bfc-9d5e-2418342382c2",
  "hash": "blake3:a38747620a3c8e134e2c836e994b2ac307601d7523b86f686bf1a27be09b9566",
  "size_bytes": 17,
  "content_type": "image/png",
  "file_path": "photo.png",
  "created_at": "2026-10-28T01:03:17.075514+00:00",
  "url": "http://127.0.0.1:8189/api/v2/assets/31524f5f-055f-4bfc-9d5e-2418342382c2/content",
  "url_expires_at": "2026-10-29T01:03:17.075515+00:00",
  "created_new": true,
  "expires_at": "2026-10-28T02:03:17.000000+00:00"
}
```

`expires_at` is present only when `expires_in` set one. `created_new` is `true` when this upload created the record and `false` when the bytes were already known.

### Streaming and the size cap {#spooling}

The `file` part is written to a temp file while it is hashed, then streamed onward from that file, to ComfyUI or into the model directory. The upload is never held in memory whole. Reading stops one byte past `max_upload_mb` (default 100 MB), which answers `413 payload_too_large`. The temp file is removed when the request ends.

### Input limits {#limits}

| Rule | Result |
|---|---|
| More than 64 parts | `422 invalid_request` |
| `file_path` or `content_type` contains a control character (below U+0020, or U+007F) | `422 invalid_request` |
| `expires_in` lands after 9999-12-31T23:59:59Z, or before the Unix epoch | `422 invalid_request` |

The control-character rule keeps a value from injecting headers into the multipart request sent to ComfyUI.

### Status codes {#upload-status}

| Status | Code | When |
|---|---|---|
| 201 | | A new asset. |
| 200 | | The bytes match an existing asset, which is returned with `created_new: false`. |
| 409 | `hash_mismatch` | `expected_hash` differs from the received bytes, or a model file appeared at the destination during the write. |
| 413 | `payload_too_large` | The file is larger than `max_upload_mb`. |
| 422 | `invalid_request` | Not multipart, malformed body, missing `file` or `file_path`, a rejected path, a failed model check, or a limit above. |
| 500 | `upstream_error` | ComfyUI could not be reached for an input upload, or rejected it. |

The order matters for dedup: `file_path` is validated first, then `expected_hash`, then the hash lookup. Bytes the proxy already holds return the existing asset before any model placement check runs.

## Destination paths {#paths}

`file_path` decides where an upload goes.

| `file_path` | Destination |
|---|---|
| `photo.png` | A workflow input, uploaded to ComfyUI's `input/` |
| `input/refs/photo.png` | A workflow input in the `refs` subfolder of `input/` |
| `checkpoints/model.safetensors` | A model file, placed under `<base dir>/models/checkpoints/` |
| `models/loras/style.safetensors` | The same as `loras/style.safetensors`. A leading `models/` is dropped. |

Every path is checked before anything is written:

- It must be relative. A leading `/` or `\` is refused.
- No segment may be `..`, anywhere in the path. This runs before the path is classified, so `input/../checkpoints/x.safetensors` cannot pass as an input and skip the model checks.
- A path with a directory must start with an allowlisted root: `input`, or one of the model roots below.

ComfyUI may rename an input upload, for example when the name is taken. The record keeps the name ComfyUI returned, and that name is what `core/ASSET` references resolve to.

## Model placement {#models}

ComfyUI's own upload endpoint writes only to `input`, `output`, and `temp`. The proxy can place model weights itself, when it shares a filesystem with ComfyUI and was started with `--comfyui-base-dir` pointing at the ComfyUI install root. Without it, a model upload is `422 invalid_request` and input uploads work as usual.

The allowlisted model roots are the weight directories directly under ComfyUI's `models/`:

```text
audio_encoders  background_removal  checkpoints  classifiers  clip_vision
controlnet  diffusers  diffusion_models  embeddings  frame_interpolation
geometry_estimation  gligen  hypernetworks  latent_upscale_models  loras
model_patches  optical_flow  photomaker  style_models  text_encoders
upscale_models  vae  vae_approx
```

`custom_nodes` and `configs` are left out on purpose. One holds arbitrary Python and the other YAML that loaders read.

A model upload must pass each of these before it lands:

| Guard | Refusal |
|---|---|
| The name ends in `.safetensors`. | 422 |
| The file parses as safetensors: an 8-byte little-endian header length (non-zero, at most 100 MiB, and within the file), then a UTF-8 JSON object whose entries, apart from `__metadata__`, each carry `dtype`, `shape`, and `data_offsets`. The tensor bytes are never read, and no pickle path exists. | 422 |
| The destination, with every existing symlink along it resolved, is still under the resolved base directory. | 422 |
| Nothing exists at the destination. | 422 |

Then the bytes go to a hidden temp file in the destination directory, so the final rename stays on one filesystem. The temp file is set to mode 0644, so a ComfyUI running as another user can read it. The destination is claimed with an `O_CREAT|O_EXCL` create, which fails if another upload got there first (`409 hash_mismatch`), and the temp file is renamed over the empty claim. A reader never sees a partial file, and an existing file is never overwritten.

The record's `file_path` is relative to the model root, without the category: `model.safetensors`, not `checkpoints/model.safetensors`. That is how ComfyUI's loader widgets name a model, so a `core/ASSET` reference to the asset resolves to a value the loader accepts.

## Register a host file {#from-path}

`POST /api/v2/assets/from-path` registers a file already under `--comfyui-base-dir` without copying it.

| Field | Notes |
|---|---|
| `path` | Required. A host path. Symlinks are resolved before the checks. |
| `file_path` | Optional display path that replaces the derived one. |
| `content_type` | Optional. Guessed from the extension when absent. |

```sh
curl -s http://127.0.0.1:8189/api/v2/assets/from-path \
  -H 'Content-Type: application/json' \
  -d '{"path": "/opt/ComfyUI/models/loras/style.safetensors"}'
```

The resolved file must be a regular file under the base directory, inside `input/`, `output/`, `temp/`, or `models/`. A file under `models/` passes the same root allowlist and safetensors header check as an upload. The file is hashed with blake3, and a hash the proxy already holds returns the existing asset.

| Status | Code | When |
|---|---|---|
| 201 | | Registered. |
| 200 | | The bytes match an existing asset. |
| 400 | `invalid_request` | The body is not JSON. |
| 404 | `blob_not_found` | The path does not exist or is not a regular file. |
| 413 | `payload_too_large` | The file is larger than `max_upload_mb`. |
| 422 | `invalid_request` | No `--comfyui-base-dir`, missing `path`, a path outside the base directory or the four roots, or a failed model check. |

## Dedup by hash {#dedup}

Every asset hash is `blake3:` followed by 64 hex digits, the 32-byte BLAKE3 digest of the bytes. The first record with a given hash owns it, and later identical bytes resolve to that record. A client can check before uploading:

```sh
curl -sI http://127.0.0.1:8189/api/v2/assets/by-hash/blake3:a38747620a3c8e134e2c836e994b2ac307601d7523b86f686bf1a27be09b9566
```

`HEAD /api/v2/assets/by-hash/{hash}` answers `200` or `404` with no body.

`POST /api/v2/assets/from-hash` with `{"hash": "blake3:..."}` returns the existing asset with `created_new: false`. A miss is `404 blob_not_found`. A missing `hash` is `422 invalid_request`, and a body that is not JSON is `400 invalid_request`.

## Metadata and downloads {#downloads}

`GET /api/v2/assets/{id}` returns the asset record for an uploaded or registered id. For an output id, it returns a record built from the id, with `hash: null`, `size_bytes` from ComfyUI's `/view` (the same number the job reports, `0` when ComfyUI cannot say), `file_path` set to the output filename, and `job_id`. An unknown or invalid id is `404 not_found`.

`GET /api/v2/assets/{id}/content` returns the bytes, and `HEAD` returns the headers alone. Both accept `Range`.

```sh
curl -s -i -H 'Range: bytes=0-4' \
  http://127.0.0.1:8189/api/v2/assets/31524f5f-055f-4bfc-9d5e-2418342382c2/content
```

```http
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Content-Length: 5
Content-Range: bytes 0-4/69
Content-Type: image/png
```

A placed or registered file is served from disk, with `Range` handled by the proxy. An input or output held by ComfyUI is streamed from ComfyUI's `/view`, with `Range` passed through and `Content-Length`, `Content-Range`, and `Accept-Ranges` copied back. The body is copied in chunks, so a large output never sits in memory.

| Status | Code | When |
|---|---|---|
| 200, 206 | | The bytes. |
| 404 | `not_found` | Unknown or invalid id. |
| 404 | `output_unavailable` | The asset is known but its bytes are gone, from disk or from ComfyUI. Re-run the job with a changed prompt. |
| 405 | `method_not_allowed` | A method other than `GET` or `HEAD`. |
| 500 | `upstream_error` | ComfyUI could not be reached. |
| 502 | `upstream_error` | ComfyUI answered with another status. |

## Delete and expiry {#delete}

`DELETE /api/v2/assets/{id}` removes the record from the index and answers `204`. An unknown id is `404 not_found`. The file itself stays where it is, in ComfyUI's directories or under the base directory.

An asset with `expires_at` is removed from the index once that time passes. A sweep runs every 300 seconds, or every `COMFY_ASSET_CLEANUP_INTERVAL` seconds when that variable holds a positive integer. It runs with and without `--state-dir`, and `expires_at` persists in the state database. Like a delete, the sweep removes records, not files.

## Output asset ids {#output-ids}

Every output on a job carries a stateless id:

```text
base64url(payload) "." base64url(HMAC-SHA256(secret, base64url(payload)))
```

The payload is a JSON object naming the job, filename, subfolder, and ComfyUI folder type:

```json
{"j": "813f058f-80fc-4023-82bf-8c20fe3190ca", "f": "out.png", "s": "", "t": "output"}
```

Both parts use unpadded base64url. The job id inside the signed payload means two jobs that write the same filename get different ids, so a client holding one job's URL is never served another job's bytes. The signature means a client cannot forge an id naming an arbitrary file that `/view` can reach. Any id that fails verification, including a well-formed forgery or a single changed character, reads as `404 not_found`.

The payload is serialized exactly as Python's `json.dumps` writes it, so this binary and the Python proxy mint the same id for the same output and secret.

A [value output](api.md#value-outputs) gets an id of the same form whose payload names the job,
node, key, and position in the list (`{"j", "n", "k", "i"}`) in place of a file. It is signed
with the same secret, carries no `f`, and so never decodes as a file id or the other way round.
Its content is read back from ComfyUI's history on each request. The proxy keeps no copy.

The secret is 32 random bytes made at startup. With `--state-dir`, it is stored in the database and reused, so ids minted before a restart still verify. Without it, a restart makes a new secret, and earlier output ids read as not found. See [State and service](state-and-service.md).

## core/ASSET references {#asset-refs}

A workflow input can name an asset instead of a filename. On submit, the proxy replaces each reference with the value ComfyUI's loaders expect:

```json
{"__type": "core/ASSET", "info": {"id": "31524f5f-055f-4bfc-9d5e-2418342382c2"}}
```

| `info` | Resolves to |
|---|---|
| `id` of an uploaded input | The ComfyUI filename, with its subfolder |
| `id` of a placed or registered model | The root-relative `file_path` |
| `id` of a job output | The output's filename, with its subfolder |
| `hash` (used when `id` is absent or does not resolve) | The asset that owns that hash |

A reference that does not resolve, or has no `info` object, fails the submit with `422 missing_asset` and lists it in `details.missing_ids`. See [Submit a job](api.md#submit).
