package sdk

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func headers(kv ...string) func(string) string {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h.Get
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		hdr      func(string) string
		code     string
		message  string
		sentinel error
	}{
		{"404 codes map to not found", 404, `{"error":{"code":"job_not_found","message":"gone"}}`, headers(), "job_not_found", "gone", ErrNotFound},
		{"asset_not_found too", 404, `{"error":{"code":"asset_not_found","message":"gone"}}`, headers(), "asset_not_found", "gone", ErrNotFound},
		{"envelope code wins over status", 422, `{"error":{"code":"missing_asset","message":"m"}}`, headers(), "missing_asset", "m", ErrMissingAsset},
		{"router body error_type is the code", 400, `{"error_type":"invalid_request"}`, headers(), "invalid_request", "HTTP 400", nil},
		{"router header error_type is the code", 400, `{}`, headers("X-Comfy-Error-Type", "rate_limited"), "rate_limited", "HTTP 400", nil},
		{"bucketless 429 means queue full", 429, ``, headers(), "queue_full", "HTTP 429", ErrQueueFull},
		{"bucketless 409 is not guessed a hash mismatch", 409, `{"error":{"message":"conflict here"}}`, headers(), "http_409", "conflict here", nil},
		{"enveloped 409 is a hash mismatch", 409, `{"error":{"code":"hash_mismatch","message":"m"}}`, headers(), "hash_mismatch", "m", ErrHashMismatch},
		{"empty envelope code reads as absent", 404, `{"error":{"code":"  ","message":"m"}}`, headers(), "not_found", "m", ErrNotFound},
		{"bodyless 401 is unauthorized", 401, ``, headers(), "unauthorized", "HTTP 401", ErrUnauthorized},
		{"402 is insufficient credits", 402, ``, headers(), "insufficient_credits", "HTTP 402", ErrInsufficientCredits},
		{"unknown status names itself", 418, `{}`, headers(), "http_418", "HTTP 418", nil},
		{"string detail is the message", 400, `{"detail":"bad thing"}`, headers(), "http_400", "bad thing", nil},
		{"validation array summarised", 422, `{"detail":[{"loc":["body","seed"],"msg":"not an int"},{"loc":["query",3]}]}`, headers(), "invalid_workflow", "body.seed: not an int; query.3", ErrInvalidWorkflow},
		{"envelope message outranks detail", 422, `{"error":{"code":"invalid_workflow","message":"env"},"detail":[{"msg":"x"}]}`, headers(), "invalid_workflow", "env", ErrInvalidWorkflow},
		{"409 ALREADY_COMPLETED", 409, `{"status":"already_completed"}`, headers(), "already_completed", "HTTP 409", nil},
		{"ui format is also an invalid workflow", 422, `{"error":{"code":"workflow_format_ui","message":"m"}}`, headers(), "workflow_format_ui", "m", ErrInvalidWorkflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := errorFromResponse(tc.status, []byte(tc.body), tc.hdr)
			if e.Code != tc.code || e.Message != tc.message || e.HTTPStatus != tc.status {
				t.Fatalf("got code=%q message=%q status=%d", e.Code, e.Message, e.HTTPStatus)
			}
			if tc.sentinel != nil && !errors.Is(e, tc.sentinel) {
				t.Fatalf("%v is not %v", e, tc.sentinel)
			}
		})
	}
}

func TestBodyExcerptOnlyWhenNoMessage(t *testing.T) {
	e := errorFromResponse(502, []byte("<html>\n  <body>no healthy\tupstream</body></html>"), headers())
	if e.Message != "HTTP 502" || e.BodyExcerpt != "<html> <body>no healthy upstream</body></html>" {
		t.Fatalf("got %q / %q", e.Message, e.BodyExcerpt)
	}
	if !strings.Contains(e.Error(), "no healthy upstream") {
		t.Fatalf("Error() = %q", e.Error())
	}
	sent := errorFromResponse(500, []byte(`{"error":{"code":"boom","message":"said so"}}`), headers())
	if sent.BodyExcerpt != "" {
		t.Fatalf("a message the server sent carried an excerpt too: %q", sent.BodyExcerpt)
	}
	long := errorFromResponse(500, []byte(strings.Repeat("x", 5000)), headers())
	if len(long.BodyExcerpt) != bodyExcerptLimit {
		t.Fatalf("excerpt length %d", len(long.BodyExcerpt))
	}
}

func TestRequestIDAndRetryAfter(t *testing.T) {
	e := errorFromResponse(429, nil, headers("X-Comfy-Request-Id", "req-1.2:3\r\nX-Evil: 1", "Retry-After", "7"))
	if e.RequestID != "req-1.2:3" {
		t.Fatalf("request id = %q", e.RequestID)
	}
	if e.RetryAfter == nil || *e.RetryAfter != 7 {
		t.Fatalf("retry after = %v", e.RetryAfter)
	}
	if got := cleanRequestID(strings.Repeat("a", 300)); len(got) != maxRequestID {
		t.Fatalf("request id not bounded: %d", len(got))
	}
	if errorFromResponse(429, nil, headers("Retry-After", "soon")).RetryAfter != nil {
		t.Fatal("unparseable Retry-After should read as absent")
	}
	if ra := errorFromResponse(429, nil, headers("Retry-After", "-1")).RetryAfter; ra == nil || *ra != -1 {
		t.Fatal("a negative Retry-After the server sent must stay distinct from absent")
	}
}

func TestStampKey(t *testing.T) {
	api := &APIError{Code: "x"}
	if err := stampKey(api, "k1"); err != api || api.IdempotencyKey != "k1" {
		t.Fatalf("api error not stamped: %v %q", err, api.IdempotencyKey)
	}
	if err := stampKey(api, "k2"); err != api || api.IdempotencyKey != "k1" {
		t.Fatal("stamp overwrote a key already set")
	}
	var te *TransportError
	if !errors.As(stampKey(errors.New("dial refused"), "k3"), &te) || te.IdempotencyKey != "k3" {
		t.Fatalf("plain error not wrapped with the key: %v", te)
	}
	if stampKey(nil, "k") != nil {
		t.Fatal("nil error stamped into something")
	}
}

func TestJobFailedIs(t *testing.T) {
	err := error(&JobFailedError{JobID: "j", Status: StatusFailed})
	if !errors.Is(err, ErrJobFailed) || err.Error() != "job j ended failed" {
		t.Fatalf("%v", err)
	}
}
