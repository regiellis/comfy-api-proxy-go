---
title: State and background service
section: Run
order: 40
next: api
description: What --state-dir persists across restarts and how expired assets are cleaned up, and how start, stop, and status run and track the proxy in the background.
---

## Two directories {#two-directories}

The proxy uses two unrelated directories, and it helps to keep them apart:

| Directory | Set by | Holds | Default |
|---|---|---|---|
| State directory | `state_dir` / `--state-dir` | SQLite database of proxy state | unset, state is in memory |
| Service directory | `COMFY_API_PROXY_STATE_DIR` | PID record and log of the background process | `$XDG_STATE_HOME/comfy-api-proxy` |

The first is about what survives a restart. The second is only about `start`, `stop`, and `status`.

## Persistence with --state-dir {#state-dir}

ComfyUI keeps the queue, history, and outputs. The proxy keeps only what ComfyUI cannot tell it. Without a state directory, that lives in memory and is gone when the process exits. With one, every write goes through to SQLite and is loaded again at startup.

```sh
comfy-api-proxy --state-dir ~/.local/share/comfy-api-proxy/state
```

The database is `state.sqlite3` in that directory, opened in WAL mode, so `state.sqlite3-wal` and `state.sqlite3-shm` sit beside it. The SQLite driver is pure Go, so the binary still needs no C library.

### What is stored {#stored}

| Table | Contents |
|---|---|
| `jobs` | Each submitted job's id, `created_at`, client id, `metadata`, `priority`, and the resolved workflow graph. |
| `idempotency_keys` | Every `Idempotency-Key` claimed by a submit, with when it was claimed. At most 10,000 are kept; the oldest are dropped first. |
| `assets` | The asset index: id, blake3 hash, size, content type, file path, where it lives in ComfyUI, tags, and `expires_at`. |
| `asset_hash_index` | Hash to asset id, for deduplication and `HEAD /api/v2/assets/by-hash/{hash}`. |
| `meta` | The secret that signs output asset ids. |

### What survives a restart {#survives}

- **Output asset ids.** The signing secret is created once and reused, so an output id minted before a restart still resolves after it. Without a state directory a new secret is drawn on every start, and every earlier output id reads as not found.
- **Job metadata.** `GET /api/v2/jobs/{id}` still returns a job's `metadata`, `priority`, and `created_at`, and `GET /api/v2/jobs/{id}/workflow` its graph. The status itself is always read live from ComfyUI.
- **Idempotency.** A key claimed before the restart is still rejected after it, with 422 `idempotency_key_reuse`, so a client retrying across a restart cannot submit twice. Keys are single-use and a reuse does not replay the first response.
- **Uploaded assets.** Uploads stay findable by id and by hash.

Only the newest 500 job records load into memory at startup. Older ones stay in the database, and `GET /api/v2/jobs/{id}` reads through to it, so they still resolve. `GET /api/v2/jobs` lists only the in-memory set.

What a state directory does not do: it does not make ComfyUI's history or its output files durable. If ComfyUI has dropped a job from its history or its output files are gone, the proxy reports that, for example as `404 output_unavailable`. See [assets](assets.md).

### Growth {#growth}

Every accepted submit stores its full workflow graph, and nothing prunes the `jobs` table. A proxy that runs for months with large graphs grows its database without bound. Rotate or remove the state file on your own schedule; removing it also resets the signing secret, which invalidates earlier output ids.

### Permissions {#permissions}

The directory is set to `0700` and the database to `0600`, because the signing secret is in it and anyone who can read it can mint valid output ids. The directory mode also covers the WAL and SHM files. On a filesystem without POSIX modes, such as FAT or some network mounts, the change can fail. The proxy then starts anyway and prints a warning that the signing secret may be readable by other local users.

### One directory per proxy {#one-per-proxy}

A state directory belongs to one proxy in front of one ComfyUI. To front several ComfyUI instances, run one proxy for each, each with its own `comfyui`, `port`, and `state_dir`.

### Compatibility with the Python proxy {#compat}

The schema is the reference's, plus one column. A state directory written by the Python proxy opens here, and the proxy adds a nullable `assets.expires_at` column to it on first open. Output ids are minted the same way in both, so a state directory moved between the two keeps every id.

## Expired asset cleanup {#cleanup}

An upload with `expires_in` gets an `expires_at` time. Every 300 seconds the proxy removes assets whose `expires_at` has passed from its index, and from the database when a state directory is set. When it removes any, it logs `Cleaned up N expired asset(s)`.

`COMFY_ASSET_CLEANUP_INTERVAL` sets the interval in seconds, under the same name the reference reads. A value that is not a positive integer is ignored.

This differs from the reference in two ways. The reference skipped cleanup when a state directory was set, and it never stored `expires_at`, so an expiry was lost on restart. Here cleanup runs either way and `expires_at` persists.

## Running in the background {#background}

`start`, `stop`, and `status` manage one background proxy per user.

```sh
comfy-api-proxy start --host global --token "$TOKEN"
comfy-api-proxy status
comfy-api-proxy stop
```

`start` takes the same flags as `run` and validates the configuration in the foreground, so a refused bind or a bad config file fails at the prompt. It refuses to start when a proxy it started is already running, or when anything already answers on the port. It then launches `comfy-api-proxy run` with the flags you passed, in a new session so closing the terminal does not stop it, and waits up to 15 seconds for the port to open. If the child exits first, `start` exits 1 and points at the log. If it never binds, `start` stops it and exits 1.

The child inherits your environment and working directory. Environment variables and a relative `--config` path resolve for it the same way they did for `start`.

`stop` sends SIGTERM, waits up to 5 seconds for a clean exit, then sends SIGKILL. A clean exit closes open event streams and gives in-flight requests up to 5 seconds. With nothing running, `stop` exits 0.

`status` exits 0 when the proxy is running and 1 when it is not. See [command line](cli.md#status).

### Service directory {#service-dir}

The record and the log live in the first of:

1. `$COMFY_API_PROXY_STATE_DIR`, the same override the reference reads
2. `$XDG_STATE_HOME/comfy-api-proxy`
3. `~/.local/state/comfy-api-proxy`

The directory is created `0700` when it does not exist. It holds two files, both `0600`:

| File | Contents |
|---|---|
| `proxy.json` | The PID record written once the child is listening. |
| `proxy.log` | The child's standard output and error: its startup panel, access log, and any error that stopped it. Appended to, never rotated. |

The record:

```json
{"pid": 41872, "exe": "/usr/local/bin/comfy-api-proxy", "host": "local", "port": 8189, "url": "http://127.0.0.1:8189"}
```

`url` is the address `status` probes. For a wildcard bind it is the loopback address, since a wildcard cannot be dialed. A missing or corrupt record counts as not running, and `stop` removes a stale one.

### Health line {#health}

When the proxy is running, `status` calls `GET /api/v2/health` on the recorded URL with a 2 second timeout:

```sh
comfy-api-proxy is running at http://127.0.0.1:8189 (pid 41872).
health: healthy
upstream: http://127.0.0.1:8188
logs: /home/you/.local/state/comfy-api-proxy/proxy.log
```

If the proxy does not answer, or answers with anything other than 200, the line reads `health: no answer` with the reason. The health endpoint never calls ComfyUI and needs no token, so this works with auth on, and `healthy` says the proxy is serving, not that ComfyUI is up.

### How the process is identified {#identify}

A PID alone is not proof: after a crash or reboot the number can belong to something else, and signalling it would hit the wrong process. So `start` records the resolved path of the binary it launched, and `stop` and `status` treat the PID as ours only while it is alive and still runs that binary.

- On Linux the check reads `/proc/<pid>/exe` and compares it with the recorded path. When the binary has been rebuilt or replaced under a running proxy, the kernel reports the old path with ` (deleted)` after it; that suffix is ignored, so the running proxy is still recognized.
- Where `/proc` is not available, the check runs `ps` and requires the command's program name to match and its first argument to be `run`.
- If neither can be read, it falls back to whether the PID is alive.

The reference matches a substring of the command line instead. This binary compares the executable, which a different program cannot match by accident.
