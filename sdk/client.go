// Package sdk is a Go client for the Comfy API v2: Comfy Cloud, or any
// deployment that speaks the same contract, such as comfy-api-proxy in front
// of a self-hosted ComfyUI. Code written against it runs unchanged on either;
// only the base URL and key differ.
//
// It is a port of the Python comfy-sdk's v2 surface: local blake3 dedup
// uploads, core/ASSET substitution, idempotent submit with Retry-After
// backoff, polling with an adaptive schedule, a live SSE event stream backed
// by polling, range-aware downloads, and typed errors.
//
//	client, err := sdk.New(sdk.WithBaseURL("http://127.0.0.1:8189")) // or COMFY_BASE_URL
//	wf, err := sdk.WorkflowFromFile("workflow_api.json")
//	wf.SetInput("10", "image", client.AssetFromFile("photo.png"))
//	job, err := client.Run(ctx, wf, sdk.SubmitOptions{})
//	err = job.GetOutputs("9")[0].ToFile(ctx, "out.png", nil)
//
// # Targets and credentials
//
// The base URL is [WithBaseURL], else COMFY_BASE_URL, else [CloudBaseURL].
// The key is [WithAPIKey], else COMFY_API_KEY. Comfy Cloud requires a key;
// another deployment without auth needs none. The bearer token is sent only
// to the base URL's origin and the Comfy Router origin.
//
// # Errors
//
// Every failure the API reports is an [*APIError]. Match it with errors.Is
// against a sentinel such as [ErrNotFound], or read its fields with
// errors.As. A failure that produced no response is a [*TransportError]. A
// job that ends other than succeeded is a [*JobFailedError] from [Job.Result]
// and [Client.Run].
//
// # Comfy Router
//
// [Client.Models] runs hosted models on Comfy Router with the same key.
// Model runs are billed; see [Models.Run] and [RetryPolicy].
//
// # Concurrency
//
// A [Client], [Job], [Asset], and [Models] are safe for concurrent use.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// CloudBaseURL is the hosted Comfy Cloud deployment, the default target.
	CloudBaseURL = "https://cloud.comfy.org"
	// BaseURLEnv names another deployment to target.
	BaseURLEnv = "COMFY_BASE_URL"
	// APIKeyEnv is read when no key is passed.
	APIKeyEnv = "COMFY_API_KEY"
	// RouterBaseURL is Comfy Router, where client.Models sends requests. A
	// separate host from the v2 API: one serves jobs and assets, the other
	// model-id addressed runs, and neither serves the other's routes.
	RouterBaseURL = "https://api.comfy.org"
	// RouterBaseURLEnv names another Router deployment.
	RouterBaseURLEnv = "COMFY_ROUTER_BASE_URL"

	defaultTimeout     = 30 * time.Second
	defaultRetryAfter  = 2
	maxIdempotencyKeyN = 255
)

// Retry pacing, variables only so tests can run it fast.
var (
	// retryBudget is how long submit keeps retrying a full queue.
	retryBudget = 60 * time.Second
	// minRetryDelay keeps a Retry-After of 0 or less from driving a tight loop.
	minRetryDelay = time.Second
	// retryAfterUnit is what one Retry-After second means.
	retryAfterUnit = time.Second
)

// Client talks to one Comfy API v2 deployment and to Comfy Router. It is
// safe for concurrent use.
type Client struct {
	t             *transport
	proxyMetadata bool
	// Models runs hosted models on Comfy Router, sharing this client's key.
	Models *Models
}

type options struct {
	proxyMetadata bool
	apiKey        *string
	baseURL       *string
	routerURL     *string
	retry         *RetryPolicy
	timeout       time.Duration
	clientInfo    string
	httpClient    *http.Client
}

// Option configures New.
type Option func(*options)

// WithAPIKey sets the bearer token. It wins over COMFY_API_KEY. An empty
// value counts as not given.
func WithAPIKey(key string) Option { return func(o *options) { o.apiKey = &key } }

// WithBaseURL targets a deployment, winning over COMFY_BASE_URL. An empty
// value counts as not given.
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = &u } }

// WithProxyMetadata encodes job labels as a JSON string for comfy-api-proxy.
// ListJobs filters labels locally, since the proxy has no label-map filter.
// Omit this option for deployments that accept metadata objects. Comfy Cloud
// refuses labels for now with an *APIError whose Code is
// "metadata_not_supported".
func WithProxyMetadata() Option { return func(o *options) { o.proxyMetadata = true } }

// WithRouterBaseURL targets another Comfy Router, winning over
// COMFY_ROUTER_BASE_URL.
func WithRouterBaseURL(u string) Option { return func(o *options) { o.routerURL = &u } }

// WithRetry sets the policy client.Models calls fail under. It does not
// govern Submit and Run, which follow the server's Retry-After.
func WithRetry(p RetryPolicy) Option { return func(o *options) { o.retry = &p } }

// WithTimeout bounds connecting and waiting for response headers, and how
// long a streamed body may sit idle. It is not a cap on a whole download.
// The default is 30 seconds. Synchronous model runs keep a 600 second limit
// on response headers, since Router answers only when the generation is done.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithClientInfo appends "app/<info>" to the User-Agent.
func WithClientInfo(info string) Option { return func(o *options) { o.clientInfo = info } }

// WithHTTPClient replaces the HTTP client. Its timeouts then apply as set,
// to every call including synchronous model runs, so a response-header limit
// on it must allow for a whole generation. The SDK still strips the bearer
// token on a redirect that leaves the configured origins.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// New builds a client. The target is WithBaseURL, else COMFY_BASE_URL, else
// Comfy Cloud. The key is WithAPIKey, else COMFY_API_KEY; blank counts as
// unset at either source. Comfy Cloud always needs a key, so its absence there
// is ErrMissingAPIKey here rather than a 401 on the first call. Another
// deployment may have no auth, so there no key means send no credentials.
func New(opts ...Option) (*Client, error) {
	o := options{timeout: defaultTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	base, err := resolveURL(o.baseURL, BaseURLEnv, CloudBaseURL)
	if err != nil {
		return nil, err
	}
	router, err := resolveURL(o.routerURL, RouterBaseURLEnv, RouterBaseURL)
	if err != nil {
		return nil, err
	}
	retry := DefaultRetry
	if o.retry != nil {
		retry = *o.retry
	}
	if err := retry.Validate(); err != nil {
		return nil, fmt.Errorf("retry policy: %w", err)
	}
	key := ""
	if o.apiKey != nil {
		key = strings.TrimSpace(*o.apiKey)
	}
	if key == "" {
		key = strings.TrimSpace(os.Getenv(APIKeyEnv))
	}
	if key == "" && sameDeployment(base, CloudBaseURL) {
		return nil, localError("missing_api_key", fmt.Sprintf(
			"no API key: pass sdk.WithAPIKey, or set %s in the environment. Comfy Cloud (%s) "+
				"requires one; set %s to target a deployment that does not", APIKeyEnv, CloudBaseURL, BaseURLEnv))
	}
	t, err := newTransport(base, router, key, o.clientInfo, o.timeout, o.httpClient)
	if err != nil {
		return nil, err
	}
	return &Client{t: t, proxyMetadata: o.proxyMetadata, Models: &Models{t: t, retry: retry}}, nil
}

// resolveURL is the explicit option, else the environment variable, else
// the default. Both base URLs share one rule, so they cannot validate
// differently.
func resolveURL(explicit *string, env, def string) (string, error) {
	raw, source := "", ""
	if explicit != nil {
		raw, source = strings.TrimSpace(*explicit), "base URL"
	}
	// An empty option counts as not given, as an empty WithAPIKey does.
	if raw == "" {
		raw, source = strings.TrimSpace(os.Getenv(env)), env
	}
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	valid := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
	if valid && u.Port() != "" {
		if n, perr := strconv.Atoi(u.Port()); perr != nil || n < 1 || n > 65535 {
			valid = false
		}
	}
	if !valid {
		return "", fmt.Errorf("%s must be an http(s) URL with no query or fragment (e.g. 'http://127.0.0.1:8189'); got %q", source, raw)
	}
	return raw, nil
}

// sameDeployment compares origin and path, so Comfy Cloud with its port
// written out is still Comfy Cloud, and a deployment mounted under a path on
// the same host is not.
func sameDeployment(a, b string) bool {
	pa, pb := "", ""
	if u, err := url.Parse(a); err == nil {
		pa = strings.TrimRight(u.Path, "/")
	}
	if u, err := url.Parse(b); err == nil {
		pb = strings.TrimRight(u.Path, "/")
	}
	return originOf(a) == originOf(b) && pa == pb
}

// BaseURL is the target with any embedded credentials redacted.
func (c *Client) BaseURL() string { return redactUserinfo(c.t.baseURL) }

// Authenticated reports whether the client sends a bearer token.
func (c *Client) Authenticated() bool { return c.t.apiKey != "" }

// String reports the target and whether a key is attached, never the key.
func (c *Client) String() string {
	return fmt.Sprintf("sdk.Client(base_url=%q, authenticated=%v)", c.BaseURL(), c.Authenticated())
}

// SubmitOptions are the optional parts of a submission.
type SubmitOptions struct {
	// PartnerAPIKey authenticates partner (API) nodes in the workflow, sent
	// as extra_data.api_key_comfy_org. Unrelated to the client's own key.
	PartnerAPIKey string
	// IdempotencyKey is minted fresh when empty. A reused key is rejected
	// (ErrIdempotencyKeyReuse), not replayed.
	IdempotencyKey string
	// Metadata labels the job. Use WithProxyMetadata when targeting
	// comfy-api-proxy, which requires string metadata instead of an object.
	Metadata map[string]string
}

// ValidateIdempotencyKey checks what the contract can carry: 1 to 255
// printable ASCII characters, since the value rides an HTTP header.
func ValidateIdempotencyKey(key string) error {
	switch {
	case key == "":
		return errors.New("idempotency key must not be empty; leave it unset to have one minted")
	case len(key) > maxIdempotencyKeyN:
		return fmt.Errorf("idempotency key is %d characters; the contract caps it at %d", len(key), maxIdempotencyKeyN)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] >= 0x7f {
			return errors.New("idempotency key must be printable ASCII: it is sent as an HTTP header")
		}
	}
	return nil
}

// Submit sends a workflow and returns its job. Asset handles in the graph
// are uploaded (or deduped) first and replaced with core/ASSET references.
// A 429 carrying Retry-After is retried for up to a minute. Any error from
// the submission carries the idempotency key it was sent under.
func (c *Client) Submit(ctx context.Context, wf *Workflow, opts SubmitOptions) (*Job, error) {
	if looksLikeUIFormat(wf.Graph) {
		return nil, &APIError{Code: "workflow_format_ui", HTTPStatus: 422,
			Message: "workflow is in UI-export format (nodes/links/last_node_id); submit the API-format graph instead"}
	}
	key := opts.IdempotencyKey
	if key != "" {
		if err := ValidateIdempotencyKey(key); err != nil {
			return nil, err
		}
	} else {
		var err error
		if key, err = newUUID(); err != nil {
			return nil, err
		}
	}
	graph, err := c.materialize(ctx, wf.Graph)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"workflow": graph}
	if opts.PartnerAPIKey != "" {
		body["extra_data"] = map[string]any{"api_key_comfy_org": opts.PartnerAPIKey}
	}
	if len(opts.Metadata) > 0 {
		body["metadata"] = opts.Metadata
		if c.proxyMetadata {
			encoded, err := json.Marshal(opts.Metadata)
			if err != nil {
				return nil, stampKey(err, key)
			}
			body["metadata"] = string(encoded)
		}
	}
	header := http.Header{"Idempotency-Key": {key}}

	deadline := time.Now().Add(retryBudget)
	for {
		var job wireJob
		err := c.t.jsonCall(ctx, http.MethodPost, "/jobs", body, header.Clone(), &job, http.StatusCreated)
		if err == nil {
			return &Job{t: c.t, w: job}, nil
		}
		delay, retry := retryDelay(err, deadline)
		if !retry {
			return nil, stampKey(err, key)
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, stampKey(err, key)
		}
	}
}

// retryDelay bounds the wait before retrying a 429, or reports that the
// error should surface. A 429 without Retry-After is retried only for the
// legacy queue_full code.
func retryDelay(err error, deadline time.Time) (time.Duration, bool) {
	var api *APIError
	if !errors.As(err, &api) || api.HTTPStatus != http.StatusTooManyRequests {
		return 0, false
	}
	var delay time.Duration
	switch {
	case api.RetryAfter != nil:
		delay = time.Duration(min(max(*api.RetryAfter, -maxRetryAfter), maxRetryAfter)) * retryAfterUnit
	case api.Code == "queue_full":
		delay = defaultRetryAfter * retryAfterUnit
	default:
		return 0, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	return min(max(delay, minRetryDelay), remaining), true
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run submits, then polls until the job ends. A job that ends other than
// succeeded is a *JobFailedError. Bound it with the context.
func (c *Client) Run(ctx context.Context, wf *Workflow, opts SubmitOptions) (*Job, error) {
	job, err := c.Submit(ctx, wf, opts)
	if err != nil {
		return nil, err
	}
	return job, job.Result(ctx)
}

// GetJob fetches a job by id.
func (c *Client) GetJob(ctx context.Context, id string) (*Job, error) {
	j := &Job{t: c.t, w: wireJob{ID: id}}
	if err := j.Refresh(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// JobSummary is one entry of the job list.
type JobSummary struct {
	ID           string
	Status       string
	CreateTime   *time.Time
	UpdateTime   *time.Time
	DeploymentID *string
	Metadata     map[string]string
	// Data is the item as the server sent it, for fields not lifted above.
	Data map[string]any
}

// ListOptions filter the job list.
type ListOptions struct {
	// Metadata keeps only jobs carrying every pair. The server filters by
	// it, and the SDK filters again locally. With WithProxyMetadata only the
	// local filter runs, over the jobs the server returns.
	Metadata map[string]string
	// Limit is the page size the server is asked for; zero leaves it unset.
	Limit int
}

// ListJobs walks every job the client can see, newest first, a page at a
// time, stopping at the first error.
func (c *Client) ListJobs(ctx context.Context, opts ListOptions) iter.Seq2[JobSummary, error] {
	return func(yield func(JobSummary, error) bool) {
		cursor := ""
		sent := map[string]bool{}
		for {
			page, err := c.listPage(ctx, opts, cursor)
			if err != nil {
				yield(JobSummary{}, err)
				return
			}
			items, err := pageJobs(page)
			if err != nil {
				yield(JobSummary{}, err)
				return
			}
			for _, item := range items {
				s, err := summaryFrom(item)
				if err != nil {
					yield(JobSummary{}, err)
					return
				}
				if hasLabels(s, opts.Metadata) && !yield(s, nil) {
					return
				}
			}
			next, err := nextCursor(page, sent)
			if err != nil {
				yield(JobSummary{}, err)
				return
			}
			if next == "" {
				return
			}
			cursor = next
		}
	}
}

func (c *Client) listPage(ctx context.Context, opts ListOptions, cursor string) (map[string]any, error) {
	q := url.Values{}
	if !c.proxyMetadata {
		for k, v := range opts.Metadata {
			q.Add("metadata["+k+"]", v)
		}
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/jobs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	deadline := time.Now().Add(retryBudget)
	for {
		var page map[string]any
		err := c.t.jsonCall(ctx, http.MethodGet, path, nil, nil, &page, http.StatusOK)
		if err == nil {
			return page, nil
		}
		delay, retry := retryDelay(err, deadline)
		if !retry {
			return nil, err
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func invalidResponse(msg string) error { return localError("invalid_response", msg) }

func pageJobs(page map[string]any) ([]any, error) {
	if page == nil {
		return nil, invalidResponse("job list response is not a JSON object")
	}
	raw, ok := page["jobs"]
	if !ok || raw == nil {
		return nil, nil
	}
	jobs, ok := raw.([]any)
	if !ok {
		return nil, invalidResponse("job list response field 'jobs' is not an array")
	}
	return jobs, nil
}

// nextCursor ends the walk on a missing or empty cursor, and refuses one it
// already followed, which would loop forever.
func nextCursor(page map[string]any, sent map[string]bool) (string, error) {
	raw, ok := page["next_cursor"]
	if !ok || raw == nil {
		return "", nil
	}
	s, isStr := raw.(string)
	if !isStr {
		return "", invalidResponse("job list response field 'next_cursor' is not a string")
	}
	if s == "" {
		return "", nil
	}
	if sent[s] {
		return "", invalidResponse("job list returned a cursor it already returned; stopping to avoid a loop")
	}
	sent[s] = true
	return s, nil
}

func summaryFrom(item any) (JobSummary, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return JobSummary{}, invalidResponse("job list item is not a JSON object")
	}
	var missing, wrong []string
	for _, k := range []string{"id", "status"} {
		switch m[k].(type) {
		case nil:
			missing = append(missing, k)
		case string:
		default:
			wrong = append(wrong, k)
		}
	}
	if len(missing) > 0 {
		return JobSummary{}, invalidResponse("job list item is missing " + strings.Join(missing, " and "))
	}
	var deployment *string
	switch d := m["deployment_id"].(type) {
	case nil:
	case string:
		deployment = &d
	default:
		wrong = append(wrong, "deployment_id")
	}
	if len(wrong) > 0 {
		return JobSummary{}, invalidResponse("job list item has a non-string " + strings.Join(wrong, " and "))
	}
	s := JobSummary{
		ID:           m["id"].(string),
		Status:       m["status"].(string),
		CreateTime:   parseTime(m["create_time"]),
		UpdateTime:   parseTime(m["update_time"]),
		DeploymentID: deployment,
		Metadata:     map[string]string{},
		Data:         m,
	}
	s.Metadata = metadataLabels(m["metadata"])
	return s, nil
}

func parseTime(v any) *time.Time {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func hasLabels(s JobSummary, want map[string]string) bool {
	for k, v := range want {
		if got, ok := s.Metadata[k]; !ok || got != v {
			return false
		}
	}
	return true
}
