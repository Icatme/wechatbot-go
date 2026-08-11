package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplayStoreRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replay_dedupe.json")
	baseTime := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	store := NewReplayStore("", path, 24*time.Hour)
	store.now = func() time.Time { return baseTime }
	if err := store.Commit("message:42"); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if !store.Seen("message:42") {
		t.Fatal("committed replay key should be seen")
	}

	reloaded := NewReplayStore("", path, 24*time.Hour)
	reloaded.now = func() time.Time { return baseTime.Add(23 * time.Hour) }
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if !reloaded.Seen("message:42") {
		t.Fatal("replay key should survive restart within TTL")
	}

	reloaded.now = func() time.Time { return baseTime.Add(25 * time.Hour) }
	if reloaded.Seen("message:42") {
		t.Fatal("expired replay key should be pruned")
	}
}

func TestReplayStoreCommitAllPersistsKeysTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay_dedupe.json")
	baseTime := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	s := NewReplayStore("", path, 24*time.Hour)
	s.now = func() time.Time { return baseTime }

	if err := s.CommitAll("message:42", "client:7", ""); err != nil {
		t.Fatalf("commit keys: %v", err)
	}
	if !s.SeenAny("missing", "client:7") {
		t.Fatal("expected one committed key to be seen")
	}

	reloaded := NewReplayStore("", path, 24*time.Hour)
	reloaded.now = s.now
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load replay store: %v", err)
	}
	if !reloaded.SeenAny("message:42", "client:7") {
		t.Fatal("committed keys did not survive reload")
	}
}

func TestReplayStoreCommitFailureKeepsMemoryAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay_dedupe.json")
	s := NewReplayStore("", path, time.Hour)
	if err := s.Commit("old"); err != nil {
		t.Fatalf("commit old key: %v", err)
	}
	oldDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read old replay state: %v", err)
	}
	s.write = func(string, any) error { return errors.New("injected write failure") }

	if err := s.CommitAll("new-1", "new-2"); err == nil {
		t.Fatal("expected commit failure")
	}
	if !s.Seen("old") {
		t.Fatal("old replay key disappeared after failure")
	}
	if s.SeenAny("new-1", "new-2") {
		t.Fatal("new replay keys were published after failure")
	}
	newDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replay state after failure: %v", err)
	}
	if string(newDisk) != string(oldDisk) {
		t.Fatalf("replay disk changed after failure: %q", newDisk)
	}
}

func TestReplayStoreMissingFile(t *testing.T) {
	store := NewReplayStore("", filepath.Join(t.TempDir(), "missing.json"), time.Hour)
	if err := store.Load(); err != nil {
		t.Fatalf("missing replay file should not error: %v", err)
	}
	if store.Seen("message:1") {
		t.Fatal("missing replay file should load an empty store")
	}
}
