package proxy

import (
	"fmt"
	"sync"
	"time"
)

// AssetStore is the asset index plus its content-hash lookup. A self-hosted
// ComfyUI is single-user, so there is one namespace, but the v2 shape holds:
// UUID-identified records over blake3-addressed bytes.
type AssetStore struct {
	mu       sync.RWMutex
	byID     map[string]*AssetRecord
	idByHash map[string]string
	persist  Persist
}

// NewAssetStore returns an empty store that writes through to persist when
// it is non-nil.
func NewAssetStore(persist Persist) *AssetStore {
	return &AssetStore{byID: map[string]*AssetRecord{}, idByHash: map[string]string{}, persist: persist}
}

func (s *AssetStore) load() error {
	recs, idx, err := s.persist.LoadAssets()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range recs {
		rec := recs[i]
		s.byID[rec.ID] = &rec
	}
	s.idByHash = idx
	return nil
}

// Get returns a copy of the record, so callers cannot race on it.
func (s *AssetStore) Get(id string) (AssetRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[id]
	if !ok {
		return AssetRecord{}, false
	}
	return *rec, true
}

// GetByHash returns the record that owns hash's dedup slot.
func (s *AssetStore) GetByHash(hash string) (AssetRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.idByHash[hash]
	if !ok {
		return AssetRecord{}, false
	}
	rec, ok := s.byID[id]
	if !ok {
		return AssetRecord{}, false
	}
	return *rec, true
}

// Add stores a new record. The first record with a given hash owns its dedup
// slot, so a later identical upload resolves to the same record.
func (s *AssetStore) Add(rec AssetRecord) (AssetRecord, error) {
	if rec.ID == "" {
		id, err := newUUID()
		if err != nil {
			return AssetRecord{}, err
		}
		rec.ID = id
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = iso(now())
	}
	if rec.Tags == nil {
		rec.Tags = []string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		if err := s.persist.UpsertAsset(rec); err != nil {
			return AssetRecord{}, fmt.Errorf("persist asset: %w", err)
		}
	}
	s.byID[rec.ID] = &rec
	if rec.Hash != "" {
		if _, taken := s.idByHash[rec.Hash]; !taken {
			s.idByHash[rec.Hash] = rec.ID
		}
	}
	return rec, nil
}

// Delete removes a record. It reports whether the record existed.
func (s *AssetStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return false, nil
	}
	if s.persist != nil {
		if err := s.persist.DeleteAsset(id); err != nil {
			return false, fmt.Errorf("persist asset delete: %w", err)
		}
	}
	s.removeLocked(rec)
	return true, nil
}

func (s *AssetStore) removeLocked(rec *AssetRecord) {
	delete(s.byID, rec.ID)
	if rec.Hash != "" && s.idByHash[rec.Hash] == rec.ID {
		delete(s.idByHash, rec.Hash)
	}
}

// DeleteExpired removes every record whose expires_at is at or before cutoff
// and returns their ids. A record with an unparseable expires_at is kept.
func (s *AssetStore) DeleteExpired(cutoff time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted []string
	for id, rec := range s.byID {
		if rec.ExpiresAt == nil {
			continue
		}
		expires, err := parseISO(*rec.ExpiresAt)
		if err != nil || expires.After(cutoff) {
			continue
		}
		if s.persist != nil {
			if err := s.persist.DeleteAsset(id); err != nil {
				return deleted, fmt.Errorf("persist expired asset delete: %w", err)
			}
		}
		s.removeLocked(rec)
		deleted = append(deleted, id)
	}
	return deleted, nil
}
