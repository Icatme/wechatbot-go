package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoggerLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: WarnLevel, Output: &buf})
	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")
	out := buf.String()
	if strings.Contains(out, "debug") || strings.Contains(out, "info") {
		t.Fatal("debug/info should be filtered")
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "error") {
		t.Fatal("warn/error should be present")
	}
}

func TestSensitiveFieldRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: InfoLevel, Output: &buf})
	logger.Info("request", F("bot_token", "secret"), F("user_id", "u1"))
	out := buf.String()
	if strings.Contains(out, "secret") {
		t.Fatal("bot_token value should be redacted")
	}
	if !strings.Contains(out, "u1") {
		t.Fatal("user_id value should be visible")
	}
}

func TestRedactString(t *testing.T) {
	s := "url?bot_token=abc123&context_token=xyz&user_id=1 cdn=https://cdn.example/download?signature=signed"
	got := RedactString(s)
	if strings.Contains(got, "abc123") || strings.Contains(got, "xyz") || strings.Contains(got, "signed") {
		t.Fatalf("tokens should be redacted: %s", got)
	}
	if !strings.Contains(got, "user_id=1") {
		t.Fatalf("non-sensitive param should remain: %s", got)
	}
}

func TestLoggerRedactsNestedAndMalformedValues(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: InfoLevel, Output: &buf, ExtraSensitiveKeys: []string{"session_cookie"}})
	logger.Info(
		`failed: {"qrcode":"qr-secret","local_token_list":["old-a","old-b"]`,
		F("response", map[string]interface{}{
			"status": "failed",
			"nested": []interface{}{map[string]string{
				"aes_key":         "aes-secret",
				"upload_full_url": "https://cdn.example/upload?signature=url-secret",
			}},
		}),
		F("session_cookie", "cookie-secret"),
	)
	out := buf.String()
	for _, secret := range []string{"qr-secret", "old-a", "old-b", "aes-secret", "url-secret", "cookie-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, "status") {
		t.Fatalf("non-sensitive diagnostics were lost: %s", out)
	}
}

func TestLoggerOutputsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Options{Level: InfoLevel, Output: &buf})
	logger.Info("hello", F("count", 42))
	out := buf.String()
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("expected JSON object, got %s", out)
	}
	if !strings.Contains(out, `"count":42`) {
		t.Fatalf("expected numeric field, got %s", out)
	}
}
