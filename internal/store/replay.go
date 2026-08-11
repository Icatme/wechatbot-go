package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Icatme/wechatbot-go/internal/persist"
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
	write  func(string, any) error
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
		write:  persist.WriteJSONAtomic,
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
	pruneReplay(parsed.SeenAt, s.now(), s.ttl)
	s.seenAt = parsed.SeenAt
	return nil
}

// Seen reports whether key was committed within the replay window.
func (s *ReplayStore) Seen(key string) bool {
	return s.SeenAny(key)
}

// SeenAny reports whether any key was committed within the replay window.
func (s *ReplayStore) SeenAny(keys ...string) bool {
	cutoff := s.now().Add(-s.ttl).UnixMilli()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range keys {
		if key == "" {
			continue
		}
		if seenAt, ok := s.seenAt[key]; ok && seenAt >= cutoff {
			return true
		}
	}
	return false
}

// Commit records key as successfully handled and persists it.
func (s *ReplayStore) Commit(key string) error {
	return s.CommitAll(key)
}

// CommitAll records keys with one atomic persistence operation.
func (s *ReplayStore) CommitAll(keys ...string) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneReplay(s.seenAt)
	pruneReplay(next, now, s.ttl)
	hasKey := false
	for _, key := range keys {
		if key == "" {
			continue
		}
		next[key] = now.UnixMilli()
		hasKey = true
	}
	if !hasKey {
		return nil
	}
	if err := s.write(s.path, replayData{SeenAt: next}); err != nil {
		return fmt.Errorf("write replay state: %w", err)
	}
	s.seenAt = next
	return nil
}

func cloneReplay(source map[string]int64) map[string]int64 {
	clone := make(map[string]int64, len(source))
	for key, seenAt := range source {
		clone[key] = seenAt
	}
	return clone
}

func pruneReplay(seenAt map[string]int64, now time.Time, ttl time.Duration) {
	cutoff := now.Add(-ttl).UnixMilli()
	for key, timestamp := range seenAt {
		if timestamp < cutoff {
			delete(seenAt, key)
		}
	}
}
