package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore(filepath.Join(dir, "context_tokens.json"))

	if err := s.Set("user1", "token-a"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if err := s.Set("user2", "token-b"); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	if s.Get("user1") != "token-a" {
		t.Fatalf("expected token-a, got %s", s.Get("user1"))
	}

	s2 := NewContextStore(s.Path())
	if err := s2.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if s2.Get("user1") != "token-a" || s2.Get("user2") != "token-b" {
		t.Fatalf("loaded tokens mismatch: %+v", s2.All())
	}
}

func TestContextStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore(filepath.Join(dir, "context_tokens.json"))
	_ = s.Set("user1", "token-a")
	_ = s.Delete("user1")
	if s.Get("user1") != "" {
		t.Fatal("expected token deleted")
	}
}

func TestContextStoreClear(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore(filepath.Join(dir, "context_tokens.json"))
	_ = s.Set("user1", "token-a")
	_ = s.Clear()
	if len(s.All()) != 0 {
		t.Fatal("expected empty after clear")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("expected file removed after clear")
	}
}

func TestContextStoreEmptyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	s := NewContextStore("")
	if s.Path() == "" {
		t.Fatal("expected default path")
	}
}

func TestContextStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore(filepath.Join(dir, "not-exist.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}
