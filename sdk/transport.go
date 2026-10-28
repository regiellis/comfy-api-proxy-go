package sdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiPrefix      = "/api/v2"
	sseIdleTimeout = 45 * time.Second
)

// transport is the protocol layer: URL building, credentials, JSON and
// multipart bodies, and turning non-2xx replies into *APIError.
type transport struct {
	baseURL    string // no trailing slash
	originURL  string // scheme://host[:port] of baseURL
	baseOrigin origin
	// routerURL is Comfy Router's base, where client.Models sends requests.
	routerURL    string
	routerOrigin origin
	apiKey       string
	userAgent    string
	idle         time.Duration
	http         *http.Client
	// runs is http with a response-header limit long enough for a
	// synchronous model run.
	runs     *http.Client
	noFollow *http.Client
}

// origin is (scheme, host, effective port), compared to decide whether a URL
// may receive the bearer token.
type origin struct {
	scheme, host string
	port         string
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

func originOf(raw string) origin {
	u, err := url.Parse(raw)
	if err != nil {
		return origin{}
	}
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: u.Port()}
	if o.port == "" {
		o.port = defaultPorts[o.scheme]
	}
	return o
}

// redactUserinfo hides credentials embedded in a URL before it is shown. It
// works on the raw text, splitting the authority on its last '@' as the
// Python SDK does, so userinfo that would re-escape differently (a literal
// '@' in the password, a percent-escape) is still hidden.
func redactUserinfo(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	return scheme + "://***@" + authority[at+1:] + rest[end:]
}

// modelRunHeaderTimeout is how long a synchronous model run may take before
// its response starts: Router answers a run only when the generation is done.
const modelRunHeaderTimeout = 600 * time.Second

func newHTTPTransport(timeout, headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
	}
}

func newTransport(baseURL, routerURL, apiKey, clientInfo string, timeout time.Duration, hc *http.Client) (*transport, error) {
	base := strings.TrimRight(baseURL, "/")
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	ua, err := userAgent(clientInfo)
	if err != nil {
		return nil, err
	}
	runs := hc
	if hc == nil {
		hc = &http.Client{Transport: newHTTPTransport(timeout, timeout)}
		runs = &http.Client{Transport: newHTTPTransport(timeout, modelRunHeaderTimeout)}
	}
	router := strings.TrimRight(routerURL, "/")
	t := &transport{
		baseOrigin:   originOf(base),
		routerURL:    router,
		routerOrigin: originOf(router),
	}
	// The token belongs to the two configured origins alone, compared by
	// scheme, host, and port as httpx does. Go keeps Authorization on a
	// redirect to the same hostname whatever its scheme or port, and to
	// subdomains, so it is dropped on any hop that leaves them. A caller's
	// own redirect policy still runs after.
	guard := func(c *http.Client) *http.Client {
		follow := *c
		prev := c.CheckRedirect
		follow.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if !t.credentialed(req.URL.String()) {
				req.Header.Del("Authorization")
			}
			if prev != nil {
				return prev(req, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		return &follow
	}
	noFollow := *hc
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The origin keeps any userinfo from the base URL, so links the server
	// hands back (urls.self, urls.events) still carry basic auth.
	authority := u.Host
	if u.User != nil {
		authority = u.User.String() + "@" + u.Host
	}
	t.baseURL = base
	t.originURL = u.Scheme + "://" + authority
	t.apiKey = apiKey
	t.userAgent = ua
	t.idle = timeout
	t.http = guard(hc)
	t.runs = guard(runs)
	t.noFollow = &noFollow
	return t, nil
}

// credentialed reports whether a URL may receive the bearer token.
func (t *transport) credentialed(raw string) bool {
	o := originOf(raw)
	return o == t.baseOrigin || (t.routerURL != "" && o == t.routerOrigin)
}

// sdkVersion is this module's version when built as a dependency.
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "0"
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			return dep.Version
		}
	}
	if info.Main.Path == modulePath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "0"
}

const modulePath = "github.com/regiellis/comfy-api-proxy-go"

func userAgent(clientInfo string) (string, error) {
	ua := "comfy-sdk-go/" + sdkVersion() + " (" + runtime.Version() + ")"
	if clientInfo != "" {
		if strings.ContainsAny(clientInfo, "\r\n") {
			return "", errors.New("client info must not contain CR or LF characters")
		}
		ua += " app/" + clientInfo
	}
	return ua, nil
}

// url resolves a path: an absolute URL as is, an origin-relative path
// carrying /api/ (the links responses embed) against the origin, and
// anything else under the base URL's /api/v2.
func (t *transport) url(path string) string {
	switch {
	case strings.HasPrefix(path, "http"):
		return path
	case strings.HasPrefix(path, "/") && strings.Contains(path, "/api/"):
		return t.originURL + path
	}
	return t.baseURL + apiPrefix + path
}

// request builds a request with the SDK's headers. The bearer token goes
// only to the configured origin, never to a third host a response named.
func (t *transport) request(ctx context.Context, method, path string, body io.Reader, header http.Header) (*http.Request, error) {
	target := t.url(path)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("User-Agent", t.userAgent)
	if t.apiKey != "" && t.credentialed(target) {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// do sends a request and reads the whole reply.
func (t *transport) do(ctx context.Context, method, path string, body io.Reader, header http.Header) (*http.Response, []byte, error) {
	return t.doWith(ctx, t.http, method, path, body, header)
}

// doWith is do on a chosen client, such as runs for a model run.
func (t *transport) doWith(ctx context.Context, client *http.Client, method, path string, body io.Reader, header http.Header) (*http.Response, []byte, error) {
	req, err := t.request(ctx, method, path, body, header)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, &TransportError{Err: err}
	}
	data, readErr := io.ReadAll(resp.Body)
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		return nil, nil, &TransportError{Err: fmt.Errorf("read %s %s: %w", method, path, err)}
	}
	return resp, data, nil
}

// jsonCallReader sends a prepared JSON body and decodes a reply in ok.
func (t *transport) jsonCallReader(ctx context.Context, method, path string, body io.Reader, header http.Header, out any, ok ...int) error {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	resp, data, err := t.do(ctx, method, path, body, header)
	if err != nil {
		return err
	}
	return decodeOrRaise(resp, data, out, ok...)
}

// jsonCall sends v (when non-nil) as JSON and decodes a reply in ok into out.
func (t *transport) jsonCall(ctx context.Context, method, path string, v any, header http.Header, out any, ok ...int) error {
	var body io.Reader
	if v != nil {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode %s %s body: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
		if header == nil {
			header = http.Header{}
		}
		header.Set("Content-Type", "application/json")
	}
	resp, data, err := t.do(ctx, method, path, body, header)
	if err != nil {
		return err
	}
	return decodeOrRaise(resp, data, out, ok...)
}

func decodeOrRaise(resp *http.Response, data []byte, out any, ok ...int) error {
	for _, code := range ok {
		if resp.StatusCode != code {
			continue
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return &APIError{
				Code:        "invalid_response",
				Message:     fmt.Sprintf("Could not decode the %d response body as JSON", resp.StatusCode),
				HTTPStatus:  resp.StatusCode,
				RequestID:   cleanRequestID(resp.Header.Get("X-Comfy-Request-Id")),
				BodyExcerpt: cleanBodyExcerpt(string(data)),
			}
		}
		return nil
	}
	return errorFromResponse(resp.StatusCode, data, resp.Header.Get)
}

// stream sends a request and hands back a live body for the caller to read.
// A non-ok reply is read and turned into *APIError. The body is guarded by an
// idle watchdog: the request is cancelled when no bytes arrive for idle,
// which stands in for httpx's read timeout without capping a long download.
func (t *transport) stream(ctx context.Context, client *http.Client, method, path string, header http.Header, idle time.Duration, ok ...int) (*http.Response, error) {
	ctx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(nil) }
	req, err := t.request(ctx, method, path, nil, header)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, &TransportError{Err: err}
	}
	for _, code := range ok {
		if resp.StatusCode == code {
			resp.Body = newIdleBody(ctx, resp.Body, idle, cancelCause)
			return resp, nil
		}
	}
	data, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	cancel()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, &TransportError{Err: err}
	}
	return nil, errorFromResponse(resp.StatusCode, data, resp.Header.Get)
}

// ErrIdleTimeout reports a response body that sent nothing for the idle
// limit. It arrives wrapped in a *TransportError.
var ErrIdleTimeout = errors.New("response body idle too long")

// idleBody cancels its request when one Read waits longer than idle. The
// clock runs only inside Read, so time the consumer spends between reads (a
// slow writer, an event handler) never counts, matching httpx's read timeout.
type idleBody struct {
	ctx    context.Context
	rc     io.ReadCloser
	idle   time.Duration
	cancel context.CancelCauseFunc
	once   sync.Once
}

func newIdleBody(ctx context.Context, rc io.ReadCloser, idle time.Duration, cancel context.CancelCauseFunc) *idleBody {
	return &idleBody{ctx: ctx, rc: rc, idle: idle, cancel: cancel}
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.idle <= 0 {
		return b.rc.Read(p)
	}
	timer := time.AfterFunc(b.idle, func() { b.cancel(ErrIdleTimeout) })
	n, err := b.rc.Read(p)
	timer.Stop()
	if err != nil && errors.Is(context.Cause(b.ctx), ErrIdleTimeout) {
		return n, &TransportError{Err: ErrIdleTimeout}
	}
	return n, err
}

func (b *idleBody) Close() error {
	var err error
	b.once.Do(func() {
		err = b.rc.Close()
		b.cancel(nil)
	})
	return err
}

func newBoundary() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate multipart boundary: %w", err)
	}
	return "----comfy" + hex.EncodeToString(b[:]), nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// headerSafe reports whether s can sit in a multipart part header.
func headerSafe(s string) bool { return !strings.ContainsAny(s, "\r\n") }

// multipartBody streams text fields, then the file part, without buffering
// the file. It returns the body and its exact length when size is known.
func multipartBody(boundary string, fields [][2]string, fileName, contentType string, file io.Reader, size int64) (io.Reader, int64) {
	var head bytes.Buffer
	for _, f := range fields {
		head.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + f[0] + "\"\r\n\r\n" + f[1] + "\r\n")
	}
	head.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"" +
		quoteEscaper.Replace(fileName) + "\"\r\nContent-Type: " + contentType + "\r\n\r\n")
	tail := "\r\n--" + boundary + "--\r\n"
	length := int64(-1)
	if size >= 0 {
		length = int64(head.Len()) + size + int64(len(tail))
	}
	return io.MultiReader(&head, file, strings.NewReader(tail)), length
}

// mediaType is a Content-Type reduced to its lower-case media type.
func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		head, _, _ := strings.Cut(ct, ";")
		return strings.ToLower(strings.TrimSpace(head))
	}
	return mt
}

// parseExpiry reads a GCS signed URL's X-Goog-Date and X-Goog-Expires.
func parseExpiry(raw string) (time.Time, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	q := u.Query()
	date, expires := q.Get("X-Goog-Date"), q.Get("X-Goog-Expires")
	if date == "" || expires == "" {
		return time.Time{}, false
	}
	issued, err := time.Parse("20060102T150405Z", date)
	if err != nil {
		return time.Time{}, false
	}
	// A value too large for a time.Duration reads as no expiry, as an
	// overflowing datetime addition does in the Python SDK.
	seconds, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || seconds < 0 || seconds > 1<<40 {
		return time.Time{}, false
	}
	return issued.Add(time.Duration(seconds) * time.Second).UTC(), true
}
