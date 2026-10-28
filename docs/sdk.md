---
title: Go SDK
section: SDK
order: 10
next: sdk-cloud
description: How to use the Go SDK against a local comfy-api-proxy, from creating a client through submitting workflows, waiting on jobs, downloading outputs, and handling errors. The same code runs against Comfy Cloud with a different base URL and key.
---

The package `github.com/regiellis/comfy-api-proxy-go/sdk` is a Go client for the Comfy API v2.
It talks to this proxy, to Comfy Cloud, or to any other host that serves the same contract, so
an app written against the local proxy moves to Comfy Cloud by changing its base URL and key.
It is a port of the Python `comfy-sdk`, and its calls take a `context.Context` where the Python
SDK has sync and async twins. This page uses the local proxy throughout. [Comfy Cloud and
Router](sdk-cloud.md) covers what changes for the hosted service.

## Install {#install}

The module needs Go 1.26 or newer.

```sh
go get github.com/regiellis/comfy-api-proxy-go/sdk
```

```go
import "github.com/regiellis/comfy-api-proxy-go/sdk"
```

The proxy must be running and reachable. See [Quickstart](quickstart.md) for starting it.

## Create a client {#client}

`sdk.New` takes functional options and checks them before any request is sent.

```go
client, err := sdk.New(
	sdk.WithBaseURL("http://127.0.0.1:8189"),
	sdk.WithClientInfo("my-app/1.0"),
)
if err != nil {
	return err
}
```

The base URL is resolved in this order:

1. `sdk.WithBaseURL(url)`
2. the `COMFY_BASE_URL` environment variable
3. `sdk.CloudBaseURL` (`https://cloud.comfy.org`)

The API key is resolved the same way: `sdk.WithAPIKey(key)`, then `COMFY_API_KEY`. A blank or
whitespace key counts as unset at both sources. With no key the client sends no
`Authorization` header, which is what a proxy bound to `127.0.0.1` without a token expects. A
proxy started with a token (required for any non-loopback bind, see [Remote
access](remote-access.md)) needs that token as the key:

```go
client, err := sdk.New(
	sdk.WithBaseURL("http://192.168.1.20:8189"),
	sdk.WithAPIKey(os.Getenv("PROXY_TOKEN")),
)
```

The base URL is the host root, without `/api/v2`; the client adds that prefix. It must be an
`http` or `https` URL with no query or fragment, and `New` returns an error otherwise.

| Option | Effect |
| --- | --- |
| `WithBaseURL(u string)` | Target deployment. Wins over `COMFY_BASE_URL`. |
| `WithAPIKey(key string)` | Bearer token. Wins over `COMFY_API_KEY`. |
| `WithProxyMetadata()` | Sends job labels the way comfy-api-proxy stores them. See [Labels](#metadata). |
| `WithTimeout(d time.Duration)` | Connect, response-header, and idle-body limit. Default 30 s. See [Timeouts](#timeouts). |
| `WithClientInfo(info string)` | Appends `app/<info>` to the User-Agent. |
| `WithHTTPClient(c *http.Client)` | Replaces the HTTP client and its timeouts. |
| `WithRouterBaseURL(u string)` | Comfy Router target. See [Comfy Cloud and Router](sdk-cloud.md). |
| `WithRetry(p sdk.RetryPolicy)` | Retry policy for Router calls only. |

The bearer token is sent only to the base URL's origin (scheme, host, and port) and to the
Comfy Router origin. It is removed on any redirect that leaves them. `client.String()` and
`client.BaseURL()` never print the key, and redact credentials embedded in the URL.

## Workflows {#workflows}

A `*sdk.Workflow` holds an API-format graph: a map of node id to `{class_type, inputs}`. Export
it from ComfyUI with "Export (API)". The editor's normal save format (with `nodes`, `links`,
and `last_node_id`) is refused by `Submit` with a `workflow_format_ui` error before anything is
sent.

```go
wf, err := sdk.WorkflowFromFile("workflow_api.json")
if err != nil {
	return err
}
wf.SetInput("3", "seed", int64(1234567890123))
wf.SetInput("6", "text", "a lighthouse at dusk")
wf.RemoveNode("12") // drop a preview node; links through it are rewired
```

`sdk.WorkflowFromJSON(data)` parses bytes, and `sdk.NewWorkflow(graph)` wraps a map you built.
`SetInput` creates the node and its `inputs` map when they are missing. `RemoveNode` rewires
links that pointed at the removed node to its single linked input, when it had exactly one, and
drops them otherwise.

`WorkflowFromJSON` and `WorkflowFromFile` decode numbers as `json.Number`, so a 64-bit seed such
as `18446744073709551615` reaches the server exactly. Decoding the same file with a plain
`json.Unmarshal` into `map[string]any` turns it into a `float64` and changes the seed.

### Input files {#assets}

Pass an `*sdk.Asset` as an input value and `Submit` uploads it, then replaces it in a copy of
the graph with a `core/ASSET` reference. Your workflow is left unchanged.

```go
wf.SetInput("10", "image", client.AssetFromFile("photo.png"))
```

Assets are lazy. Nothing is read until submit, when the SDK hashes the bytes with blake3, asks
the server whether it already holds them (`HEAD /assets/by-hash/{hash}`), and uploads only when
it does not. `client.AssetFromBytes(data, filename, contentType)`,
`client.AssetFromReader(r, filename, contentType)`, and `client.AssetFromURL(ctx, url)` build
assets from other sources. `asset.Commit(ctx)` uploads ahead of time and returns the asset id;
`asset.CreatedNew()` then reports whether new bytes were stored.

## Submit a job {#submit}

`client.Submit` sends the workflow and returns as soon as the job is queued.

```go
job, err := client.Submit(ctx, wf, sdk.SubmitOptions{})
if err != nil {
	return err
}
fmt.Println(job.ID(), job.Status()) // e.g. "3f1c..." queued
```

`sdk.SubmitOptions` fields, all optional:

| Field | Meaning |
| --- | --- |
| `IdempotencyKey string` | Key for this submission. Minted (a random UUID) when empty. |
| `Metadata map[string]string` | Job labels. See [Labels](#metadata). |
| `PartnerAPIKey string` | Key for partner (API) nodes in the graph, sent as `extra_data.api_key_comfy_org`. Unrelated to the client's own key. |

### Idempotency keys {#idempotency}

Every submit carries an `Idempotency-Key` header. Keys are single-use: the server rejects a
second submit under the same key with HTTP 422 `idempotency_key_reuse`. It does not replay the
first response. A key you supply must be 1 to 255 printable ASCII characters; the SDK checks
that locally with `sdk.ValidateIdempotencyKey` before sending.

Supply your own key when a submit must not run twice, for example one derived from an order
id. When a submit fails without a clear answer (a timeout, a dropped connection), the job may
or may not exist. Every error a submit returns carries the key it was sent under, so you can
look the job up instead of guessing:

```go
job, err := client.Submit(ctx, wf, sdk.SubmitOptions{IdempotencyKey: "order-1234"})
var apiErr *sdk.APIError
var tErr *sdk.TransportError
switch {
case errors.Is(err, sdk.ErrIdempotencyKeyReuse):
	// Already submitted under this key. Find the job rather than resubmitting.
case errors.As(err, &tErr):
	log.Printf("unknown outcome for key %s: %v", tErr.IdempotencyKey, err)
case errors.As(err, &apiErr):
	log.Printf("submit refused (%s), key %s", apiErr.Code, apiErr.IdempotencyKey)
}
```

The proxy releases a key when ComfyUI definitely created no job, so a key whose submit ComfyUI
rejected can be used again. On an unknown outcome it keeps the key held.

### Labels and `WithProxyMetadata` {#metadata}

`SubmitOptions.Metadata` attaches string labels to a job. The SDK contract carries them as a JSON
object, while comfy-api-proxy stores metadata as one opaque string and answers an object with
HTTP 422 `invalid_request`. Create the client with `sdk.WithProxyMetadata()` when targeting the
proxy:

```go
client, err := sdk.New(
	sdk.WithBaseURL("http://127.0.0.1:8189"),
	sdk.WithProxyMetadata(),
)
if err != nil {
	return err
}
job, err := client.Submit(ctx, wf, sdk.SubmitOptions{
	Metadata: map[string]string{"project": "demo", "shot": "12"},
})
if err != nil {
	return err
}
fmt.Println(job.Metadata()) // map[project:demo shot:12]
```

With the option, the map is sent as its JSON encoding (`"{\"project\":\"demo\",...}"`), and the
proxy's 1024-byte metadata limit applies to that encoded string. `job.Metadata()` and
`JobSummary.Metadata` decode labels from either an object or a JSON-encoded object, and ignore
opaque text and non-string values. Leave the option off for any deployment that accepts metadata
objects. Comfy Cloud does not take labels at all yet. See
[Comfy Cloud and Router](sdk-cloud.md#workflows).

## Wait for the result {#wait}

There are two ways to wait: polling, and the live event stream.

### Polling {#polling}

`client.Run` is `Submit` followed by `job.Result`:

```go
ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
defer cancel()

job, err := client.Run(ctx, wf, sdk.SubmitOptions{})
var failed *sdk.JobFailedError
if errors.As(err, &failed) {
	if failed.Err != nil {
		return fmt.Errorf("job %s %s: %s", failed.JobID, failed.Status, failed.Err.Message)
	}
	return err
}
if err != nil {
	return err
}
```

For a job you already hold, `job.Wait(ctx)` polls until the status is terminal and
`job.Result(ctx)` also turns a non-success into `*sdk.JobFailedError`. Polls start at half a
second and grow to one every five seconds. Neither has a built-in deadline, so bound them with
the context. `job.Refresh(ctx)` reads the job once; `client.GetJob(ctx, id)` fetches one by id.

Statuses are `queued`, `running`, `canceling`, `succeeded`, `failed`, `canceled`, and `expired`,
available as `sdk.Status*` constants. `sdk.IsTerminal(status)` is true for the last four.
Between polls, `job.Progress()`, `job.QueuePosition()`, and `job.Failure()` return copies of the
last snapshot.

### Events {#events}

`job.Events(ctx)` streams progress, previews, outputs, and status changes as they happen. It is
a Go iterator:

```go
for ev, err := range job.Events(ctx) {
	if err != nil {
		return err
	}
	switch e := ev.(type) {
	case *sdk.StatusChange:
		fmt.Println("status", e.Status)
	case *sdk.Progress:
		fmt.Printf("progress %.0f%%\n", e.Value*100)
	case *sdk.Preview:
		fmt.Println("preview", e.ContentType, len(e.Data))
	case *sdk.OutputReady:
		fmt.Println("output", e.Output.Name())
	case *sdk.Log:
		fmt.Println(e.Level, e.Message)
	}
}
if err := job.Result(ctx); err != nil { // reads final state, outputs included
	return err
}
```

The stream is backed by polling. A dropped connection reconnects; a stream that closes without
a terminal status falls back to reading the job; a server without the endpoint (HTTP 501) ends
the loop quietly. Unless an error ends it, the last event is always a terminal
`*sdk.StatusChange`. Treat `OutputReady` as an early notice and read the full list from
`job.Outputs()` after the loop, since a deployment may not send an event for every output. See
[Events](events.md) for the wire format.

## Read outputs {#outputs}

`job.Outputs()` lists every output of the job; `job.GetOutputs(nodeID)` lists one node's.
Each `*sdk.Output` carries `NodeID()`, `Name()`, `Type()` (image, video, audio, text, file, or
latent), `ContentType()`, `SizeBytes()`, and `ID()`.

```go
for i, o := range job.GetOutputs("9") {
	dest := filepath.Join("out", fmt.Sprintf("%d_%s", i, filepath.Base(o.Name())))
	if err := o.ToFile(ctx, dest, nil); err != nil {
		return err
	}
}
```

| Method | Effect |
| --- | --- |
| `ToFile(ctx, path, rng)` | Streams to a file, creating or truncating it. A failed download removes the partial file. |
| `ToWriter(ctx, w, rng)` | Streams into an `io.Writer` and returns the byte count. |
| `Bytes(ctx, rng)` | Reads into memory. |
| `DownloadURL(ctx)` | Resolves a URL for the bytes without downloading them. |

Pass `nil` for the whole file, or a `*sdk.Range` for an inclusive byte range:

```go
head, err := o.Bytes(ctx, &sdk.Range{Start: 0, End: 1023})
```

On the proxy, `DownloadURL` returns the content endpoint itself, which needs the same bearer
token as any other call. On Comfy Cloud it returns a signed URL and its expiry. Output asset
ids from the proxy are signed and stateless; see [Assets](assets.md).

## List jobs {#list}

`client.ListJobs` walks jobs newest first and yields `sdk.JobSummary` values. Break out of the
loop to stop early.

```go
for s, err := range client.ListJobs(ctx, sdk.ListOptions{Limit: 20}) {
	if err != nil {
		return err
	}
	fmt.Println(s.ID, s.Status, s.Metadata)
}
```

`ListOptions.Metadata` keeps only jobs carrying every given label. On the proxy the list is one
page: `Limit` is between 1 and 100 (default 50 when zero) and the proxy sends no cursor. With
`WithProxyMetadata`, label filtering runs in the SDK over that page, so a filter sees at most
`Limit` of the most recent jobs, not every job the proxy knows.

## Cancel {#cancel}

```go
if err := job.Cancel(ctx); err != nil {
	return err
}
if err := job.Wait(ctx); err != nil {
	return err
}
fmt.Println(job.Status()) // canceled, or succeeded if it finished first
```

A running job reads `canceling` until ComfyUI's interrupt lands at the next node boundary,
which can take seconds, so wait for the terminal status rather than reading it from the cancel
reply. After a
cancel, `job.Result` returns a `*sdk.JobFailedError` with `Status` `canceled`.

## Errors {#errors}

Every failure the API reports is an `*sdk.APIError`:

| Field | Meaning |
| --- | --- |
| `Code` | The wire error code, such as `not_found`, or `http_<status>` when the server named none the SDK knows. |
| `Message` | The server's message. |
| `HTTPStatus` | The response status. Zero for a check the SDK made before sending. |
| `Details` | The envelope's `details` object, when present. |
| `RequestID` | The `X-Comfy-Request-Id` header, cleaned for logging. |
| `RetryAfter` | `Retry-After` in seconds, `nil` when absent. |
| `IdempotencyKey` | The key a failed submit used. |

Match a kind of failure with `errors.Is` against a sentinel, and read the fields with
`errors.As`:

```go
_, err := client.GetJob(ctx, id)
switch {
case errors.Is(err, sdk.ErrNotFound):
	// unknown job id
case errors.Is(err, sdk.ErrUnauthorized):
	// missing or wrong token
case err != nil:
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		log.Printf("%d %s: %s (request %s)", apiErr.HTTPStatus, apiErr.Code, apiErr.Message, apiErr.RequestID)
	}
	return err
}
```

Sentinels for the v2 API: `ErrNotFound`, `ErrUnauthorized`, `ErrForbidden`,
`ErrInvalidWorkflow`, `ErrWorkflowFormatUI` (also matches `ErrInvalidWorkflow`),
`ErrMissingAsset`, `ErrHashMismatch`, `ErrBlobNotFound`, `ErrIdempotencyKeyReuse`,
`ErrQueueFull`, `ErrInsufficientCredits`, `ErrMissingAPIKey`, and `ErrInvalidResponse`. A code
with no sentinel (the proxy's `invalid_request`, `upstream_unreachable`, and others listed in
[API](api.md)) still arrives as an `*APIError`; compare its `Code`.

Two other error types:

- `*sdk.TransportError` is a failure that produced no response: a refused connection, a
  timeout, a stream cut mid-body. Whether the server acted on the request is unknown. It
  unwraps to the cause, so `errors.Is(err, context.DeadlineExceeded)` and
  `errors.Is(err, sdk.ErrIdleTimeout)` work through it.
- `*sdk.JobFailedError` reports a job that ended `failed`, `canceled`, or `expired`.
  `errors.Is(err, sdk.ErrJobFailed)` matches it, and its `Err` field holds the server's
  `*sdk.JobError` (code, message, node id, class type, traceback) when one was given.

## Retries and timeouts {#timeouts}

`Submit` and `ListJobs` retry an HTTP 429 that carries `Retry-After`, or a 429 with the legacy
`queue_full` code, for up to 60 seconds. Each wait is at least one second and never longer than
the budget left. Nothing else on the v2 surface is retried, because a repeated submit could run
a job twice; retry other failures yourself under the same idempotency key, after checking
whether the first attempt created the job.

`WithTimeout` (default 30 seconds) bounds three phases separately: connecting, waiting for
response headers, and a streamed body that sends nothing. It is not a cap on a whole request, so
a large download is never cut off for taking long while bytes still arrive. The idle clock runs
only while the SDK is waiting on the network, so a slow `io.Writer` does not trip it. A stalled
body surfaces as `sdk.ErrIdleTimeout` inside a `*sdk.TransportError`. The event stream uses its
own 45 second idle limit, since the server sends heartbeats.

To bound a whole operation, use the context:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()
job, err := client.Run(ctx, wf, sdk.SubmitOptions{})
```

`WithHTTPClient` replaces the client and all of these limits with its own settings.

## Concurrency {#concurrency}

A `*sdk.Client` is safe for concurrent use; create one and share it. A `*sdk.Job` is too: its
getters return copies of the last snapshot, and `Refresh`, `Wait`, and `Cancel` update it under
a lock. A `*sdk.Asset` serializes `Commit`, `Delete`, and `Reference`, so several submits that
share one asset upload it once. A `*sdk.Workflow` is a plain map with no locking: build or
modify it in one goroutine, or give each goroutine its own. `Submit` reads it and does not
change it.

```go
g, ctx := errgroup.WithContext(ctx)
for _, prompt := range prompts {
	g.Go(func() error {
		wf, err := sdk.WorkflowFromFile("workflow_api.json")
		if err != nil {
			return err
		}
		wf.SetInput("6", "text", prompt)
		_, err = client.Run(ctx, wf, sdk.SubmitOptions{})
		return err
	})
}
return g.Wait()
```

The example uses `golang.org/x/sync/errgroup`; any pattern that gives each goroutine its own
workflow works.

## The demo program {#demo}

`cmd/comfy-demo` in this repository drives the SDK end to end: it submits a workflow, follows
it by polling or by events, and downloads the outputs.

```sh
task build:demo
bin/comfy-demo -base http://127.0.0.1:8189 -events
bin/comfy-demo -base http://127.0.0.1:8189 -workflow my_api.json -out ./outputs
```

Runnable examples for `New`, `Client.Submit`, `Client.Run`, `Job.Events`, `WithProxyMetadata`,
and `APIError` are in the package documentation on pkg.go.dev.
