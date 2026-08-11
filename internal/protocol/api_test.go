package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChannelVersionBumped(t *testing.T) {
	if ChannelVersion != "0.4.0" {
		t.Fatalf("expected ChannelVersion 0.4.0, got %s", ChannelVersion)
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
	if apiErr.Endpoint != "/ilink/bot/get_qrcode_status" {
		t.Fatalf("endpoint = %q", apiErr.Endpoint)
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

func TestAPIErrorPreservesResponseDimensions(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantRet     int
		wantErrCode int
		wantCode    int
		wantExpired bool
		wantMessage string
	}{
		{
			name:        "ret session expired",
			status:      http.StatusOK,
			body:        `{"ret":-14,"errmsg":"ret expired"}`,
			wantRet:     -14,
			wantCode:    -14,
			wantExpired: true,
			wantMessage: "ret expired",
		},
		{
			name:        "errcode session expired",
			status:      http.StatusOK,
			body:        `{"ret":0,"errcode":-14,"errmsg":"errcode expired"}`,
			wantErrCode: -14,
			wantCode:    -14,
			wantExpired: true,
			wantMessage: "errcode expired",
		},
		{
			name:        "http session expired",
			status:      http.StatusUnauthorized,
			body:        `{"ret":-14,"errcode":-14,"errmsg":"http expired"}`,
			wantRet:     -14,
			wantErrCode: -14,
			wantCode:    -14,
			wantExpired: true,
			wantMessage: "http expired",
		},
		{
			name:        "other ret",
			status:      http.StatusOK,
			body:        `{"ret":-2,"errmsg":"other"}`,
			wantRet:     -2,
			wantCode:    -2,
			wantMessage: "other",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			err := NewClient().SendMessage(context.Background(), server.URL, "token", map[string]interface{}{})
			var apiErr *APIError
			if !errors.As(fmt.Errorf("wrapped: %w", err), &apiErr) {
				t.Fatalf("expected wrapped APIError, got %T: %v", err, err)
			}
			if apiErr.Endpoint != "/ilink/bot/sendmessage" || apiErr.HTTPStatus != tc.status {
				t.Fatalf("location = endpoint %q HTTP %d", apiErr.Endpoint, apiErr.HTTPStatus)
			}
			if apiErr.RetCode != tc.wantRet || apiErr.ErrCode != tc.wantErrCode || apiErr.Code() != tc.wantCode {
				t.Fatalf("codes = ret %d errcode %d effective %d", apiErr.RetCode, apiErr.ErrCode, apiErr.Code())
			}
			if apiErr.IsSessionExpired() != tc.wantExpired || apiErr.Message != tc.wantMessage {
				t.Fatalf("error = %+v", apiErr)
			}
		})
	}
}
