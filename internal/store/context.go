package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Icatme/wechatbot-go/internal/persist"
)

// ContextStore persists context tokens per user.
// Tokens are needed to reply in the correct conversation and must survive restarts.
type ContextStore struct {
	path  string
	write func(string, any) error
	mu    sync.RWMutex
	data  map[string]string
}

// NewContextStore creates a store backed by the given file path.
// If path is empty, it defaults to ~/.wechatbot/accounts/{accountID}/context_tokens.json
// when accountID is non-empty, otherwise ~/.wechatbot/context_tokens.json.
func NewContextStore(accountID, path string) *ContextStore {
	if path == "" {
		path = filepath.Join(AccountStateDir(accountID), "context_tokens.json")
	}
	return &ContextStore{
		path:  path,
		write: persist.WriteJSONAtomic,
		data:  make(map[string]string),
	}
}

// Path returns the backing file path.
func (s *ContextStore) Path() string {
	return s.path
}

// Load reads tokens from disk into memory.
// Missing files are treated as empty; malformed files return an error.
func (s *ContextStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.data = make(map[string]string)
			return nil
		}
		return fmt.Errorf("read context tokens: %w", err)
	}

	parsed := make(map[string]string)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("decode context tokens: %w", err)
		}
	}
	s.data = parsed
	return nil
}

// Save writes the current in-memory tokens to disk.
func (s *ContextStore) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.write(s.path, s.data); err != nil {
		return fmt.Errorf("write context tokens: %w", err)
	}
	return nil
}

// Get returns the context token for a user, or empty string if none.
func (s *ContextStore) Get(userID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[userID]
}

// Set stores a context token and persists it to disk.
func (s *ContextStore) Set(userID, token string) error {
	if userID == "" || token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneContext(s.data)
	next[userID] = token
	if err := s.write(s.path, next); err != nil {
		return fmt.Errorf("write context tokens: %w", err)
	}
	s.data = next
	return nil
}

// Delete removes a user's context token.
func (s *ContextStore) Delete(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneContext(s.data)
	delete(next, userID)
	if err := s.write(s.path, next); err != nil {
		return fmt.Errorf("write context tokens: %w", err)
	}
	s.data = next
	return nil
}

// All returns a copy of all stored tokens.
func (s *ContextStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// Clear persists an empty token set.
func (s *ContextStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string)
	if err := s.write(s.path, next); err != nil {
		return fmt.Errorf("write context tokens: %w", err)
	}
	s.data = next
	return nil
}

func cloneContext(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
