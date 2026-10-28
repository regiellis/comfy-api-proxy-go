---
title: Benchmarks
section: Reference
order: 20
next: troubleshooting
description: Throughput, latency, startup time, and memory of this proxy against the Python reference on one machine, how they were measured, and how to measure your own.
---

## Results {#results}

Measured on 2026-10-28 on one Linux machine (amd64, 24 threads) with nothing else running.
Both proxies sat in front of the same upstream, a Go stand-in for ComfyUI that answers
instantly, so these are each proxy's own cost. Medians of 3 runs; every run started a fresh
proxy process; 0 errors in every run.

| | Go, 8 cores | Go, 1 core | Python reference |
|---|---:|---:|---:|
| `GET /api/v2/health`, 32 concurrent | 356k req/s | 144k req/s | 56k req/s |
| `GET /api/v2/jobs/{id}`, finished job, 16 concurrent | 84k req/s | 35k req/s | 11k req/s |
| `POST /api/v2/jobs`, 16 concurrent | 125k req/s | 31k req/s | 10k req/s |
| 64 MB uploads, 4 at a time | 2003 MB/s | 890 MB/s | 815 MB/s |
| Peak memory during the uploads | 27 MB | 24 MB | 233 MB |
| Ready after start | 6 ms | 6 ms | 80 ms |
| Idle memory | 12 MB | 11 MB | 40 MB |

Latency at those loads:

| | Go, 8 cores p50 / p99 | Go, 1 core p50 / p99 | Reference p50 / p99 |
|---|---:|---:|---:|
| health | 0.04 / 0.97 ms | 0.22 / 0.53 ms | 0.57 / 0.69 ms |
| job read | 0.15 / 0.61 ms | 0.34 / 1.30 ms | 1.44 / 1.56 ms |
| submit | 0.10 / 0.74 ms | 0.36 / 2.53 ms | 1.53 / 1.78 ms |
| 64 MB upload | 77 / 282 ms | 246 / 397 ms | 310 / 348 ms |

## Reading the numbers {#reading}

- **Requests.** The Go proxy uses every core it is given; the reference serves on one event
  loop. "Go, 1 core" is the same binary with `GOMAXPROCS=1`, the fair per-core comparison, and
  it is still 2.6 to 3.2 times the reference.
- **Uploads.** The reference reads each upload into memory. The Go proxy spools it to a temp
  file while hashing, gathering 1 MB at a time, so its memory stays flat however large the
  file. On one core it moves slightly more than the reference, and on 8 cores about 2.5 times
  as much. Its slowest uploads on one core still take longer (p99 397 ms against 348 ms),
  because each one waits on the disk. A version before 0.2.0 hashed in 4 KB pieces and moved
  about half the reference's MB/s on one core.
- **Memory per job.** Both proxies keep job records in memory (the reference's design, kept for
  compatibility). The Go proxy used about 0.9 KB per stored job and the reference about 1.8 KB.
  A long-running proxy under heavy submit load grows by that much per job.
- **Against a real ComfyUI** none of this is the bottleneck: a generation takes seconds, and
  every job read also costs ComfyUI two HTTP calls. With the reference's Python fake ComfyUI as
  the upstream, Go job reads drop to 28k req/s (the fake's limit) against the reference's 11k.
  The proxy's overhead matters most for many clients polling, and for memory on small machines.

## Measure your own {#measure}

From a source checkout with Go, Task, and [uv](https://docs.astral.sh/uv/). The first run fetches
Comfy-Org's Python proxy at its pinned commit (see [Install](install.md#source)):

```sh
task bench                                   # about 4 minutes
task bench -- -runs 5 -duration 10s
task bench -- -upstream ref                  # the Python fake as upstream (no upload runs)
task bench -- -targets go,ref -upload-mb 256 -out .cache/bench/report.md
```

`task bench` pins the proxies to CPUs 0 to 7 with `taskset` and runs the load generator on the
rest. It prints a Markdown table, and `-out` also writes it with a JSON copy beside it. The
flags are in `cmd/proxybench`.

## What is not measured {#not-measured}

- Event streams. Each one holds a ComfyUI WebSocket, and the proxy caps them at 8 concurrent
  streams (`429 too_many_streams` past that), so throughput there is bounded by design.
- Model uploads into `comfyui_base_dir`, which add a safetensors header check and a write into
  the model folder.
- Anything over a network. Both proxies and the load generator ran on one host.
