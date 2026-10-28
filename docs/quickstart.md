---
title: Quick start
section: Start
order: 30
next: cli
description: Run the proxy in front of ComfyUI, submit a workflow with curl, wait for it, and download the output. About five minutes with ComfyUI already running.
---

## Start ComfyUI and the proxy {#start}

Start ComfyUI as you normally do. It listens on `127.0.0.1:8188` by default, which is also the
proxy's default upstream:

```sh
comfy-api-proxy
```

The startup panel lists the address the proxy serves on, `http://127.0.0.1:8189`, and then one
line per request. If ComfyUI is elsewhere, pass `--comfyui http://host:port`.

Check that the proxy reaches ComfyUI:

```sh
curl -s http://127.0.0.1:8189/api/v2/health
```

```json
{"status":"healthy","upstream":"http://127.0.0.1:8188"}
```

## Export a workflow {#export}

The API takes ComfyUI's **API format**, a map of node ids to `class_type` and `inputs`. In the
ComfyUI editor, open a workflow and use **Export (API)**. The proxy refuses the editor's own
save format with `422 workflow_format_ui`, so use the API export.

Save it as `workflow.json`.

## Submit it {#submit}

The request body wraps the workflow:

```sh
jq '{workflow: .}' workflow.json > job.json
curl -s -X POST http://127.0.0.1:8189/api/v2/jobs \
  -H 'Content-Type: application/json' \
  --data @job.json | tee job-response.json
```

The response is the new job:

```json
{
  "id": "a729ffde-6519-4767-9a6e-b402b213115e",
  "status": "queued",
  "outputs": [],
  "error": null,
  "urls": {
    "self": "http://127.0.0.1:8189/api/v2/jobs/a729ffde-6519-4767-9a6e-b402b213115e",
    "events": "http://127.0.0.1:8189/api/v2/jobs/a729ffde-6519-4767-9a6e-b402b213115e/events",
    "cancel": "http://127.0.0.1:8189/api/v2/jobs/a729ffde-6519-4767-9a6e-b402b213115e/cancel"
  }
}
```

The full response has more fields; [API reference](api.md#submit) lists them.

## Wait for it {#wait}

Poll the job until its status is terminal (`succeeded`, `failed`, or `canceled`):

```sh
JOB=$(jq -r .id job-response.json)
curl -s http://127.0.0.1:8189/api/v2/jobs/$JOB | jq '{status, progress}'
```

Or watch it live. The event stream sends progress, previews, each output, and then the final
status:

```sh
curl -N http://127.0.0.1:8189/api/v2/jobs/$JOB/events
```

[Events](events.md) describes every event.

## Download the output {#download}

A finished job lists its outputs, each with a `url` to its bytes:

```sh
curl -s http://127.0.0.1:8189/api/v2/jobs/$JOB | jq -r '.outputs[] | "\(.name) \(.url)"'
URL=$(curl -s http://127.0.0.1:8189/api/v2/jobs/$JOB | jq -r '.outputs[0].url')
curl -s -o output.png "$URL"
```

Output ids are signed by the proxy and stay valid across restarts; [Assets](assets.md#output-ids)
explains how.

## From Go {#go}

The same steps with the SDK:

```sh
go get github.com/regiellis/comfy-api-proxy-go/sdk
```

[Go SDK](sdk.md) walks through creating a client, submitting, waiting on events, and
downloading outputs. The same program runs against Comfy Cloud by changing the base URL and
adding an API key ([Comfy Cloud and Router](sdk-cloud.md)).

## Try it without a GPU {#no-gpu}

Comfy-Org's Python proxy ships a GPU-free ComfyUI stand-in, and a source checkout can fetch it
for you. With [uv](https://docs.astral.sh/uv/) and Task installed:

```sh
git clone https://github.com/regiellis/comfy-api-proxy-go
cd comfy-api-proxy-go
task ref:fake-comfyui &      # fetches the Python proxy once, then a fake ComfyUI on :8188
task run
```

Every job then succeeds with one tiny PNG, enough to try the API and the SDK.

## Next {#next}

- [Command line](cli.md) and [Configuration](configuration.md) for the options.
- [Remote access](remote-access.md) to reach the proxy from another machine.
