package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestContextStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore("", filepath.Join(dir, "context_tokens.json"))

	if err := s.Set("user1", "token-a"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if err := s.Set("user2", "token-b"); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	if s.Get("user1") != "token-a" {
		t.Fatalf("expected token-a, got %s", s.Get("user1"))
	}

	s2 := NewContextStore("", s.Path())
	if err := s2.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if s2.Get("user1") != "token-a" || s2.Get("user2") != "token-b" {
		t.Fatalf("loaded tokens mismatch: %+v", s2.All())
	}
}

func TestContextStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore("", filepath.Join(dir, "context_tokens.json"))
	_ = s.Set("user1", "token-a")
	_ = s.Delete("user1")
	if s.Get("user1") != "" {
		t.Fatal("expected token deleted")
	}
}

func TestContextStoreClear(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore("", filepath.Join(dir, "context_tokens.json"))
	_ = s.Set("user1", "token-a")
	_ = s.Clear()
	if len(s.All()) != 0 {
		t.Fatal("expected empty after clear")
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read cleared contexts: %v", err)
	}
	var persisted map[string]string
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode cleared contexts: %v", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("persisted contexts after clear = %v, want empty", persisted)
	}
}

func TestContextStoreMutationFailureKeepsMemoryAndDisk(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ContextStore) error
	}{
		{name: "set", mutate: func(s *ContextStore) error { return s.Set("user1", "new") }},
		{name: "delete", mutate: func(s *ContextStore) error { return s.Delete("user1") }},
		{name: "clear", mutate: func(s *ContextStore) error { return s.Clear() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "context_tokens.json")
			s := NewContextStore("", path)
			if err := s.Set("user1", "old"); err != nil {
				t.Fatalf("set old token: %v", err)
			}
			if err := s.Set("user2", "keep"); err != nil {
				t.Fatalf("set second token: %v", err)
			}
			oldMemory := s.All()
			oldDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read old contexts: %v", err)
			}
			s.write = func(string, any) error { return errors.New("injected write failure") }

			if err := tt.mutate(s); err == nil {
				t.Fatal("expected mutation failure")
			}
			if got := s.All(); !reflect.DeepEqual(got, oldMemory) {
				t.Fatalf("context memory = %v, want %v", got, oldMemory)
			}
			newDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read contexts after failure: %v", err)
			}
			if string(newDisk) != string(oldDisk) {
				t.Fatalf("context disk changed after failure: %q", newDisk)
			}
		})
	}
}

func TestContextStoreEmptyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	s := NewContextStore("", "")
	if s.Path() == "" {
		t.Fatal("expected default path")
	}
}

func TestContextStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewContextStore("", filepath.Join(dir, "not-exist.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}

func TestContextStoreConcurrentSetPersistsAllTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context_tokens.json")
	store := NewContextStore("", path)
	const total = 64
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.Set(fmt.Sprintf("user-%d", i), fmt.Sprintf("token-%d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Set: %v", err)
		}
	}

	reloaded := NewContextStore("", path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load persisted tokens: %v", err)
	}
	if got := len(reloaded.All()); got != total {
		t.Fatalf("persisted tokens = %d, want %d", got, total)
	}
}
