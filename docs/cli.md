---
title: Command line
section: Run
order: 10
next: configuration
description: Every comfy-api-proxy command and flag, the exit codes, and the flags kept compatible with the Python reference proxy.
---

## Synopsis {#synopsis}

```sh
comfy-api-proxy [command] [flags]
```

The binary has five commands: `run`, `start`, `stop`, `status`, and `config`. With no command, or when the first argument is a flag, the arguments go to `run`. These two lines are the same:

```sh
comfy-api-proxy --comfyui http://127.0.0.1:8188 --port 8189
comfy-api-proxy run --comfyui http://127.0.0.1:8188 --port 8189
```

That keeps the bare invocation of the Python reference working unchanged.

| Command | What it does |
|---|---|
| `run` | Serve in the foreground until Ctrl+C or SIGTERM. |
| `start` | Validate the configuration, then serve in the background. Takes the same flags as `run`. |
| `stop` | Stop the background proxy. Succeeds when nothing is running. |
| `status` | Report whether the background proxy is running, and probe its health. |
| `config` | Print the resolved configuration and where each value came from. |

The root command also has `--help` (`-h`), `--version` (`-v`), `help [command]`, and `completion [shell]`, which prints a shell completion script.

## Server flags {#server-flags}

`run`, `start`, and `config` share one set of flags. Each one sets a configuration key and overrides the same key from the environment or the config file. A flag counts only when you pass it, so a flag left at its default never hides a value set lower down. See [configuration](configuration.md#layering) for the full precedence.

| Flag | Type | Default | Key |
|---|---|---|---|
| `--config` | path | `$XDG_CONFIG_HOME/comfy-api-proxy/config.toml` | none (env `COMFY_PROXY_CONFIG`) |
| `--comfyui` | URL | `http://127.0.0.1:8188` | `comfyui` |
| `--host` | string | `local` | `host` |
| `--port` | int | `8189` | `port` |
| `--token` | string | unset | `token` |
| `--comfyui-token` | string | unset | `comfyui_token` |
| `--comfyui-base-dir` | path | unset | `comfyui_base_dir` |
| `--max-upload-mb` | int | `100` | `max_upload_mb` |
| `--allow-insecure-bind` | bool | `false` | `allow_insecure_bind` |
| `--state-dir` | path | unset | `state_dir` |
| `--enable-cors-header` | origin, repeatable | none | `cors_origins` |
| `--public-url` | URL | unset | `public_url` |
| `--quiet` | bool | `false` | `quiet` |

What each flag does:

- `--config`: the TOML file to read. A file named here must exist. Without it, the proxy reads the default location and skips it quietly when it is missing.
- `--comfyui`: base URL of the ComfyUI to front. Must be `http` or `https` with a host. A trailing slash is dropped.
- `--host`: bind scope. `local` (127.0.0.1), `global` (0.0.0.0), `localhost`, or a literal IPv4 or IPv6 address. Anything that is not loopback needs `--token` or `--allow-insecure-bind`. See [remote access](remote-access.md#host).
- `--port`: the port to serve on, 1 to 65535. It is fixed. A taken port is an error, never a reason to move. See [remote access](remote-access.md#port).
- `--token`: require `Authorization: Bearer <token>` on every `/api/v2/*` request except `GET /api/v2/health`.
- `--comfyui-token`: a bearer token the proxy sends to ComfyUI on every request, for a ComfyUI behind an authenticating gateway. Never sent to clients.
- `--comfyui-base-dir`: the root of a ComfyUI install on the same machine. Turns on model-file uploads into `models/<category>/` and `POST /api/v2/assets/from-path`. Without it those are rejected and input uploads still work.
- `--max-upload-mb`: the largest single upload, in MB. At least 1.
- `--allow-insecure-bind`: permit a non-loopback bind without a token. Anyone who can reach the port can then drive your ComfyUI.
- `--state-dir`: a directory for the SQLite state that survives restarts. Off by default. See [state and service](state-and-service.md#state-dir).
- `--enable-cors-header`: a browser origin allowed to call the proxy, such as `https://app.example.com`. Pass it once per origin. `*` is refused.
- `--public-url`: the origin clients reach the proxy at, used for every link in a response. Set it behind a TLS-terminating reverse proxy. See [configuration](configuration.md#public-url).
- `--quiet`: turn off the per-request access log.

The token values are never printed by `config` or the startup panel. On a shared machine, prefer the environment or a config file with tight permissions over `--token` on the command line, where other local users can read it in the process list.

## run {#run}

```sh
comfy-api-proxy run [flags]
```

Loads the configuration, binds the port, prints a startup panel, and serves until Ctrl+C or SIGTERM. On shutdown it closes open event streams at once and gives in-flight requests up to 5 seconds to finish.

The startup panel names the upstream ComfyUI, the config file in use, the host and where it came from, whether auth is on, the state directory, model uploads, and CORS origins when set, and then every URL the proxy answers on:

```sh
comfy-api-proxy 2f02e5a listening
  upstream: http://127.0.0.1:8188
  config: none
  host: global (from flag --host)
  auth: bearer token
  url: http://127.0.0.1:8189
  url: http://192.168.1.20:8189
  url: http://100.64.0.7:8189
```

That is the plain form, printed when output is piped or redirected. On a terminal the same fields are styled. `NO_COLOR` is honored.

After the panel, the proxy logs one line per finished request with the time, method, path, status, and duration. Requests turned away by the origin guard or by auth are logged too. `--quiet` turns the access log off. Operational lines, such as how many expired assets a cleanup pass removed, still print.

## start {#start}

```sh
comfy-api-proxy start [flags]
```

Starts the proxy in the background with the same flags as `run`. It resolves and validates the configuration first, in the foreground, so a bad config file or a refused bind fails where you can see it. Then it:

1. Exits 1 if a proxy started by `start` is already running.
2. Exits 1 if anything already answers on the address and port.
3. Launches `comfy-api-proxy run` with the flags you passed, detached into its own session so closing the terminal does not stop it.
4. Waits up to 15 seconds for the port to open, then records the process and prints its URL, PID, and log path.

```sh
comfy-api-proxy start --host global --token "$TOKEN"
```

```sh
comfy-api-proxy started at http://127.0.0.1:8189 (pid 41872)
logs: /home/you/.local/state/comfy-api-proxy/proxy.log
stop: comfy-api-proxy stop
```

The background process inherits your environment and working directory, so environment variables and a relative `--config` path apply to it as they did to `start`. If it exits before binding, `start` exits 1 and points at the log. See [state and service](state-and-service.md#background) for where the record and log live.

## stop {#stop}

```sh
comfy-api-proxy stop
```

Sends SIGTERM to the recorded process, waits up to 5 seconds, then sends SIGKILL if it is still alive, and removes the record. When nothing of ours is running it prints `comfy-api-proxy is not running.` and exits 0. It signals a PID only while that PID still runs the binary `start` launched.

## status {#status}

```sh
comfy-api-proxy status
```

Exits 0 when the background proxy is running and 1 when it is not. When it is running, it also calls `GET /api/v2/health` on it with a 2 second timeout and prints the result:

```sh
comfy-api-proxy is running at http://127.0.0.1:8189 (pid 41872).
health: healthy
upstream: http://127.0.0.1:8188
logs: /home/you/.local/state/comfy-api-proxy/proxy.log
```

When the probe fails, the health line reads `no answer` with the reason. The health endpoint never calls ComfyUI, so `healthy` means the proxy answers, not that ComfyUI does. `upstream` is the ComfyUI URL the proxy was configured with.

## config {#config}

```sh
comfy-api-proxy config [flags]
```

Resolves the configuration exactly as `run` would, prints each value with its source, and exits. It takes the server flags, so you can check what a command line would produce before running it. Tokens print as `set (N characters)` or `unset`. See [configuration](configuration.md#inspect) for the output.

```sh
comfy-api-proxy config --host global --token "$TOKEN"
comfy-api-proxy config --config ./config.toml
```

Use it first when a remote bind does not answer or a value is not what you expected.

## Exit codes {#exit-codes}

| Code | Meaning |
|---|---|
| `0` | Success. `stop` with nothing running. `status` with the proxy running. |
| `1` | A runtime failure: the port is taken, the background process failed to start, `start` found a proxy already running, `status` found nothing running, or the command line itself was wrong (unknown command or flag). |
| `2` | A configuration error: an invalid value, an unknown key in the config file, a config file that was named but cannot be read, or a non-loopback bind without a token. |

Exit 2 matches the reference for a refused or invalid configuration. Every configuration error names where the bad value came from. See [configuration](configuration.md#errors).

## Reference-compatible flags {#reference-flags}

These flags carry the same names and meanings as in the Python `comfy-api-proxy`, so scripts and test harnesses written for it work against this binary:

`--comfyui`, `--host`, `--port`, `--token`, `--comfyui-base-dir`, `--max-upload-mb`, `--allow-insecure-bind`, `--state-dir`, `--enable-cors-header`.

The `run`, `start`, `stop`, and `status` commands also exist in the reference. What this binary adds:

- `--host` also takes `local`, `global`, and `localhost`. The default `local` binds 127.0.0.1, the reference default.
- `--config`, `--comfyui-token`, and `--quiet` are new, as are the `config` command and the environment and config-file layers. The reference takes flags only.
- `status` prints a health line, and `start`, `stop`, and `status` identify the background process by its executable rather than a command-line match. See [state and service](state-and-service.md#identify).

The rest of the differences are listed in [differences](differences.md).
