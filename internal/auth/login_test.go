package auth

import (
	"os"
	"path/filepath"
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
