package proxy

import (
	"container/list"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// jobStore holds the job records this proxy minted. With a Persist attached,
// only the newest maxJobScan reload at startup; older ones are read through
// to the store on demand, since ComfyUI's history ring outlives that window.
type jobStore struct {
	mu      sync.RWMutex
	byID    map[string]*JobRecord
	persist Persist
}

func newJobStore(persist Persist) *jobStore {
	return &jobStore{byID: map[string]*JobRecord{}, persist: persist}
}

func (s *jobStore) load(limit int) error {
	recs, err := s.persist.LoadJobs(limit)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range recs {
		rec := recs[i]
		rec.Workflow = nil
		s.byID[rec.ID] = &rec
	}
	return nil
}

func (s *jobStore) remember(rec JobRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		if err := s.persist.UpsertJob(rec); err != nil {
			return fmt.Errorf("persist job: %w", err)
		}
	}
	s.byID[rec.ID] = &rec
	return nil
}

// meta returns the record from memory, then from the store. ok is false when
// neither knows the job.
func (s *jobStore) meta(id string) (JobRecord, bool, error) {
	s.mu.RLock()
	rec, ok := s.byID[id]
	s.mu.RUnlock()
	if ok {
		return *rec, true, nil
	}
	if s.persist == nil {
		return JobRecord{}, false, nil
	}
	stored, err := s.persist.GetJob(id)
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("read job %s: %w", id, err)
	}
	if stored == nil {
		return JobRecord{}, false, nil
	}
	return *stored, true, nil
}

// workflow returns the resolved graph a job ran with. A record reloaded at
// startup carries no graph, so its absence in memory falls through to the
// store's dedicated column read.
func (s *jobStore) workflow(id string) (json.RawMessage, error) {
	s.mu.RLock()
	rec, ok := s.byID[id]
	s.mu.RUnlock()
	if ok && rec.Workflow != nil {
		return rec.Workflow, nil
	}
	if s.persist == nil {
		return nil, nil
	}
	wf, err := s.persist.GetJobWorkflow(id)
	if err != nil {
		return nil, fmt.Errorf("read job %s workflow: %w", id, err)
	}
	return wf, nil
}

// newestFirst lists in-memory job ids by created_at, newest first.
func (s *jobStore) newestFirst() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return s.byID[ids[i]].CreatedAt.After(s.byID[ids[j]].CreatedAt)
	})
	return ids
}

// idempotency tracks single-use Idempotency-Key claims in a bounded FIFO, so
// unique keys cannot grow memory without limit.
type idempotency struct {
	mu      sync.Mutex
	order   *list.List
	index   map[string]*list.Element
	persist Persist
}

func newIdempotency(persist Persist) *idempotency {
	return &idempotency{order: list.New(), index: map[string]*list.Element{}, persist: persist}
}

func (s *idempotency) load() error {
	keys, err := s.persist.LoadIdempotencyKeys()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if _, ok := s.index[k]; !ok {
			s.index[k] = s.order.PushBack(k)
		}
	}
	return nil
}

// claim takes key and reports false when it was already taken, here or by a
// previous process. The check and the claim happen under one lock, so two
// concurrent submits with the same key cannot both win.
func (s *idempotency) claim(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[key]; ok {
		return false, nil
	}
	if s.persist != nil {
		fresh, err := s.persist.ClaimIdempotency(key, now())
		if err != nil {
			return false, fmt.Errorf("persist idempotency claim: %w", err)
		}
		if !fresh {
			s.index[key] = s.order.PushBack(key)
			return false, nil
		}
	}
	s.index[key] = s.order.PushBack(key)
	if s.order.Len() > maxIdempotencyKeys {
		oldest := s.order.Front()
		s.order.Remove(oldest)
		oldestKey, ok := oldest.Value.(string)
		if !ok {
			return true, nil
		}
		delete(s.index, oldestKey)
		if s.persist != nil {
			if err := s.persist.ReleaseIdempotency(oldestKey); err != nil {
				return true, fmt.Errorf("persist idempotency evict: %w", err)
			}
			if err := s.persist.TrimIdempotency(maxIdempotencyKeys); err != nil {
				return true, fmt.Errorf("persist idempotency trim: %w", err)
			}
		}
	}
	return true, nil
}

// release frees key after a submit that definitely created no job.
func (s *idempotency) release(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.index[key]; ok {
		s.order.Remove(el)
		delete(s.index, key)
	}
	if s.persist != nil {
		if err := s.persist.ReleaseIdempotency(key); err != nil {
			return fmt.Errorf("persist idempotency release: %w", err)
		}
	}
	return nil
}
