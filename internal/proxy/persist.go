package proxy

import (
	"encoding/json"
	"time"
)

// Persist is the --state-dir write-through store. The in-memory indexes stay
// the source of truth while the process runs; Persist makes them survive a
// restart. Implemented by internal/state over SQLite.
type Persist interface {
	GetMeta(key string) (string, bool, error)
	SetMeta(key, value string) error

	UpsertJob(rec JobRecord) error
	// LoadJobs returns the newest limit records, oldest first, without their
	// workflow graphs.
	LoadJobs(limit int) ([]JobRecord, error)
	GetJob(id string) (*JobRecord, error)
	GetJobWorkflow(id string) (json.RawMessage, error)

	// ClaimIdempotency reports false when the key is already claimed.
	ClaimIdempotency(key string, claimedAt time.Time) (bool, error)
	ReleaseIdempotency(key string) error
	// LoadIdempotencyKeys returns claimed keys oldest first.
	LoadIdempotencyKeys() ([]string, error)
	TrimIdempotency(max int) error

	UpsertAsset(rec AssetRecord) error
	// LoadAssets returns every asset and the hash index (hash to asset id).
	LoadAssets() ([]AssetRecord, map[string]string, error)
	DeleteAsset(id string) error
}

// JobRecord is what the proxy remembers about a job it submitted.
type JobRecord struct {
	ID        string
	CreatedAt time.Time
	ClientID  string
	Metadata  *string
	Priority  *int64
	// Workflow is the resolved graph that ran. Nil on records reloaded from
	// the store, which skip the column to keep startup cheap.
	Workflow json.RawMessage
}

// ComfyRef locates a file through ComfyUI's /view.
type ComfyRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// AssetRecord is one asset: its v2 fields plus how to fetch the bytes.
// Exactly one of ComfyRef and DiskPath is set.
type AssetRecord struct {
	ID          string
	Hash        string // "blake3:<hex>", empty for unhashed
	SizeBytes   int64
	ContentType string
	FilePath    *string
	CreatedAt   string
	ExpiresAt   *string
	ComfyRef    *ComfyRef
	DiskPath    *string
	Tags        []string
}
