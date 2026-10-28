---
title: Introduction
section: Start
order: 10
next: install
description: comfy-api-proxy-go serves the Comfy API v2 in front of your own ComfyUI as one static Go binary, and ships a Go SDK for the same API, so code written for Comfy Cloud runs against a local GPU unchanged.
---

## What it is {#what}

ComfyUI has two APIs. Its native one (`/prompt`, `/history`, `/queue`, `/ws`) is what the
ComfyUI frontend uses. The Comfy API v2 (`/api/v2/*`) is the one Comfy Cloud serves: jobs with
ids and states, an event stream, assets with ids and hashes, and a JSON error envelope. The
official SDKs are written against v2.

`comfy-api-proxy` sits between the two. It answers v2 requests and turns each one into calls on
a ComfyUI you run yourself. A client that works against Comfy Cloud works against your machine
by changing its base URL.

This repository is a Go port of Comfy-Org's Python
[`comfy-api-proxy`](https://github.com/Comfy-Org/comfy-api-proxy), plus a Go client for the
same API:

- `comfy-api-proxy`, one static binary with no runtime dependencies. It speaks the reference's
  wire contract: the reference's own black-box HTTP suite passes against it, 82 of 82.
- `sdk`, a Go package for the v2 API and for Comfy Router, ported from the Python
  [`comfy-sdk`](https://github.com/Comfy-Org/comfy-python-sdk). The same client talks to this
  proxy, to the Python proxy, or to Comfy Cloud.

## Why it exists {#why}

ComfyUI has no official Go SDK and no Go proxy. A Go application that wants to drive ComfyUI
has two options without one: run a Python service beside it, or write HTTP and WebSocket code
against ComfyUI's native API, which has no job ids of its own, no stable output ids, and no
error envelope.

This project is for Go developers who would rather stay in Go. The proxy deploys as a single
file (copy it to the GPU box and run it), and the SDK gives a Go program the same job model
Comfy Cloud's clients use. Code written against the SDK moves between a local ComfyUI and Comfy
Cloud without changes.

## What you get {#features}

- **Jobs.** Submit a workflow, poll it, list jobs with filters, cancel. Status reads ComfyUI's
  history and queue on every call, so it is never stale.
- **Live events.** A Server-Sent Events stream per job, bridged from ComfyUI's WebSocket:
  progress, previews, outputs, then the terminal status.
- **Assets.** Uploads stream to disk while they are hashed (blake3), duplicates are found by
  hash, and model files can be placed into ComfyUI's model folders behind strict guards.
- **Safe defaults.** Binds loopback only. A non-loopback bind needs a bearer token. A
  browser-origin guard blocks cross-site requests, and CORS is opt-in per origin.
- **Operations.** Configuration from flags, environment, or a TOML file, a `config` command
  that says where each value came from, background `start`/`stop`/`status`, and optional SQLite
  state that survives restarts.

## How it compares to the Python proxy {#compared}

The wire contract is the same, and so is the security model. The differences are in the binary
and a handful of fixes found against a real ComfyUI; [Differences](differences.md) lists each.
On one machine, the Go proxy served 6 to 12 times the reference's requests per second with less
memory, and held 64 MB uploads in 19 MB where the reference peaked at 298 MB.
[Benchmarks](benchmarks.md) has the method and the full table, including where Go is slower.

## Not an official project {#unofficial}

comfy-api-proxy-go is an independent, community Go port of Comfy-Org's `comfy-api-proxy`. It is
not an official ComfyUI or Comfy Org project, and it is not affiliated with or endorsed by Comfy
Org. Comfy and ComfyUI are trademarks of Comfy Org. Problems with this port belong in
[its issue tracker](https://github.com/regiellis/comfy-api-proxy-go/issues), not Comfy Org's.

## Where to go next {#next}

- [Install](install.md) the binary.
- [Quick start](quickstart.md): run it in front of ComfyUI and finish a job with curl.
- [Go SDK](sdk.md): drive it from Go.
