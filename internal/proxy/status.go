package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"strings"
)

// outputKind maps ComfyUI's per-node output keys to v2 output types.
var outputKind = map[string]string{
	"images":  "image",
	"gifs":    "video",
	"audio":   "audio",
	"text":    "text",
	"files":   "file",
	"latents": "latent",
}

type progress struct {
	Value      float64 `json:"value"`
	NodesDone  int     `json:"nodes_done"`
	NodesTotal int     `json:"nodes_total"`
}

type output struct {
	NodeID       string  `json:"node_id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	ContentType  string  `json:"content_type"`
	SizeBytes    int64   `json:"size_bytes"`
	ID           string  `json:"id"`
	Hash         *string `json:"hash"`
	URL          string  `json:"url"`
	URLExpiresAt string  `json:"url_expires_at"`
	JobID        string  `json:"job_id"`
	// ref is where ComfyUI keeps the file, for sizing it (fillSizes).
	ref ComfyRef
}

type jobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	NodeID    any    `json:"node_id"`
	ClassType any    `json:"class_type"`
	Traceback any    `json:"traceback"`
}

// jobState is the upstream view of one job, before proxy fields are added.
type jobState struct {
	Status        string
	Outputs       []output
	QueuePosition *int
	Progress      *progress
	Error         *jobError
	OutputsReused bool
}

type historyEntry struct {
	Outputs json.RawMessage `json:"outputs"`
	Status  *struct {
		StatusStr *string           `json:"status_str"`
		Messages  []json.RawMessage `json:"messages"`
	} `json:"status"`
}

// message is one [event, data] pair from a history entry. ok is false for
// anything not shaped that way.
func message(raw json.RawMessage) (string, json.RawMessage, bool) {
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 {
		return "", nil, false
	}
	var ev string
	if json.Unmarshal(pair[0], &ev) != nil {
		return "", nil, false
	}
	return ev, pair[1], true
}

func (e *historyEntry) messages() []json.RawMessage {
	if e.Status == nil {
		return nil
	}
	return e.Status.Messages
}

// statusOf resolves a job against ComfyUI: history first (terminal), then the
// queue (running or pending), else unknown.
//
// A job that finishes between the two reads is in neither: gone from the
// queue, but not in the history read taken a moment earlier. Reported as
// unknown it would surface as expired, a terminal state, and a fast job
// would read as lost. ComfyUI writes the history entry and clears the
// running slot under one lock, so after a queue miss the history entry is
// already there, and one more history read settles it.
func (p *Proxy) statusOf(ctx context.Context, jobID, base string) (jobState, error) {
	st, found, err := p.historyState(ctx, jobID, base)
	if err != nil || found {
		return st, err
	}
	st, err = p.queueState(ctx, jobID)
	if err != nil || st.Status != "unknown" {
		return st, err
	}
	if st, found, err := p.historyState(ctx, jobID, base); err != nil || found {
		return st, err
	}
	return jobState{Status: "unknown", Outputs: []output{}}, nil
}

// historyState reads /history/{id}; found is false when the job is not there.
func (p *Proxy) historyState(ctx context.Context, jobID, base string) (jobState, bool, error) {
	raw, ok, err := p.comfy.GetJSON(ctx, "/history/"+jobID)
	if err != nil || !ok {
		return jobState{}, false, err
	}
	var hist map[string]json.RawMessage
	if json.Unmarshal(raw, &hist) != nil {
		return jobState{}, false, nil
	}
	entryRaw, found := hist[jobID]
	if !found {
		return jobState{}, false, nil
	}
	var entry historyEntry
	if json.Unmarshal(entryRaw, &entry) != nil {
		return jobState{}, false, nil
	}
	st := p.terminalState(jobID, &entry, base)
	p.fillSizes(ctx, st.Outputs)
	return st, true, nil
}

// queueState reads /queue for a running or pending job, else unknown.
func (p *Proxy) queueState(ctx context.Context, jobID string) (jobState, error) {
	raw, ok, err := p.comfy.GetJSON(ctx, "/queue")
	if err != nil {
		return jobState{}, err
	}
	if !ok {
		return jobState{Status: "unknown", Outputs: []output{}}, nil
	}

	var q struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if json.Unmarshal(raw, &q) == nil {
		for _, item := range q.Running {
			if id, ok := queueItemID(item); ok && id == jobID {
				return jobState{Status: "running", Outputs: []output{}, Progress: &progress{}}, nil
			}
		}
		pos := 0
		for _, item := range q.Pending {
			id, ok := queueItemID(item)
			if !ok {
				continue
			}
			pos++
			if id == jobID {
				return jobState{Status: "queued", Outputs: []output{}, QueuePosition: &pos}, nil
			}
		}
	}
	return jobState{Status: "unknown", Outputs: []output{}}, nil
}

// queueItemID is item[1] of a [number, prompt_id, ...] queue entry. ok is
// false for an entry too short to carry an id; a non-string id comes back
// empty but still counts toward queue positions, as in the reference.
func queueItemID(raw json.RawMessage) (string, bool) {
	var item []json.RawMessage
	if json.Unmarshal(raw, &item) != nil || len(item) < 2 {
		return "", false
	}
	var id string
	if json.Unmarshal(item[1], &id) != nil {
		return "", true
	}
	return id, true
}

func (p *Proxy) terminalState(jobID string, entry *historyEntry, base string) jobState {
	statusStr := "success"
	if entry.Status != nil && entry.Status.StatusStr != nil {
		statusStr = *entry.Status.StatusStr
	}
	interrupted := false
	for _, m := range entry.messages() {
		if ev, _, ok := message(m); ok && ev == "execution_interrupted" {
			interrupted = true
		}
	}
	st := jobState{Outputs: p.outputs(jobID, entry.Outputs, base), OutputsReused: outputsReused(entry)}
	switch {
	case interrupted:
		st.Status = "canceled"
	case statusStr == "success":
		st.Status = "succeeded"
	default:
		st.Status = "failed"
		st.Error = errorFrom(entry)
	}
	return st
}

// outputsReused is true only when execution_cached names at least one node.
// ComfyUI emits the message on every run, with an empty list when it cached
// nothing, so its presence alone means nothing.
func outputsReused(entry *historyEntry) bool {
	for _, m := range entry.messages() {
		ev, data, ok := message(m)
		if !ok || ev != "execution_cached" {
			continue
		}
		var d struct {
			Nodes []json.RawMessage `json:"nodes"`
		}
		if json.Unmarshal(data, &d) == nil && len(d.Nodes) > 0 {
			return true
		}
	}
	return false
}

func errorFrom(entry *historyEntry) *jobError {
	for _, m := range entry.messages() {
		ev, data, ok := message(m)
		if !ok || ev != "execution_error" {
			continue
		}
		var d map[string]any
		if json.Unmarshal(data, &d) != nil || d == nil {
			continue
		}
		msg, _ := d["exception_message"].(string)
		return &jobError{Code: "node_execution_error", Message: msg, NodeID: d["node_id"], ClassType: d["node_type"]}
	}
	return &jobError{Code: "node_execution_error", Message: "execution failed"}
}

// outputs flattens a history entry's outputs in the order ComfyUI listed
// them, which is the order a client picking outputs[0] expects.
func (p *Proxy) outputs(jobID string, raw json.RawMessage, base string) []output {
	out := []output{}
	nodes, err := objectEntries(raw)
	if err != nil {
		return out
	}
	expires := iso(now().Add(retention))
	for _, node := range nodes {
		keys, err := objectEntries(node.value)
		if err != nil {
			continue
		}
		for _, k := range keys {
			kind, known := outputKind[k.key]
			if !known {
				continue
			}
			var items []json.RawMessage
			if json.Unmarshal(k.value, &items) != nil {
				continue
			}
			for _, itemRaw := range items {
				var it struct {
					Filename  *string `json:"filename"`
					Subfolder *string `json:"subfolder"`
					Type      *string `json:"type"`
				}
				if json.Unmarshal(itemRaw, &it) != nil || it.Filename == nil {
					continue
				}
				sub, typ := "", "output"
				if it.Subfolder != nil {
					sub = *it.Subfolder
				}
				if it.Type != nil {
					typ = *it.Type
				}
				id := p.outputAssetID(jobID, *it.Filename, sub, typ)
				out = append(out, output{
					NodeID:       node.key,
					Name:         *it.Filename,
					Type:         kind,
					ContentType:  guessContentType(*it.Filename),
					ID:           id,
					URL:          base + "/api/v2/assets/" + id + "/content",
					URLExpiresAt: expires,
					JobID:        jobID,
					ref:          ComfyRef{Filename: *it.Filename, Subfolder: sub, Type: typ},
				})
			}
		}
	}
	return out
}

// guessContentType maps a filename extension to a bare media type.
func guessContentType(name string) string {
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if ct == "" {
		return "application/octet-stream"
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	return ct
}

type entry struct {
	key   string
	value json.RawMessage
}

// objectEntries returns a JSON object's members in document order, which a
// Go map would lose.
func objectEntries(raw json.RawMessage) ([]entry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var out []entry
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, entry{key, v})
	}
	return out, nil
}

type jobURLs struct {
	Self   string `json:"self"`
	Events string `json:"events"`
	Cancel string `json:"cancel"`
}

type jobBody struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"`
	CreatedAt     string    `json:"created_at"`
	StartedAt     *string   `json:"started_at"`
	CompletedAt   *string   `json:"completed_at"`
	ExpiresAt     string    `json:"expires_at"`
	QueuePosition *int      `json:"queue_position"`
	Progress      *progress `json:"progress"`
	Outputs       []output  `json:"outputs"`
	Error         *jobError `json:"error"`
	OutputsReused bool      `json:"outputs_reused"`
	URLs          jobURLs   `json:"urls"`
	Metadata      *string   `json:"metadata,omitempty"`
	Priority      *int64    `json:"priority,omitempty"`
}

// jobJSON merges the upstream state with the proxy's record. A job the proxy
// knows but ComfyUI no longer does reads as expired.
func jobJSON(jobID string, st jobState, rec JobRecord, known bool, base string) jobBody {
	created := now()
	if known && !rec.CreatedAt.IsZero() {
		created = rec.CreatedAt
	}
	status := st.Status
	if status == "unknown" {
		status = "expired"
	}
	outs := st.Outputs
	if outs == nil {
		outs = []output{}
	}
	return jobBody{
		ID:            jobID,
		Status:        status,
		CreatedAt:     iso(created),
		ExpiresAt:     iso(created.Add(retention)),
		QueuePosition: st.QueuePosition,
		Progress:      st.Progress,
		Outputs:       outs,
		Error:         st.Error,
		OutputsReused: st.OutputsReused,
		URLs: jobURLs{
			Self:   base + "/api/v2/jobs/" + jobID,
			Events: base + "/api/v2/jobs/" + jobID + "/events",
			Cancel: base + "/api/v2/jobs/" + jobID + "/cancel",
		},
		Metadata: rec.Metadata,
		Priority: rec.Priority,
	}
}
