package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReauthStoreRoundTripReplaceAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json.reauth.json")
	store := NewReauthStore(path)
	first := ReauthRecord{InvalidTokenSHA256: strings.Repeat("a", 64), AccountID: "bot-1", ContextPaths: []string{"old-context.json"}}
	if err := store.Mark(first); err != nil {
		t.Fatalf("mark first transition: %v", err)
	}
	second := ReauthRecord{InvalidTokenSHA256: strings.Repeat("b", 64), AccountID: "bot-1", ContextPaths: []string{"one.json", "two.json"}}
	if err := store.Mark(second); err != nil {
		t.Fatalf("replace transition: %v", err)
	}

	reloaded := NewReauthStore(path)
	got, err := reloaded.Load()
	if err != nil {
		t.Fatalf("load transition: %v", err)
	}
	if got == nil || got.InvalidTokenSHA256 != second.InvalidTokenSHA256 || got.AccountID != second.AccountID || !reflect.DeepEqual(got.ContextPaths, second.ContextPaths) {
		t.Fatalf("loaded transition = %+v, want %+v", got, second)
	}
	if err := reloaded.Clear(); err != nil {
		t.Fatalf("clear transition: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker survived clear: %v", err)
	}
}

func TestReauthStoreMutationFailuresDoNotPublishMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reauth.json")
	store := NewReauthStore(path)
	if err := store.Mark(ReauthRecord{AccountID: "old"}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	store.write = func(string, any) error { return errors.New("injected write failure") }
	if err := store.Mark(ReauthRecord{AccountID: "new"}); err == nil {
		t.Fatal("expected mark failure")
	}
	if store.state == nil || store.state.AccountID != "old" {
		t.Fatalf("marker memory changed after failed write: %+v", store.state)
	}

	store.remove = func(string) error { return errors.New("injected remove failure") }
	if err := store.Clear(); err == nil {
		t.Fatal("expected clear failure")
	}
	if store.state == nil || store.state.AccountID != "old" {
		t.Fatalf("marker memory cleared after failed remove: %+v", store.state)
	}
}

func TestReauthStoreMalformedMarkerFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reauth.json")
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if record, err := NewReauthStore(path).Load(); err == nil || record != nil {
		t.Fatalf("malformed marker = %+v, %v", record, err)
	}
}

func TestReauthStoreRejectsInvalidTokenFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reauth.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"invalid_token_sha256":"short"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewReauthStore(path)
	if !store.Required() {
		t.Fatal("invalid marker was not reported as required")
	}
	if record, err := store.Load(); err == nil || record != nil {
		t.Fatalf("invalid fingerprint marker = %+v, %v", record, err)
	}
}
