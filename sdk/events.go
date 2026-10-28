package sdk

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"
)

// Event is one item from a job's live stream: *Progress, *Preview,
// *OutputReady, *StatusChange, or *Log.
type Event interface{ isEvent() }

// Progress is a full progress snapshot, so a dropped one costs nothing.
type Progress struct {
	Value       float64
	Message     *string
	NodesDone   *int
	NodesTotal  *int
	CurrentNode *string
	Step        *int
	Steps       *int
}

// Preview is an in-progress image.
type Preview struct {
	NodeID      string
	ContentType string
	Data        []byte
}

// OutputReady carries an output the moment it is committed.
type OutputReady struct{ Output *Output }

// StatusChange is a status transition; the stream ends after a terminal one.
type StatusChange struct {
	Status        string
	QueuePosition *int
}

// Log is a server log line.
type Log struct{ Level, Message string }

func (*Progress) isEvent()     {}
func (*Preview) isEvent()      {}
func (*OutputReady) isEvent()  {}
func (*StatusChange) isEvent() {}
func (*Log) isEvent()          {}

// reconnectPause separates a dropped stream from its reconnect.
const reconnectPause = 100 * time.Millisecond

// rawEvent is one dispatched SSE frame.
type rawEvent struct {
	event string
	data  map[string]any
}

// sseDecoder turns lines into events: event/data fields, comment lines
// ignored, a blank line dispatching. Data that is not a JSON object is
// wrapped, so a consumer always sees an object.
type sseDecoder struct {
	event string
	data  []string
	seen  bool
}

func (d *sseDecoder) push(line string) (rawEvent, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return d.dispatch()
	}
	if strings.HasPrefix(line, ":") {
		return rawEvent{}, false
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		d.event, d.seen = value, true
	case "data":
		d.data = append(d.data, value)
	}
	return rawEvent{}, false
}

func (d *sseDecoder) dispatch() (rawEvent, bool) {
	if len(d.data) == 0 && !d.seen {
		return rawEvent{}, false
	}
	raw := strings.Join(d.data, "\n")
	name := d.event
	if name == "" {
		name = "message"
	}
	d.event, d.data, d.seen = "", nil, false
	if raw == "" {
		return rawEvent{}, false
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return rawEvent{name, map[string]any{"raw": raw}}, true
	}
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{"value": v}
	}
	return rawEvent{name, m}, true
}

func optString(m map[string]any, k string) *string {
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}

func optInt(m map[string]any, k string) *int {
	if f, ok := m[k].(float64); ok {
		n := int(f)
		return &n
	}
	return nil
}

func (j *Job) eventFrom(raw rawEvent) (Event, error) {
	d := raw.data
	switch raw.event {
	case "progress":
		value, _ := d["value"].(float64)
		return &Progress{Value: value, Message: optString(d, "message"), NodesDone: optInt(d, "nodes_done"),
			NodesTotal: optInt(d, "nodes_total"), CurrentNode: optString(d, "current_node"),
			Step: optInt(d, "step"), Steps: optInt(d, "steps")}, nil
	case "preview":
		p := &Preview{ContentType: "application/octet-stream"}
		if s, ok := d["node_id"].(string); ok {
			p.NodeID = s
		}
		if s, ok := d["content_type"].(string); ok {
			p.ContentType = s
		}
		if s, ok := d["data_base64"].(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				p.Data = b
			}
		}
		return p, nil
	case "status":
		s := &StatusChange{QueuePosition: optInt(d, "queue_position")}
		if v, ok := d["status"].(string); ok {
			s.Status = v
		}
		return s, nil
	case "log":
		l := &Log{Level: "info"}
		if v, ok := d["level"].(string); ok {
			l.Level = v
		}
		if v, ok := d["message"].(string); ok {
			l.Message = v
		}
		return l, nil
	case "output":
		// A malformed output is an error, as the Python SDK's model
		// validation raises, rather than an output the caller never sees.
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, invalidResponse("output event could not be re-encoded: " + err.Error())
		}
		var w wireOutput
		if err := json.Unmarshal(raw, &w); err != nil || w.ID == "" {
			return nil, invalidResponse("output event is not a valid output")
		}
		return &OutputReady{Output: &Output{t: j.t, w: w}}, nil
	}
	return nil, nil
}

// Events streams the job's live events until a terminal status. The stream
// is best effort and poll-backed: a dropped connection reconnects, a stream
// that ends without a terminal status falls back to GET /jobs/{id}, and a
// server without the endpoint (501) ends the iteration quietly. The last
// event is always a terminal *StatusChange unless an error ends it first.
func (j *Job) Events(ctx context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		w := j.snapshot()
		path := pathOr(w.URLs.Events, w.ID, "/events")
		for {
			terminal, stop, err := j.streamOnce(ctx, path, yield)
			switch {
			case stop:
				return
			case errors.Is(err, errStreamUnsupported):
				return
			case err != nil && !isTransport(err):
				yield(nil, err)
				return
			case terminal:
				return
			}
			if err := j.Refresh(ctx); err != nil {
				yield(nil, err)
				return
			}
			if s := j.Status(); IsTerminal(s) {
				yield(&StatusChange{Status: s}, nil)
				return
			}
			if err := sleep(ctx, reconnectPause); err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

func isTransport(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

// streamOnce reads one connection. It reports whether a terminal status was
// delivered, whether the consumer stopped, and what ended the connection.
func (j *Job) streamOnce(ctx context.Context, path string, yield func(Event, error) bool) (bool, bool, error) {
	resp, err := j.t.stream(ctx, j.t.http, http.MethodGet, path, http.Header{"Accept": {"text/event-stream"}}, sseIdleTimeout, http.StatusOK)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && api.HTTPStatus == http.StatusNotImplemented {
			return false, false, errStreamUnsupported
		}
		return false, false, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			return // the stream is finished either way
		}
	}()
	var dec sseDecoder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20) // previews are whole images
	sc.Split(scanSSELines)
	for sc.Scan() {
		raw, ok := dec.push(sc.Text())
		if !ok {
			continue
		}
		ev, err := j.eventFrom(raw)
		if err != nil {
			return false, false, err
		}
		if ev == nil {
			continue
		}
		if !yield(ev, nil) {
			return false, true, nil
		}
		if s, ok := ev.(*StatusChange); ok && IsTerminal(s.Status) {
			return true, false, nil
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		return false, false, &TransportError{Err: err}
	}
	return false, false, nil
}

// scanSSELines splits on LF, CRLF, or a lone CR, the three line endings the
// SSE format allows. bufio.ScanLines knows only the first two.
func scanSSELines(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		switch c {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil // wait: the CR may be half of a CRLF
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
