package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore(filepath.Join(dir, "cursor.json"))

	if err := s.Set("buf-123"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if s.Get() != "buf-123" {
		t.Fatalf("expected buf-123, got %s", s.Get())
	}

	s2 := NewCursorStore(s.Path())
	if err := s2.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if s2.Get() != "buf-123" {
		t.Fatalf("loaded cursor mismatch: %s", s2.Get())
	}
}

func TestCursorStoreClear(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore(filepath.Join(dir, "cursor.json"))
	_ = s.Set("buf-123")
	_ = s.Clear()
	if s.Get() != "" {
		t.Fatal("expected empty after clear")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("expected file removed after clear")
	}
}

func TestCursorStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore(filepath.Join(dir, "not-exist.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.Get() != "" {
		t.Fatal("expected empty cursor")
	}
}
