package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Icatme/wechatbot-go/internal/persist"
)

// CursorStore persists the get_updates_buf cursor so the bot can resume polling after a restart.
type CursorStore struct {
	path  string
	write func(string, any) error
	mu    sync.RWMutex
	buf   string
}

// CursorData is the on-disk format for future extensibility.
type CursorData struct {
	GetUpdatesBuf string `json:"get_updates_buf"`
}

// NewCursorStore creates a store backed by the given file path.
// If path is empty, it defaults to ~/.wechatbot/accounts/{accountID}/cursor.json
// when accountID is non-empty, otherwise ~/.wechatbot/cursor.json.
func NewCursorStore(accountID, path string) *CursorStore {
	if path == "" {
		path = filepath.Join(AccountStateDir(accountID), "cursor.json")
	}
	return &CursorStore{path: path, write: persist.WriteJSONAtomic}
}

// Path returns the backing file path.
func (s *CursorStore) Path() string {
	return s.path
}

// Load reads the cursor from disk.
func (s *CursorStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.buf = ""
			return nil
		}
		return fmt.Errorf("read cursor: %w", err)
	}

	if len(data) == 0 {
		s.buf = ""
		return nil
	}

	var parsed CursorData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("decode cursor: %w", err)
	}
	s.buf = parsed.GetUpdatesBuf
	return nil
}

// Save persists the current cursor to disk.
func (s *CursorStore) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.write(s.path, CursorData{GetUpdatesBuf: s.buf}); err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	return nil
}

// Get returns the current cursor value.
func (s *CursorStore) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buf
}

// Set updates the cursor and persists it.
func (s *CursorStore) Set(buf string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(s.path, CursorData{GetUpdatesBuf: buf}); err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	s.buf = buf
	return nil
}

// Clear persists an empty cursor.
func (s *CursorStore) Clear() error {
	return s.Set("")
}
