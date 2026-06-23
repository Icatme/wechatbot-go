package protocol

import (
	"context"
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
