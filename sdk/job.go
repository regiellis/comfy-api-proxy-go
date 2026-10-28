package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Poll schedule for Wait: start small, grow, hold at the cap.
const (
	pollStart  = 500 * time.Millisecond
	pollFactor = 1.5
	pollCap    = 5 * time.Second
)

// Job is a submitted job. Its fields are the snapshot from the last call that
// returned it; Refresh, Wait, and Cancel update them. Safe for concurrent use.
type Job struct {
	t  *transport
	mu sync.RWMutex
	w  wireJob
}

func (j *Job) snapshot() wireJob {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.w
}

func (j *Job) set(w wireJob) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.w = w
}

// ID is the job id.
func (j *Job) ID() string { return j.snapshot().ID }

// Status is the last known status.
func (j *Job) Status() string { return j.snapshot().Status }

// Failure is the server's account of a failed job, nil otherwise. The
// returned value is a copy.
func (j *Job) Failure() *JobError {
	if e := j.snapshot().Error; e != nil {
		c := *e
		return &c
	}
	return nil
}

// Progress is a copy of the last progress snapshot, nil when the server
// sent none.
func (j *Job) Progress() *JobProgress {
	if p := j.snapshot().Progress; p != nil {
		c := *p
		return &c
	}
	return nil
}

// QueuePosition is the job's place in the queue while queued.
func (j *Job) QueuePosition() *int {
	if q := j.snapshot().QueuePosition; q != nil {
		c := *q
		return &c
	}
	return nil
}

// CreatedAt is when the job was created.
func (j *Job) CreatedAt() time.Time { return j.snapshot().CreatedAt }

// Metadata is the job's labels; empty when it has none, or when the server
// sends metadata that is neither a string map nor a JSON-encoded string map.
func (j *Job) Metadata() map[string]string { return labels(j.snapshot().Metadata) }

// Outputs are the job's outputs so far.
func (j *Job) Outputs() []*Output {
	w := j.snapshot()
	out := make([]*Output, len(w.Outputs))
	for i, o := range w.Outputs {
		out[i] = &Output{t: j.t, w: o}
	}
	return out
}

// GetOutputs are the outputs one node produced.
func (j *Job) GetOutputs(nodeID string) []*Output {
	var out []*Output
	for _, o := range j.Outputs() {
		if o.NodeID() == nodeID {
			out = append(out, o)
		}
	}
	return out
}

// String reports id and status.
func (j *Job) String() string {
	w := j.snapshot()
	return fmt.Sprintf("Job(id=%q, status=%q)", w.ID, w.Status)
}

// pathOr is a link the server handed back, else the id-based path.
func pathOr(link, id, suffix string) string {
	if link != "" {
		return link
	}
	return "/jobs/" + url.PathEscape(id) + suffix
}

// Refresh re-reads the job. GET /jobs/{id} is authoritative.
func (j *Job) Refresh(ctx context.Context) error {
	w := j.snapshot()
	var next wireJob
	if err := j.t.jsonCall(ctx, http.MethodGet, pathOr(w.URLs.Self, w.ID, ""), nil, nil, &next, http.StatusOK); err != nil {
		return err
	}
	j.set(next)
	return nil
}

// Wait polls until the job is terminal, on a schedule that starts at half a
// second and settles at five. Bound it with the context.
func (j *Job) Wait(ctx context.Context) error {
	delay := pollStart
	for {
		if err := j.Refresh(ctx); err != nil {
			return err
		}
		if IsTerminal(j.Status()) {
			return nil
		}
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("job %s not terminal (status %s): %w", j.ID(), j.Status(), err)
		}
		delay = min(time.Duration(float64(delay)*pollFactor), pollCap)
	}
}

// Result waits, then reports a job that did not succeed as *JobFailedError.
func (j *Job) Result(ctx context.Context) error {
	if err := j.Wait(ctx); err != nil {
		return err
	}
	w := j.snapshot()
	if w.Status != StatusSucceeded {
		return &JobFailedError{JobID: w.ID, Status: w.Status, Err: w.Error}
	}
	return nil
}

// Cancel asks the server to cancel. A running job reads canceling until the
// interrupt lands; Wait for the terminal state.
func (j *Job) Cancel(ctx context.Context) error {
	w := j.snapshot()
	var next wireJob
	if err := j.t.jsonCall(ctx, http.MethodPost, pathOr(w.URLs.Cancel, w.ID, "/cancel"), nil, nil, &next, http.StatusOK); err != nil {
		return err
	}
	j.set(next)
	return nil
}

// JobWorkflow is the graph a job ran with. Format is "api" for an executed
// graph or "save" for the original authoring workflow.
type JobWorkflow struct {
	Graph  map[string]any
	Format string
}

// Workflow fetches the graph the job ran with.
func (j *Job) Workflow(ctx context.Context) (JobWorkflow, error) {
	var w wireJobWorkflow
	if err := j.t.jsonCall(ctx, http.MethodGet, "/jobs/"+url.PathEscape(j.ID())+"/workflow", nil, nil, &w, http.StatusOK); err != nil {
		return JobWorkflow{}, err
	}
	return JobWorkflow{Graph: w.Workflow, Format: w.Format}, nil
}

// errStreamUnsupported marks a server without the events endpoint (501).
var errStreamUnsupported = errors.New("event stream not supported")
