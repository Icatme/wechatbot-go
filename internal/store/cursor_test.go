package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore("", filepath.Join(dir, "cursor.json"))

	if err := s.Set("buf-123"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if s.Get() != "buf-123" {
		t.Fatalf("expected buf-123, got %s", s.Get())
	}

	s2 := NewCursorStore("", s.Path())
	if err := s2.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if s2.Get() != "buf-123" {
		t.Fatalf("loaded cursor mismatch: %s", s2.Get())
	}
}

func TestCursorStoreClear(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore("", filepath.Join(dir, "cursor.json"))
	_ = s.Set("buf-123")
	_ = s.Clear()
	if s.Get() != "" {
		t.Fatal("expected empty after clear")
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read cleared cursor: %v", err)
	}
	if got, want := string(data), "{\n  \"get_updates_buf\": \"\"\n}\n"; got != want {
		t.Fatalf("cleared cursor file = %q, want %q", got, want)
	}
}

func TestCursorStoreSetFailureKeepsMemoryAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	s := NewCursorStore("", path)
	if err := s.Set("old"); err != nil {
		t.Fatalf("set old cursor: %v", err)
	}
	oldDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read old cursor: %v", err)
	}
	s.write = func(string, any) error { return errors.New("injected write failure") }

	if err := s.Set("new"); err == nil {
		t.Fatal("expected set failure")
	}
	if got := s.Get(); got != "old" {
		t.Fatalf("cursor memory = %q, want old", got)
	}
	newDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cursor after failure: %v", err)
	}
	if string(newDisk) != string(oldDisk) {
		t.Fatalf("cursor disk changed after failure: %q", newDisk)
	}
}

func TestCursorStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore("", filepath.Join(dir, "not-exist.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.Get() != "" {
		t.Fatal("expected empty cursor")
	}
}
