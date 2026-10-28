# comfy-api-proxy-go

[![Go Reference](https://pkg.go.dev/badge/github.com/regiellis/comfy-api-proxy-go.svg)](https://pkg.go.dev/github.com/regiellis/comfy-api-proxy-go/sdk)
[![CI](https://github.com/regiellis/comfy-api-proxy-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/regiellis/comfy-api-proxy-go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/regiellis/comfy-api-proxy-go)](https://github.com/regiellis/comfy-api-proxy-go/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**The Comfy API v2 in front of your own ComfyUI, as one Go binary, plus a Go SDK for it.**

Code written for Comfy Cloud runs against a ComfyUI on your own GPU by changing its base URL.
The proxy turns each v2 call (jobs, live events, assets) into calls on ComfyUI's native API. It
is a Go port of Comfy-Org's Python [`comfy-api-proxy`](https://github.com/Comfy-Org/comfy-api-proxy),
and that project's own HTTP test suite passes against it, 82 of 82.

> [!IMPORTANT]
> Independent project. Not an official ComfyUI or Comfy Org project, and not affiliated with or
> endorsed by Comfy Org. Comfy and ComfyUI are trademarks of Comfy Org.

Documentation: **[comfy-proxy.regiellis.com](https://comfy-proxy.regiellis.com/docs)**, with
search and an `llms.txt`. The source is in [`docs/`](docs/).

## Why

ComfyUI has no official Go SDK and no Go proxy. A Go program that drives ComfyUI otherwise runs
a Python service beside it, or speaks ComfyUI's native HTTP and WebSocket API directly, which
has no job ids, no stable output ids, and no error envelope. This project is for Go developers
who would rather stay in Go: the proxy deploys as a single static file, and the SDK gives a Go
program the job model Comfy Cloud's clients use, against a local ComfyUI or Comfy Cloud alike.

## Install

Download a binary for Linux, macOS, or Windows from
[Releases](https://github.com/regiellis/comfy-api-proxy-go/releases), or:

```sh
go install github.com/regiellis/comfy-api-proxy-go/cmd/comfy-api-proxy@latest
```

The binary is static (no libc, no Python). See [Install](docs/install.md).

## Quick start

With ComfyUI on its default `127.0.0.1:8188`:
```sh
comfy-api-proxy                                   # serves http://127.0.0.1:8189
curl -s http://127.0.0.1:8189/api/v2/health
jq '{workflow: .}' workflow.json | curl -s -X POST http://127.0.0.1:8189/api/v2/jobs \
  -H 'Content-Type: application/json' --data @-
```

`workflow.json` is ComfyUI's **Export (API)**. [Quick start](docs/quickstart.md) follows the
job to its downloaded output.

## From Go

```go
// wf is ComfyUI's Export (API), loaded with sdk.WorkflowFromJSON.
client, err := sdk.New(sdk.WithBaseURL("http://127.0.0.1:8189"))
if err != nil {
	log.Fatal(err)
}
job, err := client.Run(ctx, wf, sdk.SubmitOptions{}) // submit and wait
if err != nil {
	log.Fatal(err)
}
for _, out := range job.Outputs() {
	if err := out.ToFile(ctx, out.Name(), nil); err != nil {
		log.Fatal(err)
	}
}
```

The same client talks to Comfy Cloud and Comfy Router. See [Go SDK](docs/sdk.md) and
[Comfy Cloud and Router](docs/sdk-cloud.md).

## What it does

- **Jobs and live events:** submit, read, list, and cancel, with a Server-Sent Events stream per
  job bridged from ComfyUI's WebSocket.
- **Assets:** uploads stream to disk while hashed, deduplicated by hash, with guarded model
  placement into ComfyUI's folders.
- **Safe by default:** loopback only, a token for any other bind, cross-site browser requests
  blocked. See [Remote access](docs/remote-access.md).
- **Operations:** flags, environment, or a TOML file, a background `start`/`stop`/`status`, and
  optional SQLite state.

## Compared with the Python proxy

Same wire contract and security model. On one machine, the Go proxy served 6 to 12 times the
requests per second, started in 6 ms against 80 ms, moved 64 MB uploads 2.5 times as fast, and
held them in 27 MB of memory where the Python proxy peaked at 233 MB.
[Benchmarks](docs/benchmarks.md) has the method and every number, and
[Differences](docs/differences.md) lists each behavior change and why.

## Development

Go 1.26 and [Task](https://taskfile.dev). The conformance and e2e suites also need
[uv](https://docs.astral.sh/uv/). They fetch Comfy-Org's Python proxy and SDK at pinned commits.

```sh
task build          # bin/comfy-api-proxy
task check          # lint, Go tests, the Python proxy's suite against this binary, e2e
task bench          # this proxy against the Python reference
task site           # the docs site on this machine
```

## License

[MIT](LICENSE). The Python proxy and Python SDK it ports are Comfy-Org's, also MIT-licensed.
