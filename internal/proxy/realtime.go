package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
)

// ComfyUI's /ws is its only live signal. The bridge connects with the job's
// client_id (ComfyUI addresses prompt events at the submitting client and
// never broadcasts them), translates frames into v2 SSE events, and ends at
// the terminal transition. GET /jobs/{id} stays authoritative: every
// progress event is a full snapshot, so a dropped frame costs nothing, and an
// unreachable WebSocket falls back to polling so the stream still resolves.

const (
	throttle        = 500 * time.Millisecond // about 2 progress or preview events a second
	pollDeadline    = 10 * time.Minute
	wsReadLimit     = 64 << 20 // previews are whole encoded images
	binPreview      = 1
	binPreviewMeta  = 4
	retryAfterSecs  = "5"
	previewJPEG     = 1
	previewPNG      = 2
	defaultPreviewT = "image/jpeg"
)

// Stream pacing, variables only so tests can run it fast.
var (
	heartbeat    = 15 * time.Second
	pollInterval = time.Second
)

var terminalStatuses = map[string]bool{"succeeded": true, "failed": true, "canceled": true, "expired": true}

// snapshot is the slice of a job the stream reports.
type snapshot struct {
	Status        string
	QueuePosition *int
	Progress      *progress
	Outputs       []output
}

type statusEvent struct {
	Status        string `json:"status"`
	QueuePosition *int   `json:"queue_position,omitempty"`
}

type liveProgress struct {
	Value       float64 `json:"value"`
	NodesDone   int     `json:"nodes_done"`
	NodesTotal  int     `json:"nodes_total"`
	CurrentNode *string `json:"current_node"`
	Step        *int64  `json:"step"`
	Steps       *int64  `json:"steps"`
}

type previewEvent struct {
	NodeID      string `json:"node_id"`
	ContentType string `json:"content_type"`
	DataBase64  string `json:"data_base64"`
}

// sseWriter writes frames and flushes each one. No id: lines, since the v2
// stream is live push with no Last-Event-ID replay.
type sseWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (s *sseWriter) event(name string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (p *Proxy) acquireStream() bool {
	p.streamsMu.Lock()
	defer p.streamsMu.Unlock()
	if p.streams >= maxStreams {
		return false
	}
	p.streams++
	return true
}

func (p *Proxy) releaseStream() {
	p.streamsMu.Lock()
	defer p.streamsMu.Unlock()
	p.streams--
}

func (p *Proxy) jobEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		notFoundJob(w, id)
		return
	}
	base := p.base(r)
	st, err := p.statusOf(r.Context(), id, base)
	if err != nil {
		p.stateError(w, err)
		return
	}
	rec, known, err := p.jobs.meta(id)
	if err != nil {
		p.internalError(w, err)
		return
	}
	if st.Status == "unknown" && !known {
		notFoundJob(w, id)
		return
	}
	if !p.acquireStream() {
		w.Header().Set("Retry-After", retryAfterSecs)
		httpx.WriteError(w, http.StatusTooManyRequests, "too_many_streams",
			"Maximum concurrent event streams reached; poll GET /jobs/{id} instead.", nil)
		return
	}
	defer p.releaseStream()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	clientID := id
	if known && rec.ClientID != "" {
		clientID = rec.ClientID
	}
	b := &bridge{
		p:        p,
		jobID:    id,
		clientID: clientID,
		base:     base,
		out:      &sseWriter{w: w, rc: http.NewResponseController(w)},
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(p.streamsCtx, cancel)
	defer stop()
	// Headers are sent, so a mid-stream failure cannot change the status.
	// End the stream and let the client fall back to polling, which reports
	// upstream trouble with a proper envelope.
	if err := b.run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, comfy.ErrUnreachable) {
		p.logf("events %s: %v", id, err)
	}
}

type bridge struct {
	p        *Proxy
	jobID    string
	clientID string
	base     string
	out      *sseWriter

	lastProgress, lastPreview, lastOutput time.Time
	seen                                  map[string]bool
}

func (b *bridge) snapshot(ctx context.Context) (snapshot, error) {
	st, err := b.p.statusOf(ctx, b.jobID, b.base)
	if err != nil {
		return snapshot{}, err
	}
	rec, known, err := b.p.jobs.meta(b.jobID)
	if err != nil {
		return snapshot{}, err
	}
	job := jobJSON(b.jobID, st, rec, known, b.base)
	return snapshot{Status: job.Status, QueuePosition: job.QueuePosition, Progress: job.Progress, Outputs: job.Outputs}, nil
}

func (b *bridge) status(s snapshot) error {
	return b.out.event("status", statusEvent{Status: s.Status, QueuePosition: s.QueuePosition})
}

// outputs emits each output not yet sent on this stream.
func (b *bridge) outputs(s snapshot) error {
	for _, o := range s.Outputs {
		if b.seen[o.ID] {
			continue
		}
		b.seen[o.ID] = true
		if err := b.out.event("output", o); err != nil {
			return err
		}
	}
	return nil
}

// finish sends any outputs not yet delivered, then the terminal status, so
// the status is always the last event. Clients (both SDKs) stop reading at a
// terminal status, so an output sent after it would never be seen; the
// reference's own tests expect outputs ahead of the terminal status.
func (b *bridge) finish(s snapshot) error {
	if err := b.outputs(s); err != nil {
		return err
	}
	return b.status(s)
}

func (b *bridge) run(ctx context.Context) error {
	b.seen = map[string]bool{}
	snap, err := b.snapshot(ctx)
	if err != nil {
		return err
	}
	if terminalStatuses[snap.Status] {
		// A late client still gets the outputs, ahead of the status.
		return b.finish(snap)
	}
	if err := b.status(snap); err != nil {
		return err
	}
	if snap.Progress != nil {
		if err := b.out.event("progress", snap.Progress); err != nil {
			return err
		}
	}

	done, err := b.pumpWS(ctx)
	if done || (err != nil && !isWSFailure(err)) {
		return err
	}
	return b.pollUntilTerminal(ctx)
}

// wsFailure marks a broken or unreachable ComfyUI WebSocket, which switches
// the stream to polling instead of ending it.
type wsFailure struct{ err error }

func (e *wsFailure) Error() string { return "comfyui websocket: " + e.err.Error() }
func (e *wsFailure) Unwrap() error { return e.err }

func isWSFailure(err error) bool {
	var f *wsFailure
	return errors.As(err, &f)
}

type wsMessage struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

func (b *bridge) wsURL() string {
	base := b.p.comfy.Base
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/ws?clientId=" + b.clientID
}

// pumpWS forwards WebSocket frames until the job ends (done=true) or the
// socket closes or fails (done=false), after which the caller polls.
func (b *bridge) pumpWS(ctx context.Context) (bool, error) {
	wsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(wsCtx, 10*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, b.wsURL(), &websocket.DialOptions{HTTPClient: b.p.comfy.HTTP})
	dialCancel()
	if resp != nil && resp.Body != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			b.p.logf("events %s: close dial response: %v", b.jobID, cerr)
		}
	}
	if err != nil {
		return false, &wsFailure{err}
	}
	defer func() {
		if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
			b.p.logf("events %s: close websocket: %v", b.jobID, err)
		}
	}()
	conn.SetReadLimit(wsReadLimit)

	// Client speaks first in ComfyUI's feature_flags handshake. Declaring
	// preview metadata makes previews arrive tagged with their node id.
	hello := []byte(`{"type":"feature_flags","data":{"supports_preview_metadata":true}}`)
	if err := conn.Write(wsCtx, websocket.MessageText, hello); err != nil {
		return false, &wsFailure{err}
	}

	// Reads run in their own goroutine: cancelling a Read context closes the
	// connection in this library, so the heartbeat timer cannot be a read
	// deadline.
	msgs := make(chan wsMessage)
	go func() {
		defer close(msgs)
		for {
			typ, data, err := conn.Read(wsCtx)
			select {
			case msgs <- wsMessage{typ, data, err}:
			case <-wsCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// A job that ended between the first snapshot and the connect sent its
	// terminal message to nobody, so look again now that frames are flowing.
	snap, err := b.snapshot(ctx)
	if err != nil {
		return true, err
	}
	if terminalStatuses[snap.Status] {
		return true, b.finish(snap)
	}

	// The heartbeat runs on a fixed schedule that frames never reset.
	// ComfyUI broadcasts to every socket (status, and custom nodes such as
	// crystools.monitor twice a second), so a timer reset per frame would
	// never fire, and a missed terminal message would hang the stream.
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-ticker.C:
			if err := b.out.comment("keepalive"); err != nil {
				return true, err
			}
			snap, err := b.snapshot(ctx)
			if err != nil {
				return true, err
			}
			if terminalStatuses[snap.Status] {
				return true, b.finish(snap)
			}
		case m, ok := <-msgs:
			if !ok || m.err != nil {
				// Closed or broken socket: reconcile by polling.
				return false, nil
			}
			done, err := b.onFrame(ctx, m)
			if done || err != nil {
				return true, err
			}
		}
	}
}

// wsText is a ComfyUI text frame: {"type": ..., "data": {...}}.
type wsText struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// forThisJob decodes a text frame and reports whether it concerns this job.
// Frames scoped to another prompt on the same ComfyUI are ignored; frames
// without a prompt_id apply to everyone.
func (b *bridge) forThisJob(raw []byte) (wsText, map[string]json.RawMessage, bool) {
	var msg wsText
	if json.Unmarshal(raw, &msg) != nil {
		return msg, nil, false
	}
	data := map[string]json.RawMessage{}
	if len(msg.Data) > 0 && string(msg.Data) != "null" {
		if json.Unmarshal(msg.Data, &data) != nil {
			data = map[string]json.RawMessage{}
		}
	}
	if pidRaw, ok := data["prompt_id"]; ok && string(pidRaw) != "null" {
		var pid string
		if json.Unmarshal(pidRaw, &pid) != nil || pid != b.jobID {
			return msg, data, false
		}
	}
	return msg, data, true
}

func (b *bridge) onFrame(ctx context.Context, m wsMessage) (bool, error) {
	if m.typ == websocket.MessageBinary {
		if ev, ok := b.preview(m.data); ok {
			return false, b.out.event("preview", ev)
		}
		return false, nil
	}
	msg, data, mine := b.forThisJob(m.data)
	if !mine {
		return false, nil
	}
	switch msg.Type {
	case "progress", "progress_state":
		if time.Since(b.lastProgress) >= throttle {
			b.lastProgress = time.Now()
			if err := b.out.event("progress", translateProgress(msg.Type, data)); err != nil {
				return false, err
			}
		}
	case "executed":
		// A node's outputs were just committed: surface them now rather than
		// only at the end, coalesced so many output nodes cannot fan out into
		// one /history fetch each. A skipped window loses nothing.
		if time.Since(b.lastOutput) >= throttle {
			b.lastOutput = time.Now()
			snap, err := b.snapshot(ctx)
			if err != nil {
				return false, err
			}
			if err := b.outputs(snap); err != nil {
				return false, err
			}
		}
	case "execution_success", "execution_error", "execution_interrupted":
		// execution_error surfaces only through the final status. The v2
		// contract reserves the log event, so none is emitted, matching Cloud.
		snap, err := b.snapshot(ctx)
		if err != nil {
			return true, err
		}
		return true, b.finish(snap)
	}
	return false, nil
}

func jsonFloat(raw json.RawMessage, def float64) float64 {
	var f float64
	if raw == nil || json.Unmarshal(raw, &f) != nil {
		return def
	}
	return f
}

func clamp01(f float64) float64 { return max(0, min(1, f)) }

func translateProgress(typ string, data map[string]json.RawMessage) liveProgress {
	if typ == "progress" {
		value := jsonFloat(data["value"], 0)
		maxv := jsonFloat(data["max"], 1)
		if maxv == 0 {
			maxv = 1
		}
		step, steps := int64(value), int64(maxv)
		lp := liveProgress{Value: clamp01(value / maxv), Step: &step, Steps: &steps}
		if raw, ok := data["node"]; ok && string(raw) != "null" {
			node := rawString(raw)
			lp.CurrentNode = &node
		}
		return lp
	}

	// progress_state: {nodes: {id: {value, max, state}}}, in document order.
	type nodeState struct {
		Value *float64 `json:"value"`
		Max   *float64 `json:"max"`
		State string   `json:"state"`
	}
	nodes, err := objectEntries(data["nodes"])
	if err != nil {
		nodes = nil
	}
	total, done := len(nodes), 0
	var running *string
	var cur *nodeState
	for _, n := range nodes {
		var ns nodeState
		if json.Unmarshal(n.value, &ns) != nil {
			continue
		}
		if ns.State == "finished" {
			done++
		}
		if ns.State == "running" && running == nil {
			key := n.key
			running = &key
			cur = &ns
		}
	}
	frac := 0.0
	if total > 0 {
		frac = float64(done) / float64(total)
		if cur != nil && cur.Max != nil && *cur.Max != 0 && cur.Value != nil {
			frac = (float64(done) + *cur.Value / *cur.Max) / float64(total)
		}
	}
	lp := liveProgress{Value: clamp01(frac), NodesDone: done, NodesTotal: total, CurrentNode: running}
	if cur != nil && cur.Value != nil {
		v := int64(*cur.Value)
		lp.Step = &v
	}
	if cur != nil && cur.Max != nil {
		m := int64(*cur.Max)
		lp.Steps = &m
	}
	return lp
}

// rawString renders a JSON scalar the way Python's str() would for the
// values ComfyUI sends as node ids.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// preview decodes ComfyUI's binary framing: a 4-byte big-endian event type,
// then either a 4-byte image format and the image (type 1), or a 4-byte
// metadata length, JSON metadata, and the image (type 4).
func (b *bridge) preview(data []byte) (previewEvent, bool) {
	if len(data) < 8 || time.Since(b.lastPreview) < throttle {
		return previewEvent{}, false
	}
	ev := previewEvent{ContentType: defaultPreviewT}
	var img []byte
	switch binary.BigEndian.Uint32(data[:4]) {
	case binPreview:
		if binary.BigEndian.Uint32(data[4:8]) == previewPNG {
			ev.ContentType = "image/png"
		}
		img = data[8:]
	case binPreviewMeta:
		metaLen := int(binary.BigEndian.Uint32(data[4:8]))
		if metaLen < 0 || 8+metaLen > len(data) {
			metaLen = len(data) - 8
		}
		var meta struct {
			ImageType *string         `json:"image_type"`
			NodeID    json.RawMessage `json:"node_id"`
		}
		if json.Unmarshal(data[8:8+metaLen], &meta) == nil {
			if meta.ImageType != nil {
				ev.ContentType = *meta.ImageType
			}
			if meta.NodeID != nil {
				ev.NodeID = rawString(meta.NodeID)
			}
		}
		img = data[8+metaLen:]
	default:
		return previewEvent{}, false
	}
	b.lastPreview = time.Now()
	ev.DataBase64 = base64.StdEncoding.EncodeToString(img)
	return ev, true
}

func (b *bridge) pollUntilTerminal(ctx context.Context) error {
	deadline := time.Now().Add(pollDeadline)
	for time.Now().Before(deadline) {
		snap, err := b.snapshot(ctx)
		if err != nil {
			return err
		}
		if terminalStatuses[snap.Status] {
			return b.finish(snap)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		if err := b.out.comment("keepalive"); err != nil {
			return err
		}
	}
	return nil
}
