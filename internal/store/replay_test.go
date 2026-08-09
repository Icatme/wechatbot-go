package store

import (
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

func TestReplayStoreMissingFile(t *testing.T) {
	store := NewReplayStore("", filepath.Join(t.TempDir(), "missing.json"), time.Hour)
	if err := store.Load(); err != nil {
		t.Fatalf("missing replay file should not error: %v", err)
	}
	if store.Seen("message:1") {
		t.Fatal("missing replay file should load an empty store")
	}
}
