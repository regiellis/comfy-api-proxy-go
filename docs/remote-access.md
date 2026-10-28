---
title: Remote access
section: Run
order: 30
next: state-and-service
description: How to reach the proxy from another machine or a browser, what each bind and auth setting does, and the security defaults that hold when you change nothing.
---

## The short version {#short-version}

The proxy listens on 127.0.0.1:8189 by default, so only the machine it runs on can reach it. To reach it from elsewhere, bind every interface and set a token:

```toml
# ~/.config/comfy-api-proxy/config.toml
host = "global"
token = "pick-a-long-random-string"
```

```sh
comfy-api-proxy start
comfy-api-proxy status
```

The startup panel, or the log for `start`, lists the LAN and VPN addresses to use. Clients send `Authorization: Bearer <token>` on every `/api/v2/*` call except `GET /api/v2/health`.

The token travels in plain HTTP. The proxy does not terminate TLS, so across a network you do not trust, put it behind a VPN or a TLS reverse proxy.

## Behind a reverse proxy {#reverse-proxy}

A reverse proxy such as Caddy, nginx, or Traefik can terminate TLS in front of the proxy. The request then reaches the proxy as plain `http`, and without more configuration every link in a response (a job's `urls`, each output's `url`) would read `http://`. Clients treat that as a different origin and drop the bearer token. Tell the proxy its public origin:

```toml
host = "local"                              # the reverse proxy on this machine connects to 127.0.0.1
public_url = "https://comfy.example.com"
token = "pick-a-long-random-string"
```

The proxy never reads `X-Forwarded-Proto` or `X-Forwarded-Host`, because any client could send them and point the links somewhere else. You state the origin once in configuration instead. A path in `public_url` is kept, for a reverse proxy that serves the API under a prefix and strips it before passing the request on.

## Host {#host}

`host` picks the address the listener binds:

| Value | Binds | Reachable from |
|---|---|---|
| `local` (default) | `127.0.0.1` | this machine |
| `localhost` | `127.0.0.1` | this machine |
| `global` | `0.0.0.0` | every IPv4 interface |
| an IPv4 literal, such as `192.168.1.20` | that address | that interface |
| an IPv6 literal, such as `::1` or `::` | that address | that interface, or every IPv6 interface for `::` |

The listener uses `tcp4` for an IPv4 address and `tcp6` for an IPv6 one. `0.0.0.0` stays IPv4 only and never becomes a dual-stack socket that also listens on IPv6 addresses the startup output did not print. To serve IPv6, bind an IPv6 address.

`localhost` is mapped to 127.0.0.1 without a DNS lookup.

## Token required off loopback {#token-required}

Any `host` that is not a loopback address needs a token. Without one the proxy refuses to start and exits 2:

```sh
error: refusing to bind non-loopback host "global" (from flag --host) without a token.
Set a token (--token, COMFY_PROXY_TOKEN, or token in the config file), or set allow_insecure_bind to expose the proxy unauthenticated.
```

`allow_insecure_bind = true` (or `--allow-insecure-bind`) lifts the check. Then anyone who can reach the port can submit workflows to your ComfyUI, read its outputs, and, with `comfyui_base_dir` set, place model files on disk. Use it only on a network where every host is yours.

## Bearer auth {#bearer-auth}

With `token` set, every request under `/api/v2/` must carry:

```sh
Authorization: Bearer <token>
```

- The scheme name `Bearer` is matched without regard to case. The token is compared in constant time.
- A missing or malformed header is a 401 with code `unauthorized` and the message `Missing or malformed Authorization header.` A wrong token is a 401 with `Invalid token.`
- `GET /api/v2/health` stays open, so a supervisor or `comfy-api-proxy status` can probe the proxy without the secret.
- `OPTIONS` requests pass, because a browser's CORS preflight never carries `Authorization`.

```sh
curl -H "Authorization: Bearer $TOKEN" http://192.168.1.20:8189/api/v2/jobs
```

```json
{"error": {"code": "unauthorized", "message": "Invalid token.", "details": null}}
```

A token is also useful on loopback: it stops other local users and local software from driving the proxy.

## Startup URLs {#startup-urls}

When the bind is a specific address, the startup panel lists that one URL. When it is a wildcard (`global` or `::`), it lists loopback first and then the address of every interface that is up, in the bound address family:

```sh
  url: http://127.0.0.1:8189
  url: http://192.168.1.20:8189
  url: http://100.64.0.7:8189
```

The list leaves out:

- Container and VM bridge interfaces whose names start with `docker`, `br-`, `veth`, `virbr`, `cni`, `flannel`, `vmnet`, `vboxnet`, `lxcbr`, `lxdbr`, or `podman`. They are reachable only from this machine's own guests, and on a Docker host there can be dozens of them burying the LAN and VPN addresses.
- IPv6 link-local addresses, which do not work in a URL without a zone.

The proxy still listens on those interfaces when bound to a wildcard. They are only omitted from the printout.

## Port 8189 {#port}

The port defaults to 8189 and stays where you put it. SDK clients are configured with a fixed base URL, and a proxy that moved to the next free port would break them without saying so.

When the port is taken, `run` exits 1 and says where the value came from and how to change it:

```sh
error: port already in use: 127.0.0.1:8189 (port from default).
Another process is holding it. Stop that process, or change the port with --port, COMFY_PROXY_PORT, or port in the config file.
The proxy does not pick another port because clients are configured with this one.
```

`start` checks before it launches anything and exits 1 with `comfy-api-proxy cannot start: http://127.0.0.1:8189 is already in use.`

## Browser access and CORS {#cors}

Browsers are blocked from calling the proxy cross-site unless you allowlist their origin. To let a hosted web app call a proxy on your machine:

```sh
comfy-api-proxy --enable-cors-header https://app.example.com --token "$TOKEN"
```

```toml
cors_origins = ["https://app.example.com", "http://localhost:3000"]
```

For a request whose `Origin` is on the list, the proxy:

- answers a preflight `OPTIONS` with 204;
- sets `Access-Control-Allow-Origin` to that origin (never `*`), with `Vary: Origin` and `Access-Control-Allow-Credentials: true`;
- allows the methods `GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS` and the headers `Authorization`, `Content-Type`, and `Idempotency-Key`;
- exposes `Retry-After`, `Content-Range`, and `Accept-Ranges` to the page;
- lets the browser cache the preflight for 600 seconds.

The CORS headers go on before auth runs, so a 401 or 403 is still readable by an allowlisted page. Event streams get the same headers.

Origins are matched exactly against the normalized list, so the scheme, host, and port must all match what the browser sends. `*` is refused at startup, since an authenticated proxy must never answer every origin. See [configuration](configuration.md#cors-origins) for the accepted forms.

## Origin guard {#origin-guard}

With no configuration at all, the proxy rejects cross-site browser requests. This is ComfyUI core's origin-only middleware, ported through the reference. It stops a page you have open in a browser from scripting requests at a server on your machine (drive-by CSRF and DNS rebinding).

For a request whose `Origin` is not allowlisted:

1. `Sec-Fetch-Site: cross-site` is rejected. Browsers set that header and page script cannot forge it.
2. When `Host` names a loopback address and an `Origin` is present, the two hosts must match. Ports are compared only when both carry one. A `Host` that is a name counts as loopback when every address it resolves to is loopback.

Both rejections are a 403 with code `forbidden_origin`:

```json
{"error": {"code": "forbidden_origin", "message": "Origin 'http://evil.example' does not match request host '127.0.0.1:8189'.", "details": null}}
```

What passes:

- Requests with no `Origin`, such as curl and the SDKs. Browsers always send one on the cross-site requests the guard exists to stop.
- Same-origin requests, where the `Origin` host matches `Host`.
- Requests to a non-loopback `Host`. A reverse proxy may rewrite `Host`, so the check is left to the token and the network in that case.

The guard applies to every path, routed or not. It follows the reference's string comparison exactly, including its quirks, each of which fails closed. For example, an IPv6 origin without a port, `http://[::1]`, does not match a `Host` of `[::1]:8189`.

## Upstream auth with comfyui_token {#comfyui-token}

If ComfyUI sits behind a gateway that requires a bearer token, set `comfyui_token`:

```toml
comfyui = "https://comfy.internal.example.com"
comfyui_token = "gateway-token"
```

The proxy sends `Authorization: Bearer <comfyui_token>` on every request to ComfyUI, including the WebSocket handshake behind the event stream. It never forwards the token to clients and never prints it. It is unrelated to `token`, which is what clients present to the proxy.

## Security defaults {#security-defaults}

Each of these holds without any configuration:

| Default | What it protects against |
|---|---|
| Bind to 127.0.0.1 | Other machines reaching the proxy at all. |
| Refuse a non-loopback bind without a token (exit 2) | Exposing an unauthenticated proxy to the network by setting `host` alone. |
| Unknown config keys are errors | A typo such as `tokne` silently leaving the proxy without auth. |
| Bearer token compared in constant time, health left open | Token guessing by timing, while still allowing liveness probes. |
| Origin guard on every request | Web pages in your browser driving the proxy cross-site. |
| No CORS unless allowlisted, `*` refused | Any website reading responses from your proxy. |
| `tcp4` or `tcp6` matching the bind address | Listening on addresses the startup output did not list. |
| Fixed port, no fallback | Clients silently talking to whatever else took the port. |
| Model uploads off unless `comfyui_base_dir` is set | Writes into ComfyUI's model folders from the API. |
| Model uploads safetensors-only, allowlisted roots, symlink-checked paths, no overwrite | Planting executable files, path traversal, and clobbering existing models. See [assets](assets.md). |
| No environment HTTP proxy for upstream calls | ComfyUI traffic, which can carry the partner API key, leaking through `HTTP_PROXY`. |
| State directory 0700, database 0600 | Other local users reading the secret that signs output asset ids. See [state and service](state-and-service.md#state-dir). |
