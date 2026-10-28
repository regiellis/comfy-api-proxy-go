package sdk

import (
	"encoding/json"
	"time"
)

// Wire shapes from the Comfy API v2 contract (spec/openapi.yaml in the
// Python SDK). Optional fields are pointers so absent and zero stay distinct.

// Job statuses. Canceling is not terminal: cancellation lands at the next
// node boundary and can take seconds.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusCanceling = "canceling"
	StatusCanceled  = "canceled"
	StatusFailed    = "failed"
	StatusExpired   = "expired"
)

// IsTerminal reports whether a job in status will not change again.
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusCanceled, StatusFailed, StatusExpired:
		return true
	}
	return false
}

type jobURLs struct {
	Self   string `json:"self"`
	Events string `json:"events"`
	Cancel string `json:"cancel"`
}

// JobProgress is the latest progress snapshot on a job.
type JobProgress struct {
	Value            float64 `json:"value"`
	NodesDone        int     `json:"nodes_done"`
	NodesTotal       int     `json:"nodes_total"`
	CurrentNode      *string `json:"current_node"`
	CurrentNodeClass *string `json:"current_node_class"`
	Step             *int    `json:"step"`
	Steps            *int    `json:"steps"`
	Message          *string `json:"message"`
}

// JobError is the server's account of why a job failed.
type JobError struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	NodeID    *string `json:"node_id"`
	ClassType *string `json:"class_type"`
	Traceback *string `json:"traceback"`
}

type wireOutput struct {
	NodeID       string    `json:"node_id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	ContentType  string    `json:"content_type"`
	SizeBytes    int64     `json:"size_bytes"`
	ID           string    `json:"id"`
	Hash         *string   `json:"hash"`
	URL          string    `json:"url"`
	URLExpiresAt time.Time `json:"url_expires_at"`
	JobID        *string   `json:"job_id"`
}

type wireJob struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     *time.Time      `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	QueuePosition *int            `json:"queue_position"`
	Progress      *JobProgress    `json:"progress"`
	Outputs       []wireOutput    `json:"outputs"`
	Error         *JobError       `json:"error"`
	Metrics       map[string]*int `json:"metrics"`
	URLs          jobURLs         `json:"urls"`
	Metadata      json.RawMessage `json:"metadata"`
}

type wireAsset struct {
	ID           string     `json:"id"`
	Hash         *string    `json:"hash"`
	SizeBytes    int64      `json:"size_bytes"`
	ContentType  string     `json:"content_type"`
	FilePath     *string    `json:"file_path"`
	CreatedNew   *bool      `json:"created_new"`
	CreatedAt    time.Time  `json:"created_at"`
	URL          string     `json:"url"`
	URLExpiresAt time.Time  `json:"url_expires_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	JobID        *string    `json:"job_id"`
}

type wireJobWorkflow struct {
	Workflow map[string]any `json:"workflow"`
	Format   string         `json:"format"`
}

// labels accepts metadata objects and JSON-encoded objects stored by proxies.
// Opaque strings and non-string label values are ignored.
func labels(raw json.RawMessage) map[string]string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return map[string]string{}
	}
	return metadataLabels(value)
}

func metadataLabels(value any) map[string]string {
	out := map[string]string{}
	if encoded, ok := value.(string); ok {
		if json.Unmarshal([]byte(encoded), &value) != nil {
			return out
		}
	}
	m, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
