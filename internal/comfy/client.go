// Package comfy is the HTTP client for ComfyUI's native API.
package comfy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"time"
)

// ErrUnreachable marks a transport failure: ComfyUI is down, restarting, or
// the connection broke. Handlers map it to 503 upstream_unreachable.
var ErrUnreachable = errors.New("comfyui unreachable")

// Client talks to one ComfyUI. Requests carry no overall timeout, only a
// connect timeout, matching the reference: /view can stream large outputs
// and a history call against a busy server can be slow.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a client for base, a URL with no trailing slash. A non-empty
// token is sent as a bearer token on every request to ComfyUI, the
// WebSocket handshake included, for a ComfyUI behind an authenticating
// gateway.
func New(base, token string) *Client {
	// No environment proxy: ComfyUI traffic carries the partner API key and
	// is meant for one known host, matching aiohttp's trust_env=False default.
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	var rt http.RoundTripper = transport
	if token != "" {
		rt = bearer{token: token, next: transport}
	}
	return &Client{Base: base, HTTP: &http.Client{Transport: rt}}
}

// bearer adds the upstream token to each request. The request is cloned,
// since a RoundTripper must not modify the caller's.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// Response is a fully read upstream reply.
type Response struct {
	Status      int
	ContentType string // media type without parameters
	Body        []byte
}

// IsJSON reports whether the reply declared a JSON body.
func (r *Response) IsJSON() bool { return r.ContentType == "application/json" }

// Do sends a request and reads the whole reply. A body value is sent as JSON.
func (c *Client) Do(ctx context.Context, method, path string, body any) (*Response, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s body: %w", method, path, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrUnreachable, method, path, err)
	}
	defer closeQuietly(resp.Body)
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s %s: %v", ErrUnreachable, method, path, err)
	}
	return &Response{Status: resp.StatusCode, ContentType: mediaType(resp.Header.Get("Content-Type")), Body: data}, nil
}

// GetJSON fetches path and returns its JSON body. ok is false when the reply
// was not a 200 JSON body, which callers treat as "nothing there". A body
// that is not valid JSON is reported as unreachable, since the server on the
// other end is not behaving as ComfyUI. Callers decode the raw body into the
// shape they expect and treat a shape mismatch as absent, as the reference's
// isinstance checks do.
func (c *Client) GetJSON(ctx context.Context, path string) (json.RawMessage, bool, error) {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, err
	}
	if resp.Status != http.StatusOK || !resp.IsJSON() {
		return nil, false, nil
	}
	if !json.Valid(resp.Body) {
		return nil, false, fmt.Errorf("%w: %s returned invalid JSON", ErrUnreachable, path)
	}
	return resp.Body, true, nil
}

// Size asks for path with HEAD and returns its Content-Length. ok is false
// when the reply was not a 200 with a length, which callers read as unknown.
func (c *Client) Size(ctx context.Context, path string) (size int64, ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.Base+path, nil)
	if err != nil {
		return 0, false, fmt.Errorf("build HEAD %s: %w", path, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("%w: HEAD %s: %v", ErrUnreachable, path, err)
	}
	closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 0 {
		return 0, false, nil
	}
	return resp.ContentLength, true, nil
}

// Stream opens a request and hands back the live response for the caller to
// copy and close. Used for /view, where outputs can be large.
func (c *Client) Stream(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", path, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrUnreachable, path, err)
	}
	return resp, nil
}

// PostMultipart sends a prebuilt multipart body.
func (c *Client) PostMultipart(ctx context.Context, path, contentType string, body io.Reader) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: POST %s: %v", ErrUnreachable, path, err)
	}
	defer closeQuietly(resp.Body)
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read POST %s: %v", ErrUnreachable, path, err)
	}
	return &Response{Status: resp.StatusCode, ContentType: mediaType(resp.Header.Get("Content-Type")), Body: data}, nil
}

func mediaType(header string) string {
	if header == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return mt
}

// closeQuietly closes a response body whose contents are already consumed;
// a close error there carries no information the caller can act on.
func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}
