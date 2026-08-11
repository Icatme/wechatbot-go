package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type loginRequests struct {
	qrBody    []byte
	qrCalls   int
	pollCalls int
}

func newLoginTestClient(t *testing.T, statusBody string) (*protocol.Client, *loginRequests) {
	t.Helper()
	requests := &loginRequests{}
	client := protocol.NewClient()
	client.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var responseBody string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			requests.qrCalls++
			if req.Body != nil {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				requests.qrBody = body
			}
			responseBody = `{"qrcode":"qr-token","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			requests.pollCalls++
			responseBody = statusBody
		default:
			return nil, errors.New("unexpected auth request: " + req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})}
	return client, requests
}

func assertNoLocalTokens(t *testing.T, body []byte) {
	t.Helper()
	if len(body) == 0 {
		return
	}
	var request struct {
		LocalTokens []string `json:"local_token_list"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode QR request: %v", err)
	}
	if len(request.LocalTokens) != 0 {
		t.Fatalf("forced login sent local tokens: %v", request.LocalTokens)
	}
}

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

func TestFinalizeLoginFailsWhenCredentialsCannotBeSaved(t *testing.T) {
	status := &protocol.QRStatusResponse{
		BotToken: "bt",
		BotID:    "bid",
		UserID:   "uid",
	}
	creds, err := finalizeLogin(status, "https://default.com", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "save credentials") {
		t.Fatalf("expected persistence error, got credentials=%+v err=%v", creds, err)
	}
	if creds != nil {
		t.Fatalf("returned credentials after persistence failure: %+v", creds)
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

func TestClearCredentialsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if err := ClearCredentials(path); err != nil {
		t.Fatalf("missing credentials should already be clear: %v", err)
	}
}

func TestLoginNonForceReturnsStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	want := &Credentials{Token: "stored-token", BaseURL: "https://stored.example", AccountID: "bot-1", UserID: "user-1"}
	if err := SaveCredentials(want, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client := protocol.NewClient()
	requestCount := 0
	client.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestCount++
		return nil, errors.New("unexpected network request")
	})}

	got, err := Login(context.Background(), client, LoginOptions{CredPath: path})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if requestCount != 0 {
		t.Fatalf("non-force login made %d network requests", requestCount)
	}
	if got.Token != want.Token || got.AccountID != want.AccountID || got.UserID != want.UserID {
		t.Fatalf("loaded credentials mismatch: %+v", got)
	}
}

func TestLoginForceDoesNotReuseStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(&Credentials{Token: "invalid-token", AccountID: "bot-old", UserID: "user-old"}, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client, requests := newLoginTestClient(t, `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://api.example"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if got.Token != "fresh-token" {
		t.Fatalf("expected fresh credentials, got %+v", got)
	}
	if requests.qrCalls != 1 || requests.pollCalls != 1 {
		t.Fatalf("unexpected request counts: QR=%d poll=%d", requests.qrCalls, requests.pollCalls)
	}
	assertNoLocalTokens(t, requests.qrBody)
}

func TestLoginForceBindedRedirectDoesNotReuseStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(&Credentials{Token: "invalid-token", AccountID: "bot-old", UserID: "user-old"}, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client, requests := newLoginTestClient(t, `{"status":"binded_redirect"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if err == nil || !strings.Contains(err.Error(), "forced login cannot reuse existing credentials") {
		t.Fatalf("expected forced binded_redirect rejection, got credentials=%+v err=%v", got, err)
	}
	if got != nil {
		t.Fatalf("forced login returned stored credentials: %+v", got)
	}
	assertNoLocalTokens(t, requests.qrBody)
}

func TestLoginRejectsInvalidatedConfirmedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	client, _ := newLoginTestClient(t, `{"status":"confirmed","bot_token":"invalid-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if !errors.Is(err, ErrInvalidatedCredential) {
		t.Fatalf("expected ErrInvalidatedCredential, got credentials=%+v err=%v", got, err)
	}
	if got != nil {
		t.Fatalf("invalidated credentials returned: %+v", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("invalidated credentials were persisted: %v", statErr)
	}
}
