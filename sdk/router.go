package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Router paths, under the Router base URL.
const (
	modelRunPath     = "/v2/models/%s/%s"
	modelCatalogPath = "/v2/models"
	maxRequestIDLen  = 256
	maxQueuePace     = 60 * time.Second
	cancelTimeout    = 10 * time.Second
	// Completed is the queue status of a finished request, failed or not.
	Completed = "COMPLETED"
)

// Models runs hosted models on Comfy Router (client.Models). It shares the
// client's key and follows its RetryPolicy.
type Models struct {
	t     *transport
	retry RetryPolicy
}

// BaseURL is the Router target with any embedded credentials redacted.
func (m *Models) BaseURL() string { return redactUserinfo(m.t.routerURL) }

// Retry is the policy Router calls fail under.
func (m *Models) Retry() RetryPolicy { return m.retry }

// ParseModelID splits a "{provider}/{model}" id. A three-segment variant id
// is not addressable on the run route yet, and "." or ".." segments would
// walk the request path, so both are refused.
func ParseModelID(model string) (string, string, error) {
	segments := strings.Split(model, "/")
	shape := fmt.Errorf("model id must be '{provider}/{model}', exactly two non-empty segments separated by '/'; got %q", model)
	for _, s := range segments {
		if s == "" {
			return "", "", shape
		}
	}
	switch len(segments) {
	case 2:
	case 3:
		return "", "", fmt.Errorf("model id %q carries a variant segment, which the run route does not address yet; pass the two-segment id the catalog lists", model)
	default:
		return "", "", shape
	}
	for _, s := range segments {
		if s == "." || s == ".." {
			return "", "", fmt.Errorf("model id segments must not be '.' or '..', which would traverse the request path; got %q", model)
		}
	}
	return segments[0], segments[1], nil
}

// ParseRequestID checks a queued request id can address its routes.
func ParseRequestID(id string) error {
	switch {
	case id == "":
		return errors.New("request id must not be empty")
	case strings.Contains(id, "/"):
		return fmt.Errorf("request id must be a single path segment; got %q", id)
	case id == "." || id == "..":
		return fmt.Errorf("request id must not be '.' or '..'; got %q", id)
	case len([]rune(id)) > maxRequestIDLen:
		return fmt.Errorf("request id must be at most %d characters; got %d", maxRequestIDLen, len([]rune(id)))
	}
	for _, r := range id {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("request id must not contain control characters; got %q", id)
		}
	}
	return nil
}

func (m *Models) modelURL(model, suffix string) (string, error) {
	p, n, err := ParseModelID(model)
	if err != nil {
		return "", err
	}
	return m.t.routerURL + fmt.Sprintf(modelRunPath, url.PathEscape(p), url.PathEscape(n)) + suffix, nil
}

// BinaryResult is a model's own bytes, for a partner that answers with the
// generation itself rather than JSON.
type BinaryResult struct {
	Content     []byte
	ContentType string
	RequestID   string
}

// RunOutput is a model's native output: JSON for a partner that answers
// JSON, Binary for one that answers bytes. Exactly one is set.
type RunOutput struct {
	JSON   map[string]any
	Binary *BinaryResult
}

// RunResult is a run's output plus what Router disclosed about it.
type RunResult struct {
	Output RunOutput
	// ServingProvider is the provider that served the call after a
	// fallback succeeded; nil means the requested provider served it.
	ServingProvider *string
	// DroppedParams names native fields translation could not carry.
	DroppedParams []string
	// Replayed means the response came from the key's record and was not
	// run, or billed, again.
	Replayed  bool
	RequestID string
	// CreditsUsed is what Router priced the run at, as its decimal string.
	// Nil means not reported, never free; "0" is a real reported cost.
	CreditsUsed *string
}

// RunOptions are the optional parts of a model run.
type RunOptions struct {
	// IdempotencyKey is minted fresh when empty. Reuse it to retry a run
	// without paying twice.
	IdempotencyKey string
	// ModelProvider routes to an alternate provider, translating the body.
	ModelProvider string
	// StrictMode refuses translation that would drop fields.
	StrictMode *bool
	// FallbackProvider is "true", "false", or a provider to fall back to.
	FallbackProvider string
}

func runQuery(o RunOptions) string {
	q := url.Values{}
	if o.ModelProvider != "" {
		q.Set("model_provider", o.ModelProvider)
	}
	if o.StrictMode != nil {
		q.Set("strict_mode", strconv.FormatBool(*o.StrictMode))
	}
	if o.FallbackProvider != "" {
		q.Set("fallback_provider", o.FallbackProvider)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func jsonBody(v any) (*bytes.Reader, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode arguments: %w", err)
	}
	return bytes.NewReader(raw), nil
}

// runOutput reads a run or result body: JSON by Content-Type, bytes
// otherwise, and an untyped body probed for a JSON object.
func runOutput(resp *http.Response, data []byte) (RunOutput, error) {
	media := mediaType(resp.Header.Get("Content-Type"))
	binary := func() RunOutput {
		ct := cleanBodyExcerpt(resp.Header.Get("Content-Type"))
		if len(ct) > 128 {
			ct = ct[:128]
		}
		return RunOutput{Binary: &BinaryResult{Content: data, ContentType: ct, RequestID: cleanRequestID(resp.Header.Get("X-Comfy-Request-Id"))}}
	}
	if media != "" {
		if media == "application/json" || strings.HasSuffix(media, "+json") {
			if len(data) == 0 {
				return RunOutput{JSON: map[string]any{}}, nil
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return RunOutput{}, &APIError{Code: "invalid_response", HTTPStatus: resp.StatusCode,
					Message:     fmt.Sprintf("Could not decode the %d response body as a JSON object", resp.StatusCode),
					RequestID:   cleanRequestID(resp.Header.Get("X-Comfy-Request-Id")),
					BodyExcerpt: cleanBodyExcerpt(string(data))}
			}
			return RunOutput{JSON: m}, nil
		}
		return binary(), nil
	}
	if len(data) == 0 {
		return RunOutput{JSON: map[string]any{}}, nil
	}
	var m map[string]any
	if json.Unmarshal(data, &m) == nil && m != nil {
		return RunOutput{JSON: m}, nil
	}
	return binary(), nil
}

var decimalRe = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

func runResult(out RunOutput, h http.Header) RunResult {
	r := RunResult{Output: out, Replayed: len(h.Values("Idempotent-Replayed")) > 0, RequestID: cleanRequestID(h.Get("X-Comfy-Request-Id"))}
	if v := h.Values("X-Comfy-Router-Fallback-Provider"); len(v) > 0 {
		r.ServingProvider = &v[0]
	}
	if v := h.Values("X-Comfy-Router-Dropped-Params"); len(v) > 0 {
		var list []string
		if json.Unmarshal([]byte(v[0]), &list) == nil {
			r.DroppedParams = list
		} else {
			r.DroppedParams = []string{v[0]}
		}
	}
	if v := strings.TrimSpace(h.Get("X-Comfy-Credits-Used")); v != "" && decimalRe.MatchString(v) {
		r.CreditsUsed = &v
	}
	return r
}

func isKeyReuse(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.HTTPStatus == 422 && api.Code == "idempotency_key_reuse"
}

func markResendRefused(err error) error {
	var api *APIError
	if errors.As(err, &api) {
		api.ResendRefused = true
		return err
	}
	var te *TransportError
	if errors.As(err, &te) {
		te.ResendRefused = true
	}
	return err
}

// Run invokes a model and waits for its output. It is billed. A failure the
// Router may have acted on (a 5xx, a dropped connection) is retried only
// when the policy allows it; retries reuse one Idempotency-Key, so a retry
// cannot bill twice. If a retry is refused as a key reuse after an earlier
// attempt may have claimed the key, the earlier failure is returned with
// ResendRefused set: the generation may exist, so collect it under the key.
func (m *Models) Run(ctx context.Context, model string, args map[string]any, opts RunOptions) (RunOutput, error) {
	r, err := m.RunDetailed(ctx, model, args, opts)
	return r.Output, err
}

// RunDetailed is Run with Router's disclosures beside the output.
func (m *Models) RunDetailed(ctx context.Context, model string, args map[string]any, opts RunOptions) (RunResult, error) {
	target, err := m.modelURL(model, runQuery(opts))
	if err != nil {
		return RunResult{}, err
	}
	key, err := keyOrMint(opts.IdempotencyKey)
	if err != nil {
		return RunResult{}, err
	}
	if args == nil {
		args = map[string]any{}
	}
	rt := newRetrier(m.retry)
	var claimed error
	for {
		body, err := jsonBody(args)
		if err != nil {
			return RunResult{}, err
		}
		resp, data, err := m.t.doWith(ctx, m.t.runs, http.MethodPost, target, body,
			http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {key}})
		if err == nil {
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				out, perr := runOutput(resp, data)
				if perr != nil {
					return RunResult{}, stampKey(perr, key)
				}
				return runResult(out, resp.Header), nil
			}
			err = errorFromResponse(resp.StatusCode, data, resp.Header.Get)
		}
		if claimed != nil && isKeyReuse(err) {
			return RunResult{}, markResendRefused(stampKey(claimed, key))
		}
		d, retry := rt.delay(err)
		if !retry {
			return RunResult{}, stampKey(err, key)
		}
		if mayHaveClaimedKey(err) {
			claimed = err
		}
		if serr := sleep(ctx, d); serr != nil {
			return RunResult{}, stampKey(serr, key)
		}
	}
}

func keyOrMint(key string) (string, error) {
	if key == "" {
		return newUUID()
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// QueueUpdate is one observed state of a queued request.
type QueueUpdate struct {
	RequestID     string
	Status        string
	QueuePosition *int
	ErrorType     string
	// RetryAfter is the pace the server asked for, in seconds.
	RetryAfter *int
	Raw        map[string]any
}

// IsCompleted reports a finished request, failed or not.
func (u QueueUpdate) IsCompleted() bool { return u.Status == Completed }

func updateFrom(payload map[string]any, h http.Header, requestID string, requireStatus bool) (QueueUpdate, error) {
	if payload == nil {
		return QueueUpdate{}, invalidResponse("the queue answered with a body that is not a JSON object, so the request's state cannot be read from it")
	}
	status, _ := payload["status"].(string)
	if requireStatus && strings.TrimSpace(status) == "" {
		return QueueUpdate{}, invalidResponse("the status read answered without naming a status, so the request's state is unknown")
	}
	u := QueueUpdate{RequestID: requestID, Status: status, Raw: payload}
	if f, ok := payload["queue_position"].(float64); ok && f == float64(int(f)) {
		n := int(f)
		u.QueuePosition = &n
	}
	if et, ok := payload["error_type"].(string); ok {
		u.ErrorType = strings.TrimSpace(et)
	}
	if ra := retryAfter(h.Get("Retry-After")); ra != nil && *ra > 0 {
		u.RetryAfter = ra
	}
	return u, nil
}

// RequestHandle is a queued model request.
type RequestHandle struct {
	m         *Models
	model     string
	requestID string
}

// Model is the model id the request runs.
func (h *RequestHandle) Model() string { return h.model }

// RequestID is the queue's id for the request.
func (h *RequestHandle) RequestID() string { return h.requestID }

// String reports the model and request id.
func (h *RequestHandle) String() string {
	return fmt.Sprintf("RequestHandle(model=%q, request_id=%q)", h.model, h.requestID)
}

func (h *RequestHandle) url(suffix string) (string, error) {
	return h.m.modelURL(h.model, "/requests/"+url.PathEscape(h.requestID)+suffix)
}

// Submit queues a model run and returns its handle. It is billed when it
// runs. Collect with Get, follow with Updates, or stop with Cancel.
func (m *Models) Submit(ctx context.Context, model string, args map[string]any, opts RunOptions) (*RequestHandle, error) {
	target, err := m.modelURL(model, "/requests")
	if err != nil {
		return nil, err
	}
	key, err := keyOrMint(opts.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	payload, err := withRetry(ctx, m.retry, func() (map[string]any, error) {
		body, err := jsonBody(args)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		err = m.t.jsonCallReader(ctx, http.MethodPost, target, body, http.Header{"Idempotency-Key": {key}}, &out,
			http.StatusOK, http.StatusCreated, http.StatusAccepted)
		return out, err
	})
	if err != nil {
		return nil, stampKey(err, key)
	}
	id, _ := payload["request_id"].(string)
	if id == "" {
		return nil, stampKey(invalidResponse("the queue accepted the request but named no request_id, so it cannot be polled, collected, or cancelled"), key)
	}
	if err := ParseRequestID(id); err != nil {
		return nil, stampKey(invalidResponse("the queue named a request_id that cannot address a route: "+err.Error()), key)
	}
	return &RequestHandle{m: m, model: model, requestID: id}, nil
}

// Handle rebuilds a handle for a request submitted earlier.
func (m *Models) Handle(model, requestID string) (*RequestHandle, error) {
	if _, _, err := ParseModelID(model); err != nil {
		return nil, err
	}
	if err := ParseRequestID(requestID); err != nil {
		return nil, err
	}
	return &RequestHandle{m: m, model: model, requestID: requestID}, nil
}

func (h *RequestHandle) call(ctx context.Context, policy RetryPolicy, method, suffix string, ok ...int) (map[string]any, http.Header, error) {
	target, err := h.url(suffix)
	if err != nil {
		return nil, nil, err
	}
	type reply struct {
		body   map[string]any
		header http.Header
	}
	r, err := withRetry(ctx, policy, func() (reply, error) {
		resp, data, err := h.m.t.do(ctx, method, target, nil, nil)
		if err != nil {
			return reply{}, err
		}
		var body map[string]any
		if err := decodeOrRaise(resp, data, &body, ok...); err != nil {
			return reply{}, err
		}
		return reply{body, resp.Header}, nil
	})
	return r.body, r.header, err
}

// Status reads the request's current state.
func (h *RequestHandle) Status(ctx context.Context) (QueueUpdate, error) {
	body, hdr, err := h.call(ctx, h.m.retry, http.MethodGet, "/status", http.StatusOK)
	if err != nil {
		return QueueUpdate{}, err
	}
	return updateFrom(body, hdr, h.requestID, true)
}

// Updates polls the request, yielding each change of status or position,
// and ends after COMPLETED. Polls follow the queue's Retry-After (capped at a
// minute), else a schedule from half a second to five. Bound it with ctx.
func (h *RequestHandle) Updates(ctx context.Context) iter.Seq2[QueueUpdate, error] {
	return func(yield func(QueueUpdate, error) bool) {
		var prev *QueueUpdate
		delay := pollStart
		for {
			u, err := h.Status(ctx)
			if err != nil {
				yield(QueueUpdate{}, err)
				return
			}
			changed := prev == nil || prev.Status != u.Status || !sameInt(prev.QueuePosition, u.QueuePosition)
			prev = &u
			if changed && !yield(u, nil) {
				return
			}
			if u.IsCompleted() {
				return
			}
			wait := delay
			delay = min(time.Duration(float64(delay)*pollFactor), pollCap)
			if u.RetryAfter != nil {
				wait = min(time.Duration(*u.RetryAfter)*time.Second, maxQueuePace)
			}
			if err := sleep(ctx, wait); err != nil {
				yield(QueueUpdate{}, fmt.Errorf("model request %s not complete (status %q): %w", h.requestID, u.Status, err))
				return
			}
		}
	}
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Get waits for the request to complete, then collects its output. A
// completion that names an error_type is returned as that Router error.
func (h *RequestHandle) Get(ctx context.Context) (RunOutput, error) {
	var last *QueueUpdate
	for u, err := range h.Updates(ctx) {
		if err != nil {
			return RunOutput{}, err
		}
		last = &u
	}
	if last == nil || !last.IsCompleted() {
		return RunOutput{}, invalidResponse("the poll loop ended without observing a completion")
	}
	return h.collect(ctx, *last)
}

func (h *RequestHandle) collect(ctx context.Context, completion QueueUpdate) (RunOutput, error) {
	if e := errorFromCompletion(completion.Raw, h.requestID); e != nil {
		return RunOutput{}, e
	}
	target, err := h.url("")
	if err != nil {
		return RunOutput{}, err
	}
	out, err := withRetry(ctx, h.m.retry, func() (RunOutput, error) {
		resp, data, err := h.m.t.do(ctx, http.MethodGet, target, nil, nil)
		if err != nil {
			return RunOutput{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return RunOutput{}, errorFromResponse(resp.StatusCode, data, resp.Header.Get)
		}
		return runOutput(resp, data)
	})
	if err != nil {
		return RunOutput{}, err
	}
	// The result route can carry the terminal envelope again.
	if out.JSON != nil && out.JSON["status"] == Completed {
		if e := errorFromCompletion(out.JSON, h.requestID); e != nil {
			return RunOutput{}, e
		}
	}
	return out, nil
}

// Cancel asks the queue to stop the request.
func (h *RequestHandle) Cancel(ctx context.Context) (QueueUpdate, error) {
	body, hdr, err := h.call(ctx, h.m.retry, http.MethodPut, "/cancel", http.StatusOK, http.StatusAccepted, http.StatusNoContent)
	if err != nil {
		return QueueUpdate{}, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return updateFrom(body, hdr, h.requestID, false)
}

// cancelBestEffort is one short cancel attempt that outlives a finished ctx.
func (h *RequestHandle) cancelBestEffort(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	_, _, err := h.call(cctx, NoRetry, http.MethodPut, "/cancel", http.StatusOK, http.StatusAccepted, http.StatusNoContent)
	return err
}

// Subscribe submits, follows the queue (passing each change to onUpdate
// when it is non-nil), and collects the output. When ctx ends first, the
// request is cancelled on a best-effort basis before the error returns, so
// an abandoned wait does not leave a generation running.
func (m *Models) Subscribe(ctx context.Context, model string, args map[string]any, opts RunOptions, onUpdate func(QueueUpdate)) (RunOutput, error) {
	h, err := m.Submit(ctx, model, args, opts)
	if err != nil {
		return RunOutput{}, err
	}
	var last *QueueUpdate
	for u, err := range h.Updates(ctx) {
		if err != nil {
			if ctx.Err() != nil {
				if cerr := h.cancelBestEffort(ctx); cerr != nil {
					return RunOutput{}, errors.Join(err, fmt.Errorf("best-effort cancel of %s: %w", h.requestID, cerr))
				}
			}
			return RunOutput{}, err
		}
		last = &u
		if onUpdate != nil {
			onUpdate(u)
		}
	}
	if last == nil || !last.IsCompleted() {
		return RunOutput{}, invalidResponse("the poll loop ended without observing a completion")
	}
	return h.collect(ctx, *last)
}

// CatalogModel is one runnable model.
type CatalogModel struct {
	ID       string
	Provider string
	Model    string
	// Billing holds per-model billing facts (never prices), as the server
	// sent them, so a field it adds reaches the caller without a release.
	Billing map[string]any
}

// ModelPage is one page of the catalog.
type ModelPage struct {
	Data []CatalogModel
	// HasMore is whether another page exists; walk on it, never on a short
	// Data.
	HasMore    bool
	NextCursor string
	// Limit is the page size served; a request above 100 is clamped.
	Limit     int
	RequestID string
}

// ListModelsOptions page the catalog.
type ListModelsOptions struct {
	Cursor string
	Limit  int
}

// readPolicy also retries in-flight failures: a catalog or schema read
// changes nothing, so repeating it is always safe.
func (m *Models) readPolicy() RetryPolicy {
	p := m.retry
	p.RetryPossiblyInFlight = true
	return p
}

// Page fetches one catalog page.
func (m *Models) Page(ctx context.Context, opts ListModelsOptions) (ModelPage, error) {
	q := url.Values{}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	target := m.t.routerURL + modelCatalogPath
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	type reply struct {
		body map[string]any
		rid  string
	}
	r, err := withRetry(ctx, m.readPolicy(), func() (reply, error) {
		resp, data, err := m.t.do(ctx, http.MethodGet, target, nil, nil)
		if err != nil {
			return reply{}, err
		}
		var body map[string]any
		if err := decodeOrRaise(resp, data, &body, http.StatusOK); err != nil {
			return reply{}, err
		}
		return reply{body, cleanRequestID(resp.Header.Get("X-Comfy-Request-Id"))}, nil
	})
	if err != nil {
		return ModelPage{}, err
	}
	return catalogPage(r.body, r.rid)
}

func catalogInvalid(msg, rid string) error {
	e := localError("invalid_response", msg)
	e.RequestID = rid
	return e
}

func catalogPage(body map[string]any, rid string) (ModelPage, error) {
	if body == nil {
		return ModelPage{}, catalogInvalid("catalog page is not a JSON object", rid)
	}
	data, okData := body["data"].([]any)
	hasMore, okMore := body["has_more"].(bool)
	if !okData || !okMore {
		return ModelPage{}, catalogInvalid("catalog page is missing data or has_more", rid)
	}
	page := ModelPage{HasMore: hasMore, RequestID: rid, Limit: len(data)}
	if l, ok := body["limit"].(float64); ok && l == float64(int(l)) {
		page.Limit = int(l)
	}
	if c, ok := body["next_cursor"].(string); ok && hasMore {
		page.NextCursor = c
	}
	for _, raw := range data {
		e, ok := raw.(map[string]any)
		if !ok {
			return ModelPage{}, catalogInvalid("catalog entry is not a JSON object", rid)
		}
		id, _ := e["id"].(string)
		provider, _ := e["provider"].(string)
		name, _ := e["model"].(string)
		if id == "" || provider == "" || name == "" {
			return ModelPage{}, catalogInvalid("catalog entry is missing id, provider or model", rid)
		}
		p, n, err := ParseModelID(id)
		if err != nil {
			return ModelPage{}, catalogInvalid("catalog entry id is not a runnable model id", rid)
		}
		if p != provider || n != name {
			return ModelPage{}, catalogInvalid("catalog entry id does not match its provider and model", rid)
		}
		billing, _ := e["billing"].(map[string]any)
		if billing == nil {
			billing = map[string]any{}
		}
		page.Data = append(page.Data, CatalogModel{ID: id, Provider: provider, Model: name, Billing: billing})
	}
	return page, nil
}

// List walks the whole catalog, page by page, stopping at the first error.
func (m *Models) List(ctx context.Context, opts ListModelsOptions) iter.Seq2[CatalogModel, error] {
	return func(yield func(CatalogModel, error) bool) {
		seen := map[string]bool{}
		if opts.Cursor != "" {
			seen[opts.Cursor] = true
		}
		for {
			page, err := m.Page(ctx, opts)
			if err != nil {
				yield(CatalogModel{}, err)
				return
			}
			for _, cm := range page.Data {
				if !yield(cm, nil) {
					return
				}
			}
			if !page.HasMore {
				return
			}
			switch {
			case page.NextCursor == "":
				yield(CatalogModel{}, catalogInvalid("catalog page says has_more but names no next_cursor", page.RequestID))
				return
			case seen[page.NextCursor]:
				yield(CatalogModel{}, catalogInvalid("catalog walk was handed a cursor it already followed", page.RequestID))
				return
			}
			seen[page.NextCursor] = true
			opts.Cursor = page.NextCursor
		}
	}
}

// SchemaResult is a model's input and output schemas.
type SchemaResult struct {
	// Unchanged is true on a 304: the document is the one the etag names.
	Unchanged bool
	// Document is a standalone OpenAPI document, nil when Unchanged.
	Document  map[string]any
	ETag      string
	RequestID string
}

// Schema fetches a model's schemas. Pass the ETag from an earlier call to get
// Unchanged instead of the document again.
func (m *Models) Schema(ctx context.Context, model, etag string) (SchemaResult, error) {
	target, err := m.modelURL(model, "/openapi.json")
	if err != nil {
		return SchemaResult{}, err
	}
	var header http.Header
	if etag != "" {
		for i := 0; i < len(etag); i++ {
			if etag[i] >= 0x80 || etag[i] < 0x20 {
				return SchemaResult{}, fmt.Errorf("etag must be printable ASCII; got %q", etag)
			}
		}
		header = http.Header{"If-None-Match": {etag}}
	}
	return withRetry(ctx, m.readPolicy(), func() (SchemaResult, error) {
		resp, data, err := m.t.do(ctx, http.MethodGet, target, nil, header.Clone())
		if err != nil {
			return SchemaResult{}, err
		}
		rid := cleanRequestID(resp.Header.Get("X-Comfy-Request-Id"))
		if resp.StatusCode == http.StatusNotModified && etag != "" {
			tag := resp.Header.Get("ETag")
			if tag == "" {
				tag = etag
			}
			return SchemaResult{Unchanged: true, ETag: tag, RequestID: rid}, nil
		}
		var doc map[string]any
		if err := decodeOrRaise(resp, data, &doc, http.StatusOK); err != nil {
			return SchemaResult{}, err
		}
		if doc == nil {
			return SchemaResult{}, catalogInvalid(fmt.Sprintf("The %d schema response is not a JSON object", resp.StatusCode), rid)
		}
		return SchemaResult{Document: doc, ETag: resp.Header.Get("ETag"), RequestID: rid}, nil
	})
}
