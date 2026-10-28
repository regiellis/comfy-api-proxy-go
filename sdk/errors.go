package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Sentinel errors for the failure kinds a caller branches on. Every error the
// API returns is an *APIError, and errors.Is matches it against these by its
// code, so callers write errors.Is(err, sdk.ErrNotFound).
var (
	ErrMissingAPIKey       = errors.New("missing API key")
	ErrNotFound            = errors.New("not found")
	ErrInvalidWorkflow     = errors.New("invalid workflow")
	ErrWorkflowFormatUI    = errors.New("workflow is in UI-export format")
	ErrMissingAsset        = errors.New("missing asset")
	ErrHashMismatch        = errors.New("hash mismatch")
	ErrBlobNotFound        = errors.New("blob not found")
	ErrIdempotencyKeyReuse = errors.New("idempotency key reuse")
	ErrQueueFull           = errors.New("queue full")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
	ErrInsufficientCredits = errors.New("insufficient credits")
	ErrJobFailed           = errors.New("job failed")
	ErrInvalidResponse     = errors.New("invalid response")
)

// Comfy Router error buckets, one per x-comfy-error-types value in the
// Router contract, in its order. ErrRouter matches any of them, and any
// error the Router labelled with a bucket this SDK does not know yet.
var (
	ErrRouter                   = errors.New("router error")
	ErrInvalidInput             = errors.New("invalid input")
	ErrContentPolicyViolation   = errors.New("content policy violation")
	ErrProviderError            = errors.New("provider error")
	ErrProviderTimeout          = errors.New("provider timeout")
	ErrModelNotFound            = errors.New("model not found")
	ErrConcurrencyLimitExceeded = errors.New("concurrency limit exceeded")
	ErrClientDisconnected       = errors.New("client disconnected")
	ErrInternalError            = errors.New("internal error")
	ErrDeadlineExceeded         = errors.New("deadline exceeded")
	ErrNotEnabled               = errors.New("not enabled")
	ErrServiceUnavailable       = errors.New("service unavailable")
	ErrRateLimited              = errors.New("rate limited")
	ErrCancelled                = errors.New("cancelled")
	ErrQueueTimeout             = errors.New("queue timeout")
	ErrRequestNotFound          = errors.New("request not found")
	ErrQueueBacklogFull         = errors.New("queue backlog full")
	// ErrCancelRefused is a 409 refusal to cancel; ErrAlreadyCompleted, the
	// one refusal the contract names, also matches it.
	ErrCancelRefused    = errors.New("cancel refused")
	ErrAlreadyCompleted = errors.New("already completed")
)

// RouterErrorTypes lists the Router buckets in contract order.
var RouterErrorTypes = []string{
	"invalid_input", "content_policy_violation", "provider_error", "provider_timeout",
	"insufficient_credits", "model_not_found", "unauthorized", "forbidden",
	"concurrency_limit_exceeded", "client_disconnected", "internal_error", "deadline_exceeded",
	"not_enabled", "service_unavailable", "rate_limited", "cancelled", "queue_timeout",
	"request_not_found", "queue_backlog_full",
}

var routerSentinels = map[string]error{
	"invalid_input":              ErrInvalidInput,
	"content_policy_violation":   ErrContentPolicyViolation,
	"provider_error":             ErrProviderError,
	"provider_timeout":           ErrProviderTimeout,
	"insufficient_credits":       ErrInsufficientCredits,
	"model_not_found":            ErrModelNotFound,
	"unauthorized":               ErrUnauthorized,
	"forbidden":                  ErrForbidden,
	"concurrency_limit_exceeded": ErrConcurrencyLimitExceeded,
	"client_disconnected":        ErrClientDisconnected,
	"internal_error":             ErrInternalError,
	"deadline_exceeded":          ErrDeadlineExceeded,
	"not_enabled":                ErrNotEnabled,
	"service_unavailable":        ErrServiceUnavailable,
	"rate_limited":               ErrRateLimited,
	"cancelled":                  ErrCancelled,
	"queue_timeout":              ErrQueueTimeout,
	"request_not_found":          ErrRequestNotFound,
	"queue_backlog_full":         ErrQueueBacklogFull,
	"already_completed":          ErrAlreadyCompleted,
}

// ValidationErrorDetail is one entry of a Router validation response.
type ValidationErrorDetail struct {
	Loc   []any
	Msg   string
	Type  string
	Ctx   map[string]any
	Input any
}

// Location joins Loc into a dotted path such as "body.prompt".
func (d ValidationErrorDetail) Location() string {
	var parts []string
	for _, p := range d.Loc {
		switch v := p.(type) {
		case string:
			parts = append(parts, v)
		case float64:
			if v == float64(int64(v)) {
				parts = append(parts, strconv.FormatInt(int64(v), 10))
			}
		}
	}
	return strings.Join(parts, ".")
}

// sentinelByCode maps wire codes to sentinels. Several codes share one, as
// the not-found family does.
var sentinelByCode = map[string]error{
	"invalid_workflow":      ErrInvalidWorkflow,
	"workflow_format_ui":    ErrWorkflowFormatUI,
	"missing_asset":         ErrMissingAsset,
	"hash_mismatch":         ErrHashMismatch,
	"blob_not_found":        ErrBlobNotFound,
	"idempotency_key_reuse": ErrIdempotencyKeyReuse,
	"queue_full":            ErrQueueFull,
	"insufficient_credits":  ErrInsufficientCredits,
	"not_found":             ErrNotFound,
	"job_not_found":         ErrNotFound,
	"asset_not_found":       ErrNotFound,
	"unauthorized":          ErrUnauthorized,
	"forbidden":             ErrForbidden,
	"missing_api_key":       ErrMissingAPIKey,
	"invalid_response":      ErrInvalidResponse,
}

// codeByStatus names the failure when the server sent no code of its own.
var codeByStatus = map[int]string{
	401: "unauthorized",
	402: "insufficient_credits",
	403: "forbidden",
	404: "not_found",
	422: "invalid_workflow",
	429: "queue_full",
}

// APIError is a failure the API reported, or one the SDK refused locally
// before sending anything. Fields the failure did not carry are zero.
type APIError struct {
	// Code is the wire code, such as "not_found", or "http_<status>" when the
	// server named none the SDK recognizes.
	Code       string
	Message    string
	HTTPStatus int
	Details    map[string]any
	// RequestID is the server's X-Comfy-Request-Id, cleaned to a bounded,
	// printable token so it is safe to log.
	RequestID string
	// RetryAfter is the Retry-After header in seconds, nil when absent.
	RetryAfter *int
	// IdempotencyKey is the key a failed submit was sent under, so a caller
	// can poll or list for the job an ambiguous failure may have created.
	IdempotencyKey string
	// BodyExcerpt is a cleaned, bounded slice of a body that carried no
	// message, such as a proxy's HTML error page.
	BodyExcerpt string
	// ErrorType is a Comfy Router error bucket, when the server sent one.
	ErrorType string
	// Validation holds a Router validation response's entries.
	Validation []ValidationErrorDetail
	// ResendRefused marks a model run whose retry was refused as a key
	// reuse after an earlier attempt may have claimed the key. The error
	// reported is that earlier attempt's; the generation may have happened
	// and been billed, so collect it rather than starting a new one.
	ResendRefused bool
}

// Error is the server's message, followed by a body excerpt when the
// response carried no message.
func (e *APIError) Error() string {
	if e.BodyExcerpt != "" {
		return e.Message + ": " + e.BodyExcerpt
	}
	return e.Message
}

// Is matches the sentinel for the error's code, mirroring the Python SDK's
// class tree: a UI-format workflow is also an invalid workflow, an
// already-completed refusal is also a cancel refusal, and anything the Router
// labelled with a bucket is an ErrRouter.
func (e *APIError) Is(target error) bool {
	if target == ErrRouter {
		return e.ErrorType != "" || routerSentinels[e.Code] != nil
	}
	if target == ErrCancelRefused && e.Code == "already_completed" {
		return true
	}
	if s, ok := sentinelByCode[e.Code]; ok {
		if s == target {
			return true
		}
		if s == ErrWorkflowFormatUI && target == ErrInvalidWorkflow {
			return true
		}
	}
	return routerSentinels[e.Code] == target
}

// JobFailedError reports a job that reached a terminal state other than
// succeeded. errors.Is(err, ErrJobFailed) matches it.
type JobFailedError struct {
	JobID  string
	Status string
	// Err is the server's account of the failure, nil when it gave none.
	Err *JobError
}

// Error names the job and the status it ended in.
func (e *JobFailedError) Error() string {
	return fmt.Sprintf("job %s ended %s", e.JobID, e.Status)
}

// Is matches ErrJobFailed.
func (e *JobFailedError) Is(target error) bool { return target == ErrJobFailed }

// TransportError wraps a failure that never produced a response: refused
// connection, timeout, a stream cut mid-body. It keeps the idempotency key a
// submit was sent under. Whether the server saw the request is unknown, so
// poll or list before resubmitting under the same key.
type TransportError struct {
	IdempotencyKey string
	// ResendRefused: see APIError.ResendRefused.
	ResendRefused bool
	Err           error
}

// Error is the underlying failure's message.
func (e *TransportError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying failure, so errors.Is can find
// ErrIdleTimeout or context.Canceled.
func (e *TransportError) Unwrap() error { return e.Err }

// stampKey records the idempotency key on whatever submit lets out.
func stampKey(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	var api *APIError
	if errors.As(err, &api) {
		if api.IdempotencyKey == "" {
			api.IdempotencyKey = key
		}
		return err
	}
	var te *TransportError
	if errors.As(err, &te) {
		if te.IdempotencyKey == "" {
			te.IdempotencyKey = key
		}
		return err
	}
	return &TransportError{IdempotencyKey: key, Err: err}
}

func localError(code, message string) *APIError {
	return &APIError{Code: code, Message: message}
}

const (
	maxRequestID      = 200
	bodyExcerptLimit  = 256
	bodyExcerptWindow = 8
)

// cleanRequestID keeps the leading run of request-id characters, at most 200,
// so a hostile header cannot inject control characters into a log line.
func cleanRequestID(raw string) string {
	s := strings.TrimSpace(raw)
	n := 0
	for n < len(s) && n < maxRequestID && isRequestIDByte(s[n]) {
		n++
	}
	return s[:n]
}

func isRequestIDByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("._:+/=@-", c) >= 0
}

// cleanBodyExcerpt collapses whitespace, blanks unprintable characters, and
// bounds the result to 256 characters.
func cleanBodyExcerpt(raw string) string {
	head := []rune(strings.TrimLeftFunc(raw, unicode.IsSpace))
	if len(head) > bodyExcerptLimit*bodyExcerptWindow {
		head = head[:bodyExcerptLimit*bodyExcerptWindow]
	}
	for i, r := range head {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			head[i] = ' '
		}
	}
	collapsed := []rune(strings.Join(strings.Fields(string(head)), " "))
	if len(collapsed) > bodyExcerptLimit {
		collapsed = collapsed[:bodyExcerptLimit]
	}
	return string(collapsed)
}

// errorFromResponse reads the error envelope {"error": {code, message,
// details}}, falling back to a Router error_type, a FastAPI-style detail
// array, and finally the HTTP status.
func errorFromResponse(status int, body []byte, header func(string) string) *APIError {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		parsed = nil
	}
	envelope, _ := parsed["error"].(map[string]any)
	str := func(m map[string]any, k string) string {
		s, _ := m[k].(string)
		return strings.TrimSpace(s)
	}

	code := str(envelope, "code")
	message := cleanBodyExcerpt(str(envelope, "message"))
	details, _ := envelope["details"].(map[string]any)
	bucket := strings.TrimSpace(header("X-Comfy-Error-Type"))
	if bucket == "" {
		bucket = str(parsed, "error_type")
	}
	if code == "" {
		code = bucket
	}
	if code == "" && status == 409 && strings.EqualFold(str(parsed, "status"), "ALREADY_COMPLETED") {
		code = "already_completed"
	}
	if code == "" {
		code = codeByStatus[status]
	}
	if code == "" {
		code = "http_" + strconv.Itoa(status)
	}
	var validation []ValidationErrorDetail
	if d, ok := parsed["detail"].([]any); ok {
		validation = validationEntries(d)
	}
	if message == "" {
		switch d := parsed["detail"].(type) {
		case []any:
			message = summariseDetail(d)
		case string:
			message = cleanBodyExcerpt(d)
		}
	}
	excerpt := ""
	if message == "" {
		message = "HTTP " + strconv.Itoa(status)
		excerpt = cleanBodyExcerpt(string(body))
	}
	return &APIError{
		Code:        code,
		Message:     message,
		HTTPStatus:  status,
		Details:     details,
		RequestID:   cleanRequestID(header("X-Comfy-Request-Id")),
		RetryAfter:  retryAfter(header("Retry-After")),
		BodyExcerpt: excerpt,
		ErrorType:   bucket,
		Validation:  validation,
	}
}

func validationEntries(entries []any) []ValidationErrorDetail {
	var out []ValidationErrorDetail
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		d := ValidationErrorDetail{Input: m["input"]}
		if loc, ok := m["loc"].([]any); ok {
			for _, p := range loc {
				switch v := p.(type) {
				case string:
					d.Loc = append(d.Loc, v)
				case float64:
					if v == float64(int64(v)) {
						d.Loc = append(d.Loc, v)
					}
				}
			}
		}
		d.Msg, _ = m["msg"].(string)
		d.Type, _ = m["type"].(string)
		d.Ctx, _ = m["ctx"].(map[string]any)
		out = append(out, d)
	}
	return out
}

// errorFromCompletion reads a queued request's terminal payload. A payload
// naming an error_type is a failed generation; nil means it completed.
func errorFromCompletion(payload map[string]any, requestID string) *APIError {
	bucket, _ := payload["error_type"].(string)
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil
	}
	e := &APIError{Code: bucket, ErrorType: bucket, RequestID: cleanRequestID(requestID)}
	switch d := payload["detail"].(type) {
	case string:
		e.Message = cleanBodyExcerpt(d)
	case []any:
		e.Validation = validationEntries(d)
		e.Message = summariseDetail(d)
	}
	if e.Message == "" {
		e.Message = "the request completed with error_type '" + bucket + "'"
	}
	return e
}

// summariseDetail renders a validation array as "loc: msg; loc: msg".
func summariseDetail(entries []any) string {
	var parts []string
	size := 0
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		var loc []string
		if raw, ok := m["loc"].([]any); ok {
			for _, p := range raw {
				switch v := p.(type) {
				case string:
					loc = append(loc, v)
				case float64:
					if v == float64(int64(v)) {
						loc = append(loc, strconv.FormatInt(int64(v), 10))
					}
				}
			}
		}
		location := strings.Join(loc, ".")
		msg, _ := m["msg"].(string)
		msg = strings.TrimSpace(msg)
		part := location
		switch {
		case location != "" && msg != "":
			part = location + ": " + msg
		case msg != "":
			part = msg
		case location == "":
			continue
		}
		cleaned := cleanBodyExcerpt(part)
		if cleaned == "" {
			continue
		}
		parts = append(parts, cleaned)
		size += len(cleaned) + 2
		if size >= bodyExcerptLimit*bodyExcerptWindow {
			break
		}
	}
	return cleanBodyExcerpt(strings.Join(parts, "; "))
}

func retryAfter(raw string) *int {
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if errors.Is(err, strconv.ErrRange) {
		// Too large to hold: the server means "a very long time", which
		// the retry budget then caps, as Python's unbounded int does.
		n = maxRetryAfter
		if strings.HasPrefix(strings.TrimSpace(raw), "-") {
			n = -maxRetryAfter
		}
		return &n
	}
	if err != nil {
		return nil
	}
	return &n
}

// maxRetryAfter keeps seconds * time.Second inside time.Duration.
const maxRetryAfter = 1 << 32
