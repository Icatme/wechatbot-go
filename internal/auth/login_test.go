package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

func TestLocalTokenList(t *testing.T) {
	if got := localTokenList(nil); got != nil {
		t.Fatalf("expected nil for nil existing, got %v", got)
	}
	creds := &Credentials{Token: "tok-123"}
	got := localTokenList(creds)
	if len(got) != 1 || got[0] != "tok-123" {
		t.Fatalf("expected [tok-123], got %v", got)
	}
}

func TestFinalizeLogin(t *testing.T) {
	dir := t.TempDir()
	status := &protocol.QRStatusResponse{
		BotToken: "bt",
		BotID:    "bid",
		UserID:   "uid",
		BaseURL:  "https://example.com",
	}
	path := filepath.Join(dir, "creds.json")
	creds, err := finalizeLogin(status, "https://default.com", path)
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if creds.Token != "bt" || creds.AccountID != "bid" || creds.UserID != "uid" {
		t.Fatalf("credentials mismatch: %+v", creds)
	}
	if creds.BaseURL != "https://example.com" {
		t.Fatalf("expected base URL from status, got %s", creds.BaseURL)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("credentials file not saved")
	}
}

func TestLoadCredentialsMissing(t *testing.T) {
	dir := t.TempDir()
	creds, err := LoadCredentials(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if creds != nil {
		t.Fatal("expected nil for missing file")
	}
}

func TestSaveCredentialsReplacesJSONWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create credentials directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatalf("write old credentials: %v", err)
	}
	creds := &Credentials{Token: "secret", BaseURL: "https://example.com", AccountID: "account", UserID: "user"}

	if err := SaveCredentials(creds, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("credentials do not end in newline: %q", data)
	}
	var got Credentials
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if got.Token != creds.Token || got.AccountID != creds.AccountID || got.UserID != creds.UserID {
		t.Fatalf("saved credentials = %+v, want %+v", got, *creds)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got&0077 != 0 {
		t.Fatalf("credentials permissions = %04o, want no group or other permissions", got)
	}
}
