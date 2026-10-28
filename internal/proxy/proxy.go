// Package proxy implements the Comfy API v2 surface over one ComfyUI.
//
// GET /api/v2/jobs/{id} is authoritative and reads ComfyUI's /history and
// /queue on every call. The proxy itself only keeps what ComfyUI cannot tell
// it: the job records it minted (created_at, metadata, priority, the resolved
// workflow), Idempotency-Key claims, the asset index, and the secret that
// signs output asset ids. With --state-dir those write through to SQLite.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
	"github.com/regiellis/comfy-api-proxy-go/internal/config"
	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
)

const (
	retention = 24 * time.Hour

	maxIdempotencyKeyLen = 255
	maxIdempotencyKeys   = 10_000
	maxMetadataBytes     = 1024
	minPriority          = -1_000_000
	maxPriority          = 1_000_000
	maxListJobs          = 100
	defaultListJobs      = 50
	// maxJobScan caps how many records GET /jobs resolves upstream (two calls
	// each) and how many job records reload from --state-dir at startup.
	maxJobScan = 500
	// maxStreams bounds concurrent SSE streams, each of which holds a ComfyUI
	// WebSocket open.
	maxStreams = 8
)

// Proxy holds the v2 handlers and the proxy-layer state.
type Proxy struct {
	comfy     *comfy.Client
	baseDir   string // resolved --comfyui-base-dir, empty when unset
	maxUpload int64
	publicURL string // public_url, empty when links follow the request
	secret    []byte
	store     Persist // nil without --state-dir
	log       io.Writer

	jobs   *jobStore
	idem   *idempotency
	assets *AssetStore

	sizes sizeCache

	streamsMu sync.Mutex
	streams   int
	// streamsCtx ends every open event stream when the server shuts down.
	streamsCtx  context.Context
	stopStreams context.CancelFunc
}

// New builds a Proxy. store is nil for in-memory mode. log receives
// operational lines such as expired-asset cleanup counts.
func New(cfg *config.Config, store Persist, log io.Writer) (*Proxy, error) {
	p := &Proxy{
		comfy:     comfy.New(cfg.ComfyUI, cfg.ComfyUIToken),
		maxUpload: int64(cfg.MaxUploadMB) * 1024 * 1024,
		publicURL: cfg.PublicURL,
		store:     store,
		log:       log,
		jobs:      newJobStore(store),
		idem:      newIdempotency(store),
		assets:    NewAssetStore(store),
	}
	p.streamsCtx, p.stopStreams = context.WithCancel(context.Background())
	if cfg.ComfyUIBaseDir != "" {
		abs, err := filepath.Abs(cfg.ComfyUIBaseDir)
		if err != nil {
			return nil, fmt.Errorf("resolve comfyui_base_dir: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		p.baseDir = abs
	}
	if err := p.loadSecret(); err != nil {
		return nil, err
	}
	if store != nil {
		if err := p.jobs.load(maxJobScan); err != nil {
			return nil, fmt.Errorf("load job records: %w", err)
		}
		if err := p.idem.load(); err != nil {
			return nil, fmt.Errorf("load idempotency keys: %w", err)
		}
		if err := p.assets.load(); err != nil {
			return nil, fmt.Errorf("load asset index: %w", err)
		}
	}
	return p, nil
}

// loadSecret reuses the persisted output-id signing secret, so ids minted by
// a previous process still verify, or mints and stores a new one.
func (p *Proxy) loadSecret() error {
	if p.store != nil {
		stored, ok, err := p.store.GetMeta("asset_secret")
		if err != nil {
			return fmt.Errorf("read asset secret: %w", err)
		}
		if ok {
			secret, err := hex.DecodeString(stored)
			if err != nil || len(secret) == 0 {
				return fmt.Errorf("stored asset secret is not valid hex")
			}
			p.secret = secret
			return nil
		}
	}
	p.secret = make([]byte, 32)
	if _, err := rand.Read(p.secret); err != nil {
		return fmt.Errorf("generate asset secret: %w", err)
	}
	if p.store != nil {
		if err := p.store.SetMeta("asset_secret", hex.EncodeToString(p.secret)); err != nil {
			return fmt.Errorf("store asset secret: %w", err)
		}
	}
	return nil
}

// StopStreams ends every open SSE stream. Register it as a server shutdown
// hook so a graceful stop does not wait on streams that never go idle.
func (p *Proxy) StopStreams() { p.stopStreams() }

// Register adds the v2 routes to mux.
func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/jobs", p.submit)
	mux.HandleFunc("GET /api/v2/jobs", p.listJobs)
	mux.HandleFunc("GET /api/v2/jobs/{id}", p.getJob)
	mux.HandleFunc("GET /api/v2/jobs/{id}/workflow", p.getJobWorkflow)
	mux.HandleFunc("POST /api/v2/jobs/{id}/cancel", p.cancelJob)
	mux.HandleFunc("GET /api/v2/jobs/{id}/events", p.jobEvents)

	mux.HandleFunc("POST /api/v2/assets", p.uploadAsset)
	mux.HandleFunc("POST /api/v2/assets/from-hash", p.assetFromHash)
	mux.HandleFunc("POST /api/v2/assets/from-path", p.assetFromPath)
	mux.HandleFunc("GET /api/v2/assets/{id}", p.getAsset)
	mux.HandleFunc("DELETE /api/v2/assets/{id}", p.deleteAsset)
	// by-hash/{hash} and {id}/content overlap on "by-hash/content", which
	// ServeMux refuses as ambiguous, so one handler dispatches both with
	// by-hash taking precedence, as in the reference's route order.
	mux.HandleFunc("/api/v2/assets/{first}/{second}", p.assetSubresource)
}

func (p *Proxy) assetSubresource(w http.ResponseWriter, r *http.Request) {
	first, second := r.PathValue("first"), r.PathValue("second")
	switch {
	case first == "by-hash" && r.Method == http.MethodHead:
		r.SetPathValue("hash", second)
		p.headAssetByHash(w, r)
	case second == "content" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		r.SetPathValue("id", first)
		p.getAssetContent(w, r)
	case first == "by-hash" || second == "content":
		w.Header().Set("Allow", allowFor(first, second))
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.", nil)
	default:
		http.NotFound(w, r)
	}
}

func allowFor(first, second string) string {
	switch {
	case first == "by-hash" && second == "content":
		return "GET, HEAD"
	case first == "by-hash":
		return "HEAD"
	}
	return "GET, HEAD"
}

func now() time.Time { return time.Now().UTC() }

// isoLayout always prints microseconds, which every Python fromisoformat
// accepts, where Python's own isoformat drops a zero fraction.
const isoLayout = "2006-01-02T15:04:05.000000-07:00"

func iso(t time.Time) string { return t.UTC().Format(isoLayout) }

func parseISO(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// base is the origin for URLs in a response: public_url when it is set,
// else the scheme and Host the request arrived with. The operator states
// the public origin rather than the proxy trusting X-Forwarded-* headers.
func (p *Proxy) base(r *http.Request) string {
	if p.publicURL != "" {
		return p.publicURL
	}
	return externalBase(r)
}

// externalBase is the absolute base URL the client reached us on, from the
// connection and the Host header. X-Forwarded-* is ignored on purpose, so a
// client cannot point emitted links at another origin.
func externalBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// validJobID mirrors Python's uuid.UUID parsing, which the reference uses as
// its gate: an optional "urn:uuid:" prefix and braces, hyphens anywhere, then
// exactly 32 hex digits. Anything else is rejected before it can be spliced
// into an upstream path, where a dot-segment or "?" would retarget the call.
func validJobID(id string) bool {
	s := strings.ReplaceAll(id, "urn:", "")
	s = strings.ReplaceAll(s, "uuid:", "")
	s = strings.Trim(s, "{}")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// newUUID returns a random version 4 UUID string.
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

func (p *Proxy) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	if _, err := fmt.Fprintf(p.log, format+"\n", args...); err != nil {
		return // log sink gone; serving continues
	}
}
