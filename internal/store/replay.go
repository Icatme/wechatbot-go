package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultReplayTTL is the window in which a handled message identity is ignored.
const DefaultReplayTTL = 24 * time.Hour

type replayData struct {
	SeenAt map[string]int64 `json:"seen_at"`
}

// ReplayStore persists handled message identities so repeated getUpdates
// deliveries do not invoke application handlers more than once.
type ReplayStore struct {
	path   string
	ttl    time.Duration
	now    func() time.Time
	mu     sync.RWMutex
	seenAt map[string]int64
}

// NewReplayStore creates a replay store backed by path.
func NewReplayStore(accountID, path string, ttl time.Duration) *ReplayStore {
	if path == "" {
		path = filepath.Join(AccountStateDir(accountID), "replay_dedupe.json")
	}
	if ttl <= 0 {
		ttl = DefaultReplayTTL
	}
	return &ReplayStore{
		path:   path,
		ttl:    ttl,
		now:    time.Now,
		seenAt: make(map[string]int64),
	}
}

// Path returns the backing file path.
func (s *ReplayStore) Path() string {
	return s.path
}

// Load reads handled message identities from disk and drops expired entries.
func (s *ReplayStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.seenAt = make(map[string]int64)
			return nil
		}
		return fmt.Errorf("read replay state: %w", err)
	}

	var parsed replayData
	if len(data) > 0 {
		if err := json.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("decode replay state: %w", err)
		}
	}
	if parsed.SeenAt == nil {
		parsed.SeenAt = make(map[string]int64)
	}
	s.seenAt = parsed.SeenAt
	s.pruneLocked(s.now())
	return nil
}

// Seen reports whether key was committed within the replay window.
func (s *ReplayStore) Seen(key string) bool {
	if key == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	_, ok := s.seenAt[key]
	return ok
}

// Commit records key as successfully handled and persists it.
func (s *ReplayStore) Commit(key string) error {
	if key == "" {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.seenAt[key] = now.UnixMilli()
	return s.saveLocked()
}

func (s *ReplayStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.ttl).UnixMilli()
	for key, seenAt := range s.seenAt {
		if seenAt < cutoff {
			delete(s.seenAt, key)
		}
	}
}

func (s *ReplayStore) saveLocked() error {
	if err := ensureDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("ensure replay state dir: %w", err)
	}
	out, err := json.MarshalIndent(replayData{SeenAt: s.seenAt}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode replay state: %w", err)
	}
	if err := os.WriteFile(s.path, append(out, '\n'), 0600); err != nil {
		return fmt.Errorf("write replay state: %w", err)
	}
	return nil
}
