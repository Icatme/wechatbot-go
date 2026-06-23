package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CursorStore persists the get_updates_buf cursor so the bot can resume polling after a restart.
type CursorStore struct {
	path string
	mu   sync.RWMutex
	buf  string
}

// CursorData is the on-disk format for future extensibility.
type CursorData struct {
	GetUpdatesBuf string `json:"get_updates_buf"`
}

// NewCursorStore creates a store backed by the given file path.
// If path is empty, it defaults to ~/.wechatbot/cursor.json.
func NewCursorStore(path string) *CursorStore {
	if path == "" {
		path = filepath.Join(DefaultStateDir(), "cursor.json")
	}
	return &CursorStore{path: path}
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
	buf := s.buf
	s.mu.RUnlock()

	if err := ensureDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("ensure state dir: %w", err)
	}

	out, err := json.MarshalIndent(CursorData{GetUpdatesBuf: buf}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cursor: %w", err)
	}

	if err := os.WriteFile(s.path, append(out, '\n'), 0600); err != nil {
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
	s.buf = buf
	s.mu.Unlock()
	return s.Save()
}

// Clear removes the persisted cursor.
func (s *CursorStore) Clear() error {
	s.mu.Lock()
	s.buf = ""
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return err
	}
	_ = os.Remove(s.path)
	return nil
}
