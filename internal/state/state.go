// Package state is the --state-dir SQLite store behind the proxy's in-memory
// indexes: job records, Idempotency-Key claims, the asset index, and the
// output-id signing secret. One store belongs to one proxy and one ComfyUI.
//
// The schema matches the reference, so a state directory written by the
// Python proxy opens here, plus a nullable assets.expires_at column that the
// reference never persisted.
package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps the binary CGO-free

	"github.com/regiellis/comfy-api-proxy-go/internal/proxy"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    client_id  TEXT NOT NULL,
    metadata   TEXT,
    priority   INTEGER,
    workflow   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_created_at ON jobs(created_at DESC);
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key        TEXT PRIMARY KEY,
    claimed_at TEXT NOT NULL,
    job_id     TEXT
);
CREATE TABLE IF NOT EXISTS assets (
    id           TEXT PRIMARY KEY,
    hash         TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    file_path    TEXT,
    created_at   TEXT NOT NULL,
    comfy_ref    TEXT,
    disk_path    TEXT,
    tags         TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS asset_hash_index (
    hash     TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL
);
`

// isoLayout matches internal/proxy, so timestamps sort as text.
const isoLayout = "2006-01-02T15:04:05.000000-07:00"

// Store implements proxy.Persist.
type Store struct {
	db *sql.DB
	// Warnings lists permission changes that could not be made. They are not
	// fatal, as in the reference: a filesystem without POSIX modes (FAT, some
	// network mounts) still works, it just cannot be locked down.
	Warnings []string
}

var _ proxy.Persist = (*Store)(nil)

// Open creates or opens dir/state.sqlite3. The directory is 0700 and the
// database 0600: it holds the HMAC secret that signs output asset ids, and
// any local user who could read it could mint valid ids. The directory mode
// also covers the WAL and SHM files SQLite creates beside it.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	var warnings []string
	if err := os.Chmod(dir, 0o700); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not restrict state dir to 0700: %v", err))
	}
	path := filepath.Join(dir, "state.sqlite3")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create state db: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create state db: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not restrict state db to 0600: %v", err))
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// One connection serializes writes the way the reference's lock does.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Warnings: warnings}
	if err := s.migrate(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	rows, err := s.db.Query("PRAGMA table_info(assets)")
	if err != nil {
		return fmt.Errorf("inspect assets table: %w", err)
	}
	hasExpires := false
	for rows.Next() {
		var (
			cid          int
			name, typ    string
			notNull, pk  int
			defaultValue sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return errors.Join(fmt.Errorf("inspect assets table: %w", err), rows.Close())
		}
		if name == "expires_at" {
			hasExpires = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("inspect assets table: %w", err)
	}
	if !hasExpires {
		if _, err := s.db.Exec("ALTER TABLE assets ADD COLUMN expires_at TEXT"); err != nil {
			return fmt.Errorf("add assets.expires_at: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read meta %s: %w", key, err)
	}
	return v, true, nil
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", key, err)
	}
	return nil
}

func (s *Store) UpsertJob(rec proxy.JobRecord) error {
	workflow := string(rec.Workflow)
	if workflow == "" {
		workflow = "{}"
	}
	_, err := s.db.Exec(`INSERT INTO jobs(id, created_at, client_id, metadata, priority, workflow)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET created_at=excluded.created_at, client_id=excluded.client_id,
		metadata=excluded.metadata, priority=excluded.priority, workflow=excluded.workflow`,
		rec.ID, rec.CreatedAt.UTC().Format(isoLayout), rec.ClientID, nullString(rec.Metadata), nullInt(rec.Priority), workflow)
	if err != nil {
		return fmt.Errorf("upsert job %s: %w", rec.ID, err)
	}
	return nil
}

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func scanJob(id, created, clientID string, metadata sql.NullString, priority sql.NullInt64) (proxy.JobRecord, error) {
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return proxy.JobRecord{}, fmt.Errorf("job %s: bad created_at %q: %w", id, created, err)
	}
	rec := proxy.JobRecord{ID: id, CreatedAt: t.UTC(), ClientID: clientID}
	if metadata.Valid {
		m := metadata.String
		rec.Metadata = &m
	}
	if priority.Valid {
		p := priority.Int64
		rec.Priority = &p
	}
	return rec, nil
}

func (s *Store) LoadJobs(limit int) ([]proxy.JobRecord, error) {
	rows, err := s.db.Query("SELECT id, created_at, client_id, metadata, priority FROM jobs ORDER BY created_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("load jobs: %w", err)
	}
	var recs []proxy.JobRecord
	for rows.Next() {
		var (
			id, created, clientID string
			metadata              sql.NullString
			priority              sql.NullInt64
		)
		if err := rows.Scan(&id, &created, &clientID, &metadata, &priority); err != nil {
			return nil, errors.Join(fmt.Errorf("load jobs: %w", err), rows.Close())
		}
		rec, err := scanJob(id, created, clientID, metadata, priority)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		recs = append(recs, rec)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("load jobs: %w", err)
	}
	// Oldest first, as the interface promises.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs, nil
}

func (s *Store) GetJob(id string) (*proxy.JobRecord, error) {
	var (
		created, clientID string
		metadata          sql.NullString
		priority          sql.NullInt64
	)
	err := s.db.QueryRow("SELECT created_at, client_id, metadata, priority FROM jobs WHERE id = ?", id).
		Scan(&created, &clientID, &metadata, &priority)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read job %s: %w", id, err)
	}
	rec, err := scanJob(id, created, clientID, metadata, priority)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Store) GetJobWorkflow(id string) (json.RawMessage, error) {
	var wf string
	err := s.db.QueryRow("SELECT workflow FROM jobs WHERE id = ?", id).Scan(&wf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read job %s workflow: %w", id, err)
	}
	if !json.Valid([]byte(wf)) {
		return nil, nil
	}
	return json.RawMessage(wf), nil
}

func (s *Store) ClaimIdempotency(key string, claimedAt time.Time) (bool, error) {
	res, err := s.db.Exec("INSERT INTO idempotency_keys(key, claimed_at, job_id) VALUES (?, ?, NULL) ON CONFLICT(key) DO NOTHING",
		key, claimedAt.UTC().Format(isoLayout))
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return n == 1, nil
}

func (s *Store) ReleaseIdempotency(key string) error {
	if _, err := s.db.Exec("DELETE FROM idempotency_keys WHERE key = ?", key); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

func (s *Store) LoadIdempotencyKeys() ([]string, error) {
	rows, err := s.db.Query("SELECT key FROM idempotency_keys ORDER BY claimed_at ASC")
	if err != nil {
		return nil, fmt.Errorf("load idempotency keys: %w", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, errors.Join(fmt.Errorf("load idempotency keys: %w", err), rows.Close())
		}
		keys = append(keys, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("load idempotency keys: %w", err)
	}
	return keys, nil
}

func (s *Store) TrimIdempotency(max int) error {
	_, err := s.db.Exec(`DELETE FROM idempotency_keys WHERE key IN (
		SELECT key FROM idempotency_keys ORDER BY claimed_at ASC
		LIMIT max(0, (SELECT COUNT(*) FROM idempotency_keys) - ?))`, max)
	if err != nil {
		return fmt.Errorf("trim idempotency keys: %w", err)
	}
	return nil
}

func (s *Store) UpsertAsset(rec proxy.AssetRecord) error {
	var comfyRef any
	if rec.ComfyRef != nil {
		raw, err := json.Marshal(rec.ComfyRef)
		if err != nil {
			return fmt.Errorf("encode comfy_ref: %w", err)
		}
		comfyRef = string(raw)
	}
	tags := rec.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsRaw, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("encode tags: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("upsert asset %s: %w", rec.ID, err)
	}
	if _, err := tx.Exec(`INSERT INTO assets(id, hash, size_bytes, content_type, file_path, created_at, comfy_ref, disk_path, tags, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET hash=excluded.hash, size_bytes=excluded.size_bytes,
		content_type=excluded.content_type, file_path=excluded.file_path, created_at=excluded.created_at,
		comfy_ref=excluded.comfy_ref, disk_path=excluded.disk_path, tags=excluded.tags, expires_at=excluded.expires_at`,
		rec.ID, rec.Hash, rec.SizeBytes, rec.ContentType, nullString(rec.FilePath), rec.CreatedAt,
		comfyRef, nullString(rec.DiskPath), string(tagsRaw), nullString(rec.ExpiresAt)); err != nil {
		return errors.Join(fmt.Errorf("upsert asset %s: %w", rec.ID, err), tx.Rollback())
	}
	if rec.Hash != "" {
		if _, err := tx.Exec("INSERT INTO asset_hash_index(hash, asset_id) VALUES (?, ?) ON CONFLICT(hash) DO NOTHING", rec.Hash, rec.ID); err != nil {
			return errors.Join(fmt.Errorf("index asset %s: %w", rec.ID, err), tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert asset %s: %w", rec.ID, err)
	}
	return nil
}

func (s *Store) LoadAssets() ([]proxy.AssetRecord, map[string]string, error) {
	rows, err := s.db.Query("SELECT id, hash, size_bytes, content_type, file_path, created_at, comfy_ref, disk_path, tags, expires_at FROM assets")
	if err != nil {
		return nil, nil, fmt.Errorf("load assets: %w", err)
	}
	var recs []proxy.AssetRecord
	for rows.Next() {
		var (
			rec                              proxy.AssetRecord
			hash                             sql.NullString
			filePath, comfyRef, disk, expire sql.NullString
			tags                             sql.NullString
		)
		if err := rows.Scan(&rec.ID, &hash, &rec.SizeBytes, &rec.ContentType, &filePath, &rec.CreatedAt, &comfyRef, &disk, &tags, &expire); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("load assets: %w", err), rows.Close())
		}
		rec.Hash = hash.String
		rec.FilePath = fromNull(filePath)
		rec.DiskPath = fromNull(disk)
		rec.ExpiresAt = fromNull(expire)
		if comfyRef.Valid && comfyRef.String != "" {
			var ref proxy.ComfyRef
			if err := json.Unmarshal([]byte(comfyRef.String), &ref); err != nil {
				return nil, nil, errors.Join(fmt.Errorf("asset %s: bad comfy_ref: %w", rec.ID, err), rows.Close())
			}
			rec.ComfyRef = &ref
		}
		rec.Tags = []string{}
		if tags.Valid && tags.String != "" {
			if err := json.Unmarshal([]byte(tags.String), &rec.Tags); err != nil {
				return nil, nil, errors.Join(fmt.Errorf("asset %s: bad tags: %w", rec.ID, err), rows.Close())
			}
		}
		recs = append(recs, rec)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, fmt.Errorf("load assets: %w", err)
	}

	hrows, err := s.db.Query("SELECT hash, asset_id FROM asset_hash_index")
	if err != nil {
		return nil, nil, fmt.Errorf("load hash index: %w", err)
	}
	idx := map[string]string{}
	for hrows.Next() {
		var h, id string
		if err := hrows.Scan(&h, &id); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("load hash index: %w", err), hrows.Close())
		}
		idx[h] = id
	}
	if err := errors.Join(hrows.Err(), hrows.Close()); err != nil {
		return nil, nil, fmt.Errorf("load hash index: %w", err)
	}
	return recs, idx, nil
}

func fromNull(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func (s *Store) DeleteAsset(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete asset %s: %w", id, err)
	}
	if _, err := tx.Exec("DELETE FROM assets WHERE id = ?", id); err != nil {
		return errors.Join(fmt.Errorf("delete asset %s: %w", id, err), tx.Rollback())
	}
	if _, err := tx.Exec("DELETE FROM asset_hash_index WHERE asset_id = ?", id); err != nil {
		return errors.Join(fmt.Errorf("delete asset %s index: %w", id, err), tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete asset %s: %w", id, err)
	}
	return nil
}
