package store

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/Icatme/wechatbot-go/internal/persist"
)

const reauthStateVersion = 1

// ReauthRecord is the durable, fail-closed marker for an interrupted or
// incomplete session transition. InvalidTokenSHA256 is a fingerprint, never
// the credential itself.
type ReauthRecord struct {
	Version            int      `json:"version"`
	InvalidTokenSHA256 string   `json:"invalid_token_sha256,omitempty"`
	AccountID          string   `json:"account_id,omitempty"`
	ContextPaths       []string `json:"context_paths,omitempty"`
}

// ReauthStore persists the session-transition marker next to credentials.
type ReauthStore struct {
	path   string
	write  func(string, any) error
	remove func(string) error
	mu     sync.RWMutex
	state  *ReauthRecord
}

// ReauthStatePath derives a marker path unique to a credential file.
func ReauthStatePath(credentialsPath string) string {
	return credentialsPath + ".reauth.json"
}

// NewReauthStore creates a durable reauthentication marker store.
func NewReauthStore(path string) *ReauthStore {
	return &ReauthStore{
		path:   path,
		write:  persist.WriteJSONAtomic,
		remove: os.Remove,
	}
}

// Path returns the marker file path.
func (s *ReauthStore) Path() string { return s.path }

// Load reads and publishes the marker. A malformed marker is an error rather
// than being treated as an inactive transition.
func (s *ReauthStore) Load() (*ReauthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read reauthentication marker: %w", err)
	}
	var parsed ReauthRecord
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode reauthentication marker: %w", err)
	}
	if parsed.Version != reauthStateVersion {
		return nil, fmt.Errorf("unsupported reauthentication marker version %d", parsed.Version)
	}
	if parsed.InvalidTokenSHA256 != "" {
		decoded, err := hex.DecodeString(parsed.InvalidTokenSHA256)
		if err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("invalid reauthentication token fingerprint")
		}
	}
	s.state = cloneReauthRecord(&parsed)
	return cloneReauthRecord(s.state), nil
}

// Required reports whether a marker file exists. Unreadable and malformed
// files count as required so callers remain fail-closed.
func (s *ReauthStore) Required() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != nil {
		return true
	}
	_, err := os.Stat(s.path)
	return err == nil || !os.IsNotExist(err)
}

// Mark atomically persists record before publishing it in memory.
func (s *ReauthStore) Mark(record ReauthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.Version = reauthStateVersion
	record.ContextPaths = append([]string(nil), record.ContextPaths...)
	if err := s.write(s.path, record); err != nil {
		return fmt.Errorf("write reauthentication marker: %w", err)
	}
	s.state = cloneReauthRecord(&record)
	return nil
}

// Clear removes the durable marker before publishing the inactive state.
func (s *ReauthStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove reauthentication marker: %w", err)
	}
	s.state = nil
	return nil
}

func cloneReauthRecord(record *ReauthRecord) *ReauthRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.ContextPaths = append([]string(nil), record.ContextPaths...)
	return &clone
}
