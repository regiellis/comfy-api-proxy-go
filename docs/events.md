---
title: Event stream
section: API
order: 20
next: assets
description: The Server-Sent Events stream at /api/v2/jobs/{id}/events. Covers its event names and payloads, how it bridges ComfyUI's WebSocket, and why it always ends.
---

## The endpoint {#endpoint}

`GET /api/v2/jobs/{id}/events` streams one job's live state as Server-Sent Events. It is a convenience on top of polling. `GET /api/v2/jobs/{id}` stays authoritative, and a client that never opens the stream sees the same state by polling it.

The stream is live push only. Events carry no `id:` line, there is no `Last-Event-ID` resume, and anything sent while a client was disconnected is gone. A client that reconnects gets a fresh snapshot.

```sh
curl -sN http://127.0.0.1:8189/api/v2/jobs/813f058f-80fc-4023-82bf-8c20fe3190ca/events
```

```http
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

| Status | Code | When |
|---|---|---|
| 200 | | The stream opens. |
| 404 | `not_found` | The id is not a UUID, or neither the proxy nor ComfyUI knows the job. |
| 429 | `too_many_streams` | Eight streams are already open. Sent with `Retry-After: 5`. Poll the job instead, or retry. |
| 503 | `upstream_unreachable` | ComfyUI could not be reached for the first snapshot. |

The cap is eight concurrent streams per proxy, because each holds a ComfyUI WebSocket open. A slot frees when its stream ends.

A sample run of a short job:

```text
event: status
data: {"status":"running"}

event: progress
data: {"value":0,"nodes_done":0,"nodes_total":0}

event: progress
data: {"value":0.5,"nodes_done":0,"nodes_total":0,"current_node":"3","step":1,"steps":2}

event: preview
data: {"node_id":"","content_type":"image/png","data_base64":"iVBORw0KGgo..."}

event: output
data: {"node_id":"9","name":"out.png","type":"image","content_type":"image/png","size_bytes":1553743,"id":"eyJq...","hash":null,"url":"http://127.0.0.1:8189/api/v2/assets/eyJq.../content","url_expires_at":"2026-10-29T01:03:01.728635+00:00","job_id":"813f058f-80fc-4023-82bf-8c20fe3190ca"}

event: status
data: {"status":"succeeded"}
```

## Events {#events}

| Event | Payload | Sent |
|---|---|---|
| `status` | `{"status", "queue_position"}` | First on connect, and last when the job ends. |
| `progress` | A progress snapshot | On connect when the job is running, then live, at most about twice a second. |
| `preview` | `{"node_id", "content_type", "data_base64"}` | Live preview images, at most about twice a second. |
| `output` | An output object, the same shape as in `job.outputs` | As each node's outputs are committed, and before the terminal status. |

No `log` event is sent, because the v2 contract reserves it. A node execution error shows up as a terminal `failed` status, and the details are in the job's `error` field.

### status {#status}

```json
{"status": "queued", "queue_position": 2}
```

`status` is one of the job statuses on [the API page](api.md#job-fields). `queue_position` is present only while the job is queued. The terminal statuses are `succeeded`, `failed`, `canceled`, and `expired`. The terminal `status` is always the last event on the stream.

### progress {#progress}

Every `progress` event is a complete snapshot, so one event re-syncs a client that missed earlier ones.

| Field | Notes |
|---|---|
| `value` | Overall fraction, 0 to 1. |
| `nodes_done`, `nodes_total` | Finished and total nodes, from ComfyUI's `progress_state`. Both are 0 when the frame came from ComfyUI's per-step `progress` message. |
| `current_node` | The running node id, or `null`. |
| `step`, `steps` | The running node's step and step count, or `null`. |

The snapshot sent on connect comes from the job object and carries only `value`, `nodes_done`, and `nodes_total`.

### preview {#preview}

```json
{"node_id": "12", "content_type": "image/jpeg", "data_base64": "/9j/4AAQ..."}
```

`data_base64` is the encoded image, standard base64. `content_type` is `image/jpeg` unless ComfyUI marked the frame as PNG or named another type. `node_id` is the node that produced the preview when ComfyUI tags it, and an empty string when it does not. The proxy keeps no preview to replay on connect.

### output {#output}

The payload is the output object from [the API page](api.md#output-fields). Each output id is sent at most once per stream, and every output that has not been sent yet goes out before the terminal `status`. Clients, both SDKs included, stop reading at a terminal status, so an output sent after it would never be seen.

## How it bridges ComfyUI {#bridge}

ComfyUI's WebSocket at `/ws` is its only live signal, and ComfyUI sends a prompt's events only to the client that submitted it. The proxy submits each job with the job id as its `client_id`, so the stream for a job connects to `/ws?clientId=<job id>` and receives that job's frames.

On connect the stream:

1. Takes a snapshot from ComfyUI's `/history` and `/queue`. A job that is already terminal gets its outputs and its terminal `status`, and the stream ends.
2. Sends the `status` event, and a `progress` event when the job is running.
3. Opens the WebSocket and sends ComfyUI's `feature_flags` handshake with `supports_preview_metadata`, so previews arrive tagged with their node id.
4. Takes a second snapshot now that frames are flowing. A job that finished between the first snapshot and the connect sent its terminal message to nobody, and this check catches it.
5. Translates frames until the job ends.

Frame translation:

| ComfyUI frame | Stream |
|---|---|
| `progress`, `progress_state` | A `progress` event, throttled to one per 500 ms. |
| binary preview | A `preview` event, throttled to one per 500 ms. |
| `executed` | A fresh snapshot, and an `output` event for each output not yet sent. Coalesced to one snapshot per 500 ms. |
| `execution_success`, `execution_error`, `execution_interrupted` | A final snapshot, then remaining outputs, then the terminal `status`. |

Frames that name another prompt are ignored. Frames with no `prompt_id` apply to every client.

## Reconcile and heartbeats {#reconcile}

Every 15 seconds the stream sends a comment line and takes a fresh snapshot. A terminal snapshot ends the stream the same way a terminal frame would.

```text
: keepalive
```

The schedule is fixed, and incoming frames never reset it. A real ComfyUI broadcasts frames to every WebSocket, its own `status` and custom nodes such as crystools' monitor about twice a second. A timer reset by each frame would never fire, and a job whose terminal message was missed would leave the stream open forever. With the fixed schedule, a missed terminal message costs at most one interval.

The snapshot right after connecting and the fixed reconcile together are why a stream always reaches a terminal status while ComfyUI is reachable.

## When the WebSocket fails {#fallback}

If ComfyUI's WebSocket cannot be opened, or closes or breaks mid-stream, the stream switches to polling. It takes a snapshot every second, sends a `: keepalive` comment after each, and ends with the remaining outputs and the terminal `status` once the job is terminal.

Polling gives up after 10 minutes and closes the stream without a terminal status. A client that sees the stream end without one should poll `GET /api/v2/jobs/{id}`.

If ComfyUI becomes unreachable after the stream opened, the stream ends. The status line is already sent, so it cannot become an error. Polling reports the problem with a proper `503 upstream_unreachable`.

## Shutdown {#shutdown}

Open streams never go idle, so they would hold a graceful shutdown open. When the proxy stops, every open stream is closed at once, without a terminal event. Treat a closed stream with no terminal status the same way in every case: poll the job.

## A minimal client {#client}

The loop below reads events with the Go standard library and falls back to polling when the stream ends without a terminal status.

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

var terminal = map[string]bool{"succeeded": true, "failed": true, "canceled": true, "expired": true}

func main() {
	base, id := os.Args[1], os.Args[2] // http://127.0.0.1:8189 and a job id
	resp, err := http.Get(base + "/api/v2/jobs/" + id + "/events")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "stream refused:", resp.Status, "; poll instead")
		os.Exit(1)
	}

	event, done := "", false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20) // previews can be large
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "status":
				var s struct{ Status string }
				if err := json.Unmarshal([]byte(data), &s); err == nil {
					fmt.Println("status:", s.Status)
					done = terminal[s.Status]
				}
			case "output":
				var o struct{ Name, URL string }
				if err := json.Unmarshal([]byte(data), &o); err == nil {
					fmt.Println("output:", o.Name, o.URL)
				}
			case "progress":
				fmt.Println("progress:", data)
			}
		}
		if done {
			return
		}
	}
	fmt.Println("stream ended without a terminal status; poll", base+"/api/v2/jobs/"+id)
}
```

The [Go SDK](sdk.md) does this for you: `Job.Events` reconnects when the stream drops and falls back to polling, so it always ends on a terminal status.
