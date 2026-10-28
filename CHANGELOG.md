# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version may
carry breaking changes, marked **BREAKING CHANGE**. A patch version never does.

## [Unreleased]

## [0.1.1] - 2026-10-28

### Fixed
- The SDK's documentation shows on pkg.go.dev. `LICENSE` is now the plain MIT text with both
  copyright lines, and the attribution to Comfy Org's projects moved to `NOTICE`. The extra
  paragraph had kept pkg.go.dev from recognizing the license, so it hid v0.1.0's docs.
- A binary built with `go install` reports its module version in `--version` and the startup
  panel, where it printed `dev`.

## [0.1.0] - 2026-10-27

The first release: a Go port of Comfy-Org's Python `comfy-api-proxy` and a Go client for the
Comfy API v2. An independent project, not affiliated with or endorsed by Comfy Org.

### Added
- `comfy-api-proxy`, one static binary serving the Comfy API v2 (`/api/v2/*`) in front of a
  self-hosted ComfyUI. It submits, reads, lists, and cancels jobs, streams each job's events as
  Server-Sent Events bridged from ComfyUI's WebSocket, uploads and downloads assets with blake3
  dedup, signs output asset ids the way the Python proxy signs them, and places model files
  under `--comfyui-base-dir` behind guards. The Python proxy's black-box HTTP suite passes
  against it, 82 of 82.
- Release binaries for Linux and macOS (amd64, arm64) and Windows (amd64). The background
  service commands build and follow the reference on Windows but are untested there.
- The CLI: `run`, `start`, `stop`, `status`, and `config`, with the Python proxy's flags
  accepted unchanged. Configuration from flags, `COMFY_PROXY_*` environment variables, or a TOML
  file, with every value's source reported.
- Bind scopes (`local`, `global`, `localhost`, or a literal IP), a bearer token required for any
  non-loopback bind, the browser origin guard, and per-origin CORS.
- `--state-dir`: jobs, idempotency keys, and assets in SQLite across restarts, compatible with a
  state directory written by the Python proxy.
- `comfyui_token`, a bearer token sent to a ComfyUI behind an authenticating gateway.
- `sdk`, a Go client for the Comfy API v2 and Comfy Router, ported from the Python `comfy-sdk`:
  jobs, events, assets, typed errors, retries, and `WithProxyMetadata` for job labels against
  this proxy.
- `task bench`, which measures this proxy against the Python reference.

### Fixed (relative to the Python proxy)
- A job that finishes between the proxy's history read and its queue read no longer reads as
  `expired`.
- An event stream no longer hangs when the terminal message arrives before the stream's
  WebSocket connects, or under constant broadcast frames from ComfyUI and custom nodes.
- Outputs always arrive on the event stream before the terminal status.
- Uploads spool to disk while hashing instead of being read into memory.
- Asset expiry persists in the state database, and expired assets are cleaned up with or
  without a state directory.
- `expires_in` past year 9999 is a 422, not a 500.

### Security
- `golang.org/x/text` 0.43.0, for GO-2026-5970 (an infinite loop on invalid input, reachable
  through the access log's terminal styling). `task security` runs `govulncheck` over the proxy
  and the docs site, and CI runs it on every push.

### Fixed (relative to the Python proxy, from its open issues)
- `public_url` (`--public-url`, `COMFY_PROXY_PUBLIC_URL`) sets the origin of every link in a
  response, so a proxy behind a TLS-terminating reverse proxy returns `https://` links
  (Comfy-Org/comfy-api-proxy#31).
- Outputs report their real `size_bytes`, read from ComfyUI's `/view` once per output and
  cached, on the job and on `GET /api/v2/assets/{id}` (Comfy-Org/comfy-api-proxy#26).

### Fixed (during the port)
- A CORS origin written in mixed case now matches, as in the Python proxy: the scheme and host
  are lowercased.

[Unreleased]: https://github.com/regiellis/comfy-api-proxy-go/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/regiellis/comfy-api-proxy-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/regiellis/comfy-api-proxy-go/releases/tag/v0.1.0
