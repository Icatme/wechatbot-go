package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChannelVersionBumped(t *testing.T) {
	if ChannelVersion != "0.3.0" {
		t.Fatalf("expected ChannelVersion 0.3.0, got %s", ChannelVersion)
	}
}

func TestPollQRStatusDecodeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json"))
	}))
	defer ts.Close()

	_, err := NewClient().PollQRStatus(context.Background(), ts.URL, "qr", "")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestPollQRStatusHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer ts.Close()

	_, err := NewClient().PollQRStatus(context.Background(), ts.URL, "qr", "")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", apiErr.HTTPStatus)
	}
}

func TestGetUpdatesDecodeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json"))
	}))
	defer ts.Close()

	_, err := NewClient().GetUpdates(context.Background(), ts.URL, "tok", "", 15*time.Second)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected decode response error, got %v", err)
	}
}

func TestSendMessageEmptyResponseReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	err := NewClient().SendMessage(context.Background(), ts.URL, "tok", map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected decode response error, got %v", err)
	}
}

func TestBuildMessageUsesCallerIdentityAndOptionalRunID(t *testing.T) {
	item := map[string]interface{}{
		"type":      1,
		"text_item": map[string]string{"text": "hello"},
	}
	msg := BuildMessage("user-1", "context-1", "client-1", "run-1", item)
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"client_id":"client-1","context_token":"context-1","from_user_id":"","item_list":[{"text_item":{"text":"hello"},"type":1}],"message_state":2,"message_type":2,"run_id":"run-1","to_user_id":"user-1"}`
	if string(raw) != want {
		t.Fatalf("message JSON = %s, want %s", raw, want)
	}

	withoutRun := BuildMessage("user-1", "context-1", "client-2", "", item)
	if _, ok := withoutRun["run_id"]; ok {
		t.Fatal("empty run_id should be omitted")
	}
}
