---
title: Configuration
section: Run
order: 20
next: remote-access
description: How the proxy resolves its settings from flags, environment variables, a TOML file, and defaults, with every key, its type and default, and the errors a bad value produces.
---

## Layering {#layering}

Every setting is resolved from four layers. The first layer that sets a key wins:

1. A command-line flag you passed.
2. A `COMFY_PROXY_*` environment variable.
3. The config file.
4. The built-in default.

A flag left at its default does not count as set, so `comfy-api-proxy run` with no flags still honors the environment and the file. The proxy records which layer each final value came from, prints it in `comfy-api-proxy config`, and names it in every error.

## Environment variables {#environment}

A key's variable is `COMFY_PROXY_` plus the key in upper case: `port` is `COMFY_PROXY_PORT`, `comfyui_base_dir` is `COMFY_PROXY_COMFYUI_BASE_DIR`.

- An empty or whitespace-only variable counts as unset. `COMFY_PROXY_PORT=` in a service file falls through to the config file instead of failing as a bad integer.
- Integers must parse as integers. Booleans take `true`, `false`, `1`, `0`, `t`, `f`, and the `TRUE`, `True`, `T` style variants of each.
- `COMFY_PROXY_CORS_ORIGINS` is a comma-separated list. Spaces around each origin are trimmed, and empty entries are dropped.

`COMFY_PROXY_CONFIG` names the config file. It is not a key.

```sh
COMFY_PROXY_HOST=global COMFY_PROXY_TOKEN="$TOKEN" comfy-api-proxy
COMFY_PROXY_CORS_ORIGINS="https://a.example.com, https://b.example.com" comfy-api-proxy
```

## Config file {#file}

The file is TOML. The proxy reads the first of:

1. `--config <path>`
2. `COMFY_PROXY_CONFIG`
3. The default location: `$XDG_CONFIG_HOME/comfy-api-proxy/config.toml`, which is `~/.config/comfy-api-proxy/config.toml` when `XDG_CONFIG_HOME` is unset. On macOS the default is `~/Library/Application Support/comfy-api-proxy/config.toml`.

Only the default location may be missing. A file named by `--config` or `COMFY_PROXY_CONFIG` must exist and be readable, or the proxy exits 2.

An unknown key is an error, so a typo fails loudly instead of leaving a setting at its default. That matters most for `host` and `token`, where a silent fallback would change who can reach the proxy.

A complete file, with every key at its default and the optional ones commented out:

```toml
comfyui = "http://127.0.0.1:8188"
host = "local"
port = 8189

# token = "change-me"
# comfyui_token = "secret"
# allow_insecure_bind = false
# comfyui_base_dir = "/path/to/ComfyUI"
# max_upload_mb = 100
# state_dir = "/path/to/state"
# cors_origins = ["https://app.example.com"]
# quiet = false
```

`config.example.toml` in the repository carries the same keys with a comment on each.

## Keys {#keys}

| Key | Type | Default | Flag | Environment |
|---|---|---|---|---|
| `comfyui` | string (URL) | `http://127.0.0.1:8188` | `--comfyui` | `COMFY_PROXY_COMFYUI` |
| `host` | string | `local` | `--host` | `COMFY_PROXY_HOST` |
| `port` | integer | `8189` | `--port` | `COMFY_PROXY_PORT` |
| `token` | string | unset | `--token` | `COMFY_PROXY_TOKEN` |
| `comfyui_token` | string | unset | `--comfyui-token` | `COMFY_PROXY_COMFYUI_TOKEN` |
| `allow_insecure_bind` | boolean | `false` | `--allow-insecure-bind` | `COMFY_PROXY_ALLOW_INSECURE_BIND` |
| `comfyui_base_dir` | string (path) | unset | `--comfyui-base-dir` | `COMFY_PROXY_COMFYUI_BASE_DIR` |
| `max_upload_mb` | integer | `100` | `--max-upload-mb` | `COMFY_PROXY_MAX_UPLOAD_MB` |
| `state_dir` | string (path) | unset | `--state-dir` | `COMFY_PROXY_STATE_DIR` |
| `cors_origins` | array of strings | empty | `--enable-cors-header` (repeatable) | `COMFY_PROXY_CORS_ORIGINS` (comma-separated) |
| `public_url` | string (URL) | unset | `--public-url` | `COMFY_PROXY_PUBLIC_URL` |
| `quiet` | boolean | `false` | `--quiet` | `COMFY_PROXY_QUIET` |

### comfyui {#comfyui}

Base URL of the ComfyUI the proxy fronts. It must use `http` or `https` and include a host. A trailing slash is removed.

Requests to ComfyUI never go through an `HTTP_PROXY` or `HTTPS_PROXY` from the environment, because they can carry the partner API key.

### host {#host}

The bind scope:

| Value | Binds |
|---|---|
| `local` | `127.0.0.1` |
| `localhost` | `127.0.0.1` |
| `global` | `0.0.0.0`, every IPv4 interface |
| an IPv4 or IPv6 literal | that address. Brackets around an IPv6 address are accepted. |

An empty host is an error rather than a wildcard bind. Any address that is not loopback needs `token` or `allow_insecure_bind`. See [remote access](remote-access.md#host).

### port {#port}

The port to serve on, 1 to 65535. It is fixed: if it is taken the proxy exits 1 and does not try another, because clients are configured with this one.

### token {#token}

When set, every `/api/v2/*` request except `GET /api/v2/health` must carry `Authorization: Bearer <token>`. Required for a non-loopback `host` unless `allow_insecure_bind` is set. See [remote access](remote-access.md#bearer-auth).

### comfyui_token {#comfyui-token}

A bearer token the proxy sends to ComfyUI on every request, for a ComfyUI behind an authenticating gateway. It is never sent to clients. It is independent of `token`.

### allow_insecure_bind {#allow-insecure-bind}

Permits a non-loopback bind with no `token`. Anyone who can reach the port can then submit work to your ComfyUI.

### comfyui_base_dir {#comfyui-base-dir}

The root of a ComfyUI install on the same machine as the proxy. Setting it turns on model-file uploads into `models/<category>/` and `POST /api/v2/assets/from-path`. Without it, model-file uploads are rejected and input uploads still work. A relative path is resolved against the working directory, and symlinks in it are resolved at startup. See [assets](assets.md).

### max_upload_mb {#max-upload-mb}

The largest single upload, in MB. Must be at least 1.

### state_dir {#state-dir}

Turns on SQLite persistence of job records, idempotency keys, the asset index, and the signing secret for output asset ids. Unset means all of that lives in memory and is lost on restart. See [state and service](state-and-service.md#state-dir).

This is not the directory where `start` keeps its PID record and log. That one is separate; see [state and service](state-and-service.md#service-dir).

### cors_origins {#cors-origins}

Browser origins allowed to call the proxy. Each entry must be an absolute `http` or `https` URL with a host and nothing after it: no path other than `/`, no query, no fragment, no user info. `*` is refused. Entries are normalized to the form a browser sends in its `Origin` header, with the default port dropped, so `https://app.example.com:443/` becomes `https://app.example.com`. The scheme and host are lowercased, as browsers send them, so `https://App.Example.com` matches too. See [remote access](remote-access.md#cors).

### public_url {#public-url}

The origin clients reach the proxy at, such as `https://comfy.example.com`. Every URL the proxy puts in a response (a job's `urls`, each output's `url`, an asset's `url`) starts with it. Unset, links follow the scheme and `Host` of each request, which is right when clients connect to the proxy directly. Behind a reverse proxy that terminates TLS, requests arrive as plain `http`, so set this or clients get `http://` links, treat them as another origin, and drop the bearer token. It must be an absolute `http` or `https` URL with a host and no query, fragment, or user info. A path is kept, for a reverse proxy that serves the proxy under a prefix and strips it. The scheme and host are lowercased and a trailing `/` is dropped. See [remote access](remote-access.md#reverse-proxy).

### quiet {#quiet}

Turns off the per-request access log.

## Errors {#errors}

A configuration error exits 2 and names the layer the bad value came from, so you know which flag, variable, or file to fix. The source reads `flag --port`, `env COMFY_PROXY_PORT`, `config /path/to/config.toml`, or `default`.

| Problem | Message |
|---|---|
| bad `comfyui` | `comfyui must be an http(s) URL with a host, got "ftp://x" (from flag --comfyui)` |
| bad `host` | `host must be "local", "global", "localhost", or an IP address, got "foo" (from flag --host)` |
| empty `host` | `host is empty; use "local", "global", or an IP address (from ...)` |
| `port` out of range | `port must be between 1 and 65535, got 70000 (from flag --port)` |
| `max_upload_mb` below 1 | `max_upload_mb must be at least 1, got 0 (from flag --max-upload-mb)` |
| bad CORS origin | `invalid --enable-cors-header: CORS origin '*' is not allowed; pass explicit origins such as https://app.example.com (from flag --enable-cors-header)` |
| non-loopback bind, no token | `refusing to bind non-loopback host "global" (from flag --host) without a token.` followed by the three ways to set one |
| env var not an integer | `COMFY_PROXY_PORT must be an integer, got "abc"` |
| env var not a boolean | `COMFY_PROXY_QUIET must be true or false, got "maybe"` |
| named file missing | `cannot read config file /path/config.toml: open /path/config.toml: no such file or directory` |
| unknown key | `config file /path/config.toml has unknown keys:` followed by the offending line, marked |
| wrong type in the file | `config file /path/config.toml: toml: cannot decode TOML string into struct field config.Config.Port of type int` |

An unknown key points at its line:

```sh
error: config file /home/you/.config/comfy-api-proxy/config.toml has unknown keys:
1| hots = "global"
 | ~~~~ unknown field
```

A taken port is a runtime error, exit 1, and also names where the port came from. See [remote access](remote-access.md#port).

## Inspecting the result {#inspect}

`comfy-api-proxy config` resolves everything exactly as `run` would, prints each value with its source, and exits. It accepts the same flags, so you can test a command line before you use it.

```sh
comfy-api-proxy config --port 9001
```

```sh
file: /home/you/.config/comfy-api-proxy/config.toml (loaded)
comfyui: http://127.0.0.1:8188 (default)
host: local → 127.0.0.1 (config /home/you/.config/comfy-api-proxy/config.toml)
port: 9001 (flag --port)
token: unset (default)
comfyui token: unset (default)
insecure: false (default)
base dir: none (default)
upload: 100 MB (default)
state: none (default)
cors: none (default)
quiet: false (default)
```

The `file` line says whether the file was `loaded` or `not found, using defaults`, and adds `named explicitly` when it came from `--config` or `COMFY_PROXY_CONFIG`. `host` shows the setting and the address it resolves to. Tokens print as `set (N characters)` or `unset`, never their value.

A configuration that would fail at startup fails here too, with the same message and exit code 2.
