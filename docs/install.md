---
title: Install
section: Start
order: 20
next: quickstart
description: Download a release binary, install with go install, or build from source. The binary is static and needs no libc, Python, or other runtime.
---

## Requirements {#requirements}

- A ComfyUI the proxy can reach over HTTP. It has been run against ComfyUI 0.41.2. The proxy
  does not need to run on the same machine, but model placement
  ([`comfyui_base_dir`](configuration.md)) needs ComfyUI's folders on a local disk.
- Linux or macOS on amd64 or arm64, or Windows on amd64. Linux is where it is developed and
  tested. The Windows build compiles and follows the Python proxy's Windows behavior, but it has
  not been run on Windows yet. There, `stop` ends the process without a graceful shutdown, as
  the Python proxy does.

## Download a release {#release}

Each release on GitHub carries a static binary for every supported platform, with a
`checksums.txt` beside them.

```sh
VERSION=0.1.0
curl -LO https://github.com/regiellis/comfy-api-proxy-go/releases/download/v${VERSION}/comfy-api-proxy_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/regiellis/comfy-api-proxy-go/releases/download/v${VERSION}/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf comfy-api-proxy_${VERSION}_linux_amd64.tar.gz comfy-api-proxy
sudo install comfy-api-proxy /usr/local/bin/
comfy-api-proxy --version
```

Replace `linux_amd64` with `linux_arm64`, `darwin_amd64`, `darwin_arm64`, or `windows_amd64`
(a `.zip` on Windows).

## Install with Go {#go-install}

With Go 1.26 or newer:

```sh
go install github.com/regiellis/comfy-api-proxy-go/cmd/comfy-api-proxy@latest
```

The binary lands in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` when `GOBIN` is unset. Set
`CGO_ENABLED=0` for a fully static binary; the SQLite driver is pure Go, so nothing needs cgo.

## Build from source {#source}

You need Go 1.26 and [Task](https://taskfile.dev).

```sh
git clone https://github.com/regiellis/comfy-api-proxy-go
cd comfy-api-proxy-go
task build
./bin/comfy-api-proxy --version
```

`task build` sets `CGO_ENABLED=0` and stamps the git version into the binary.

The conformance and end-to-end suites, `task bench`, and the GPU-free fake ComfyUI use
Comfy-Org's Python proxy and Python SDK. They are not part of this repository. The tasks that
need them fetch them on first use with `task ref:init` and `task sdk:ref:init`, which clone
each one at the commit this release was tested against, into `proxy-ref-ts/` and
`comfy-python-sdk/`. Those tasks also need [uv](https://docs.astral.sh/uv/).

## The SDK {#sdk}

The Go SDK is a package in the same module:

```sh
go get github.com/regiellis/comfy-api-proxy-go/sdk
```

See [Go SDK](sdk.md).

## Next {#next}

[Quick start](quickstart.md) runs the proxy in front of ComfyUI and takes one job from submit to
downloaded output.
