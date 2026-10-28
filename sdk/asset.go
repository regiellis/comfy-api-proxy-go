package sdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"lukechampine.com/blake3"
)

const defaultContentType = "application/octet-stream"

// Asset is an input file. It is lazy: nothing is hashed or sent until
// Commit, which a Submit carrying it in the graph calls. Commit first asks
// the server whether it already holds identical bytes (by blake3 hash) and
// only uploads when it does not. An Asset is safe for concurrent use.
type Asset struct {
	t           *transport
	contentType string
	filePath    string
	hashFn      func() (string, error)
	open        func() (io.ReadCloser, int64, error)
	key         string

	// op serializes Commit, Delete, and Reference, and is taken with the
	// caller's context so a waiting call can give up. mu guards the fields
	// below and is only ever held briefly, so the getters never wait on an
	// upload. hashMu covers computing the hash, which can read a large file.
	op         chan struct{}
	hashMu     sync.Mutex
	mu         sync.Mutex
	hash       string
	id         string
	createdNew *bool
	url        string
	jobID      *string
	expiresAt  *time.Time
}

func guessContentType(name string) string {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			return mt
		}
	}
	return defaultContentType
}

func newAsset(t *transport, contentType, filePath string, hashFn func() (string, error), open func() (io.ReadCloser, int64, error)) *Asset {
	return &Asset{t: t, contentType: contentType, filePath: filePath, hashFn: hashFn, open: open, op: make(chan struct{}, 1)}
}

func (a *Asset) acquire(ctx context.Context) error {
	select {
	case a.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Asset) release() { <-a.op }

// AssetFromFile streams a file from disk, hashed and uploaded on commit.
func (c *Client) AssetFromFile(p string) *Asset {
	name := filepath.Base(p)
	return newAsset(c.t, guessContentType(name), name,
		func() (string, error) { return hashFile(p) },
		func() (io.ReadCloser, int64, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, 0, err
			}
			info, err := f.Stat()
			if err != nil {
				return nil, 0, errors.Join(err, f.Close())
			}
			return f, info.Size(), nil
		})
}

// AssetFromBytes uploads an in-memory buffer. An empty filename becomes
// "upload.bin"; an empty content type is guessed from the filename.
func (c *Client) AssetFromBytes(data []byte, filename, contentType string) *Asset {
	name := filename
	if name == "" {
		name = "upload.bin"
	}
	if contentType == "" {
		contentType = guessContentType(filename)
	}
	return newAsset(c.t, contentType, name,
		func() (string, error) { return hashBytes(data), nil },
		func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
		})
}

// AssetFromReader reads r to the end and uploads the bytes.
func (c *Client) AssetFromReader(r io.Reader, filename, contentType string) (*Asset, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	return c.AssetFromBytes(data, filename, contentType), nil
}

// AssetFromURL downloads url and uploads its bytes. The client's bearer
// token goes with the download only when url is on the client's own origin,
// and the download is cancelled if the body stalls for the client's timeout.
func (c *Client) AssetFromURL(ctx context.Context, rawURL string) (*Asset, error) {
	shown := safeURL(rawURL)
	resp, err := c.t.stream(ctx, c.t.http, http.MethodGet, rawURL, nil, c.t.idle,
		http.StatusOK, http.StatusNonAuthoritativeInfo, http.StatusPartialContent)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return nil, fmt.Errorf("download %s: HTTP %d", shown, api.HTTPStatus)
		}
		return nil, fmt.Errorf("download %s: %w", shown, err)
	}
	data, readErr := io.ReadAll(resp.Body)
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		return nil, fmt.Errorf("download %s: %w", shown, err)
	}
	name := "download.bin"
	if u, err := url.Parse(rawURL); err == nil {
		if b := path.Base(u.Path); b != "" && b != "/" && b != "." && headerSafe(b) {
			name = b
		}
	}
	return c.AssetFromBytes(data, name, mediaType(resp.Header.Get("Content-Type"))), nil
}

// safeURL drops userinfo, query, and fragment, which can carry credentials
// or a signature, before a URL goes into an error message.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the given URL"
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}

// GetAsset fetches an already-committed asset.
func (c *Client) GetAsset(ctx context.Context, id string) (*Asset, error) {
	var w wireAsset
	if err := c.t.jsonCall(ctx, http.MethodGet, "/assets/"+url.PathEscape(id), nil, nil, &w, http.StatusOK); err != nil {
		return nil, err
	}
	fp := id
	if w.FilePath != nil {
		fp = *w.FilePath
	}
	hash := ""
	if w.Hash != nil {
		hash = *w.Hash
	}
	a := newAsset(c.t, w.ContentType, fp,
		func() (string, error) { return hash, nil },
		func() (io.ReadCloser, int64, error) {
			return nil, 0, errors.New("this asset is already committed; nothing to upload")
		})
	a.apply(w)
	return a, nil
}

// DeleteAsset deletes an asset by id.
func (c *Client) DeleteAsset(ctx context.Context, id string) error {
	return c.t.jsonCall(ctx, http.MethodDelete, "/assets/"+url.PathEscape(id), nil, nil, nil, http.StatusNoContent)
}

// ID is the server's asset id, empty until committed.
func (a *Asset) ID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

// FilePath is the name the asset is uploaded under.
func (a *Asset) FilePath() string { return a.filePath }

// CreatedNew reports whether the commit stored new bytes (true) or matched
// bytes the server already had (false). Nil before commit.
func (a *Asset) CreatedNew() *bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.createdNew
}

// JobID is the job that produced this asset; nil for uploads.
func (a *Asset) JobID() *string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.jobID
}

// ExpiresAt is the asset's retention deadline, nil when it has none.
func (a *Asset) ExpiresAt() *time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expiresAt
}

// Hash is the local blake3 hash, computed once. It is only a dedup hint; the
// server recomputes it from the bytes it receives.
func (a *Asset) Hash() (string, error) {
	a.hashMu.Lock()
	defer a.hashMu.Unlock()
	a.mu.Lock()
	known := a.hash
	a.mu.Unlock()
	if known != "" {
		return known, nil
	}
	h, err := a.hashFn()
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", a.filePath, err)
	}
	a.mu.Lock()
	a.hash = h
	a.mu.Unlock()
	return h, nil
}

func (a *Asset) apply(w wireAsset) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.id = w.ID
	if w.Hash != nil && *w.Hash != "" {
		a.hash = *w.Hash
	}
	a.createdNew = w.CreatedNew
	a.url = w.URL
	a.jobID = w.JobID
	a.expiresAt = w.ExpiresAt
}

// Commit makes the asset exist server-side and returns its id. Idempotent:
// a committed asset returns at once. A retried upload reuses one
// Idempotency-Key per asset, so it cannot create a second copy.
func (a *Asset) Commit(ctx context.Context) (string, error) {
	if err := a.acquire(ctx); err != nil {
		return "", err
	}
	defer a.release()
	return a.commit(ctx)
}

// commit runs with op held.
func (a *Asset) commit(ctx context.Context) (string, error) {
	if id := a.ID(); id != "" {
		return id, nil
	}
	digest, err := a.Hash()
	if err != nil {
		return "", err
	}
	exists, err := a.t.headAssetByHash(ctx, digest)
	if err != nil {
		return "", err
	}
	var w wireAsset
	if exists {
		err = a.t.jsonCall(ctx, http.MethodPost, "/assets/from-hash",
			map[string]any{"hash": digest, "file_path": a.filePath}, nil, &w, http.StatusOK, http.StatusCreated)
	} else {
		w, err = a.upload(ctx, digest)
	}
	if err != nil {
		return "", err
	}
	a.apply(w)
	return w.ID, nil
}

func (a *Asset) upload(ctx context.Context, digest string) (wireAsset, error) {
	// The filename and content type go into a part header, where a CR or LF
	// would start a header of the caller's choosing.
	if !headerSafe(a.filePath) || !headerSafe(a.contentType) {
		return wireAsset{}, errors.New("asset file path and content type must not contain CR or LF")
	}
	if a.key == "" {
		k, err := newUUID()
		if err != nil {
			return wireAsset{}, err
		}
		a.key = k
	}
	file, size, err := a.open()
	if err != nil {
		return wireAsset{}, fmt.Errorf("open %s: %w", a.filePath, err)
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			return // read-only source, already fully sent or abandoned
		}
	}()
	boundary, err := newBoundary()
	if err != nil {
		return wireAsset{}, err
	}
	fields := [][2]string{{"content_type", a.contentType}, {"file_path", a.filePath}, {"expected_hash", digest}}
	body, length := multipartBody(boundary, fields, a.filePath, a.contentType, file, size)
	req, err := a.t.request(ctx, http.MethodPost, "/assets", body, http.Header{
		"Content-Type":    {"multipart/form-data; boundary=" + boundary},
		"Idempotency-Key": {a.key},
	})
	if err != nil {
		return wireAsset{}, err
	}
	if length >= 0 {
		req.ContentLength = length
		req.Header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	resp, err := a.t.http.Do(req)
	if err != nil {
		return wireAsset{}, &TransportError{Err: err}
	}
	data, readErr := io.ReadAll(resp.Body)
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		return wireAsset{}, &TransportError{Err: err}
	}
	var w wireAsset
	if err := decodeOrRaise(resp, data, &w, http.StatusOK, http.StatusCreated, http.StatusAccepted); err != nil {
		return wireAsset{}, err
	}
	return w, nil
}

// Delete removes the committed asset.
func (a *Asset) Delete(ctx context.Context) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	id := a.ID()
	if id == "" {
		return errors.New("cannot delete an uncommitted asset")
	}
	if err := a.t.jsonCall(ctx, http.MethodDelete, "/assets/"+url.PathEscape(id), nil, nil, nil, http.StatusNoContent); err != nil {
		return err
	}
	a.mu.Lock()
	a.id = ""
	a.mu.Unlock()
	return nil
}

// Reference commits the asset and returns its core/ASSET reference, the
// object a workflow input carries. It holds the asset's operation slot
// throughout, so a concurrent Delete cannot slip between the commit and the
// reference it returns.
func (a *Asset) Reference(ctx context.Context) (map[string]any, error) {
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	id, err := a.commit(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info := map[string]any{"id": id, "file_path": a.filePath}
	if a.hash != "" {
		info["hash"] = a.hash
	}
	return map[string]any{"__type": "core/ASSET", "info": info}, nil
}

// DownloadURL is where the asset's bytes can be fetched, with its expiry
// when the server issued a signed URL.
type DownloadURL struct {
	URL       string
	ExpiresAt *time.Time
}

// DownloadURL commits the asset and resolves a URL for its bytes.
func (a *Asset) DownloadURL(ctx context.Context) (DownloadURL, error) {
	id, err := a.Commit(ctx)
	if err != nil {
		return DownloadURL{}, err
	}
	return a.t.contentURL(ctx, id)
}

func (t *transport) headAssetByHash(ctx context.Context, digest string) (bool, error) {
	resp, data, err := t.do(ctx, http.MethodHead, "/assets/by-hash/"+url.PathEscape(digest), nil, nil)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, errorFromResponse(resp.StatusCode, data, resp.Header.Get)
}

// contentURL resolves an asset's content URL without following redirects:
// Comfy Cloud answers with a redirect to a signed URL, a self-hosted proxy
// serves the bytes itself. The redirect target is never contacted, so the
// bearer token never reaches it.
func (t *transport) contentURL(ctx context.Context, id string) (DownloadURL, error) {
	p := "/assets/" + url.PathEscape(id) + "/content"
	resp, err := t.stream(ctx, t.noFollow, http.MethodGet, p, nil, t.idle,
		http.StatusOK, http.StatusPartialContent, http.StatusFound, http.StatusMovedPermanently,
		http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect)
	if err != nil {
		return DownloadURL{}, err
	}
	loc := resp.Header.Get("Location")
	redirect := resp.StatusCode >= 300 && resp.StatusCode < 400
	if err := resp.Body.Close(); err != nil {
		return DownloadURL{}, &TransportError{Err: err}
	}
	if redirect && loc != "" {
		d := DownloadURL{URL: loc}
		if exp, ok := parseExpiry(loc); ok {
			d.ExpiresAt = &exp
		}
		return d, nil
	}
	return DownloadURL{URL: t.url(p)}, nil
}

func hashBytes(data []byte) string {
	sum := blake3.Sum256(data)
	return "blake3:" + hex.EncodeToString(sum[:])
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	h := blake3.New(32, nil)
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	return "blake3:" + hex.EncodeToString(h.Sum(nil)), nil
}

// newUUID returns a random version 4 UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
