---
title: Comfy Cloud and Router
section: SDK
order: 20
next: differences
description: How to point the same Go SDK client at Comfy Cloud for workflows and at Comfy Router for hosted models, and what changes from the local proxy. Covers model runs, queued requests, schemas, retries, and which calls are billed.
---

The client from the [Go SDK](sdk.md) page reaches Comfy Cloud with a different base URL and a
key. The workflow code itself does not change. The same client also runs hosted models on Comfy
Router through `client.Models`.

## What changes from the local proxy {#changes}

| | Local proxy | Comfy Cloud |
| --- | --- | --- |
| Base URL | `http://127.0.0.1:8189` (or your host) | `https://cloud.comfy.org`, the default |
| Key | Optional; the proxy's token when it has one | Required, your Comfy API key |
| Job labels | `WithProxyMetadata()` | Leave the option off |
| Job list | Served by the proxy | `https://platformapi.comfy.org` (see [below](#jobs)) |
| `Output.DownloadURL` | The proxy's content endpoint | A signed URL with an expiry |
| Cost | Free | Billed per workflow run |

With no base URL set, `sdk.New` targets Comfy Cloud and requires a key. A missing key fails at
construction rather than as a 401 on the first call:

```go
// COMFY_API_KEY must be set in the environment.
client, err := sdk.New()
if errors.Is(err, sdk.ErrMissingAPIKey) {
	log.Fatal("set COMFY_API_KEY")
}
if err != nil {
	log.Fatal(err)
}
```

Keep the key in the environment rather than in source. `sdk.WithAPIKey(key)` wins over
`COMFY_API_KEY` when you load it from somewhere else. A blank value counts as unset.

To switch one program between the proxy and Cloud, set `COMFY_BASE_URL` and `COMFY_API_KEY` and
call `sdk.New()` with no target options. Unset `COMFY_BASE_URL` means Cloud.

## Workflows on Comfy Cloud {#workflows}

Submitting, waiting, and downloading are the calls from the [Go SDK](sdk.md) page:

```go
wf, err := sdk.WorkflowFromFile("workflow_api.json")
if err != nil {
	return err
}
wf.SetInput("10", "image", client.AssetFromFile("photo.png"))
job, err := client.Run(ctx, wf, sdk.SubmitOptions{})
if err != nil {
	return err
}
for _, o := range job.Outputs() {
	if err := o.ToFile(ctx, filepath.Base(o.Name()), nil); err != nil {
		return err
	}
}
```

Differences seen on Cloud:

- **Every run is billed.** A retry under a fresh key is a second run. Supply an
  `IdempotencyKey` you can recompute, and on an ambiguous failure look for the job before
  submitting again.
- **Models must exist on Cloud.** A graph that loads a checkpoint only your local ComfyUI has
  fails there.
- **The event stream** sent `queued` then `succeeded`, with no live `output` event, in the runs
  checked so far. Read outputs from `job.Outputs()` after `job.Result`, which works on both.
- **Download URLs** are signed URLs on another host. `Output.DownloadURL` returns one with its
  `ExpiresAt` and never contacts that host, so the bearer token does not reach it.
  `ToFile`, `ToWriter`, and `Bytes` follow the redirect for you and drop the token on the way.
- **No labels yet.** Cloud refuses any `SubmitOptions.Metadata` with HTTP 422 and code
  `metadata_not_supported` ("Job metadata is kept only for jobs sent to a deployment for now"),
  checked before the workflow, so no job is created and nothing is billed. Submit to Cloud
  without labels, and match the case with `errors.As(err, &apiErr)` and
  `apiErr.Code == "metadata_not_supported"`. Checked against `cloud.comfy.org` on 2026-10-27.
- **Partner nodes** in the graph take `SubmitOptions.PartnerAPIKey`.

### Listing jobs {#jobs}

`https://cloud.comfy.org` answers `GET /api/v2/jobs` with HTTP 501. The workspace address
`https://platformapi.comfy.org` serves it with the same key, so list through a second client:

```go
jobs, err := sdk.New(sdk.WithBaseURL("https://platformapi.comfy.org"))
if err != nil {
	return err
}
for s, err := range jobs.ListJobs(ctx, sdk.ListOptions{Limit: 10}) {
	if err != nil {
		return err
	}
	fmt.Println(s.ID, s.Status)
}
```

When a page carries `next_cursor`, `ListJobs` follows it until the last page or until you break
out of the loop.

## Comfy Router {#router}

Comfy Router runs hosted models (image, video, and audio providers) addressed by a model id of
the form `provider/model`. It lives on a separate host from the v2 API: `https://api.comfy.org`
by default, or `COMFY_ROUTER_BASE_URL`, or `sdk.WithRouterBaseURL(u)`. `client.Models` uses the
client's key, which is sent to exactly the v2 origin and the Router origin.

### Catalog and schemas {#catalog}

Reading the catalog and schemas is free.

```go
for m, err := range client.Models.List(ctx, sdk.ListModelsOptions{}) {
	if err != nil {
		return err
	}
	fmt.Println(m.ID, m.Billing)
}
```

`List` walks every page. `Models.Page(ctx, opts)` fetches one page, with `HasMore` and
`NextCursor` for walking it yourself; a `Limit` above 100 is clamped by the server.
`CatalogModel.Billing` holds per-model billing facts as the server sent them, not prices.

`Models.Schema` returns a model's OpenAPI document. Pass the `ETag` from an earlier call to get
`Unchanged` instead of the document again:

```go
res, err := client.Models.Schema(ctx, "bfl/flux-pro-1.1", "")
if err != nil {
	return err
}
// later
again, err := client.Models.Schema(ctx, "bfl/flux-pro-1.1", res.ETag)
if err != nil {
	return err
}
if again.Unchanged {
	// res.Document is still current
}
```

### Running a model {#run}

There are two ways to run a model. Both are billed.

**Synchronous: `Run` and `RunDetailed`.** One request that returns when the generation is done.
The response-header limit for these calls is 600 seconds, independent of `WithTimeout`.

```go
res, err := client.Models.RunDetailed(ctx, "bfl/flux-pro-1.1",
	map[string]any{"prompt": "a red cube", "width": 512, "height": 512},
	sdk.RunOptions{})
if err != nil {
	return err
}
switch {
case res.Output.Binary != nil:
	if err := os.WriteFile("out.bin", res.Output.Binary.Content, 0o644); err != nil {
		return err
	}
case res.Output.JSON != nil:
	fmt.Println(res.Output.JSON) // often a short-lived signed URL to the result
}
if res.CreditsUsed != nil {
	fmt.Println("credits:", *res.CreditsUsed)
}
```

`RunOutput` holds exactly one of `JSON` (for a provider that answers JSON) and `Binary` (for one
that answers the generated bytes). `Run` returns only the output. `RunDetailed` adds what Router
disclosed: `RequestID`, `Replayed` (the answer came from the key's earlier record and was not
run or billed again), `ServingProvider` (set when a fallback provider served the call),
`DroppedParams`, and `CreditsUsed`. A nil `CreditsUsed` means not reported, which is not the
same as free.

**Queued: `Submit`, then `Updates`, `Get`, or `Cancel`.** Use this for long generations, or to
show queue position.

```go
h, err := client.Models.Submit(ctx, "bfl/flux-pro-1.1", map[string]any{"prompt": "a red cube"}, sdk.RunOptions{})
if err != nil {
	return err
}
log.Println("request", h.RequestID()) // keep it to collect later with Models.Handle
for u, err := range h.Updates(ctx) {
	if err != nil {
		return err
	}
	fmt.Println(u.Status, u.QueuePosition)
}
out, err := h.Get(ctx)
if err != nil {
	return err
}
fmt.Println(out.JSON)
```

`Updates` yields each change of status or queue position and ends after `COMPLETED`
(`sdk.Completed`), pacing its polls by the server's `Retry-After`. `Get` waits for completion
and collects the output; a completion that names an error type comes back as that Router error.
`h.Cancel(ctx)` asks the queue to stop. `client.Models.Handle(model, requestID)` rebuilds a
handle in another process.

`Models.Subscribe(ctx, model, args, opts, onUpdate)` does submit, follow, and collect in one
call. If `ctx` ends first, it cancels the request on a best-effort basis before returning, so an
abandoned wait does not leave a generation running.

`sdk.RunOptions` fields: `IdempotencyKey` (minted when empty), `ModelProvider` (route to an
alternate provider, translating the body), `StrictMode` (refuse a translation that would drop
fields), and `FallbackProvider` (`"true"`, `"false"`, or a provider name).

### Retries {#retries}

Router calls follow a `sdk.RetryPolicy`, `sdk.DefaultRetry` unless replaced with
`sdk.WithRetry`. It does not apply to workflow `Submit` and `Run`.

- A 429 is retried only when it carries `Retry-After`.
- A failure that never reached the server (DNS, refused connection, connect timeout) is retried.
- A 5xx or a connection dropped mid-request may have started a billed run, so it is retried only
  when `RetryPossiblyInFlight` is true. It is false in `DefaultRetry`.
- A generation still running (409 `concurrency_limit_exceeded` or 504 `deadline_exceeded`, each
  with `Retry-After`) is collected under the same key for up to `CollectMaxElapsed` (20 minutes
  by default) when `RetryCollectable` is true.
- Everything else, and anything after `MaxElapsed` (60 seconds by default), is returned.

Catalog and schema reads also retry in-flight failures, since repeating a read changes nothing.
`sdk.NoRetry` makes every call a single attempt.

```go
policy := sdk.DefaultRetry
policy.RetryPossiblyInFlight = true // accept the billing risk of a resend after a 5xx
client, err := sdk.New(sdk.WithRetry(policy))
```

All retries of one run share one `Idempotency-Key`, so Router can recognize a resend. If a resend
is refused as a key reuse after an earlier attempt may have claimed the key, the SDK returns that
earlier failure with `ResendRefused` set on the `*sdk.APIError` or `*sdk.TransportError`. The
generation may exist and may have been billed: collect it under the key rather than running a
new one.

### Router errors {#errors}

Router failures are `*sdk.APIError` values with `ErrorType` set to the Router's bucket.
`errors.Is(err, sdk.ErrRouter)` matches any of them. Each bucket has a sentinel:
`ErrInvalidInput`, `ErrContentPolicyViolation`, `ErrProviderError`, `ErrProviderTimeout`,
`ErrInsufficientCredits`, `ErrModelNotFound`, `ErrUnauthorized`, `ErrForbidden`,
`ErrConcurrencyLimitExceeded`, `ErrClientDisconnected`, `ErrInternalError`,
`ErrDeadlineExceeded`, `ErrNotEnabled`, `ErrServiceUnavailable`, `ErrRateLimited`,
`ErrCancelled`, `ErrQueueTimeout`, `ErrRequestNotFound`, and `ErrQueueBacklogFull`. A refused
cancel matches `ErrCancelRefused`, and `ErrAlreadyCompleted` when the request had finished.

```go
_, err := client.Models.Run(ctx, model, args, sdk.RunOptions{})
var apiErr *sdk.APIError
switch {
case errors.Is(err, sdk.ErrInvalidInput) && errors.As(err, &apiErr):
	for _, v := range apiErr.Validation {
		fmt.Println(v.Location(), v.Msg)
	}
case errors.Is(err, sdk.ErrInsufficientCredits):
	// top up before retrying
case errors.As(err, &apiErr) && apiErr.ResendRefused:
	// collect under apiErr.IdempotencyKey; do not start a new run
}
```

## Billing {#billing}

These calls cost money: `Models.Run`, `Models.RunDetailed`, `Models.Submit`, `Models.Subscribe`,
and any workflow `Submit` or `Run` against Comfy Cloud. A content-policy rejection may be charged
for some models; the catalog's `Billing` map flags those with `charges_on_policy_rejection`.
`CreditsUsed` reports what Router priced a run at when it says so, and nil otherwise. The SDK
does not estimate or cap spending. The catalog, schemas, job reads, job lists, and downloads are
free.

## Trying it from a clone {#tasks}

A clone of this repository has `task` targets that drive `cmd/comfy-demo` against Cloud. Copy
`.env.example` to `.env` and set `COMFY_API_KEY`; only these targets load it. The billed ones ask
for confirmation first.

```sh
task cloud:jobs                                            # free: recent Cloud jobs, proves the key
task cloud:models                                          # free: the Router catalog
task cloud:schema -- bfl/flux-pro-1.1                      # free: one model's input schema
task cloud:run -- bfl/flux-pro-1.1 '{"prompt":"a red cube"}'   # billed: one synchronous run
task cloud:workflow -- -workflow my_api.json -events       # billed: a workflow on Cloud
```
