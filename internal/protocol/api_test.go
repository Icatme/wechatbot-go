package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestAPIErrorRedactsSensitiveResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ret":-2,"errcode":401,"status":"failed","details":{"bot_token":"token-secret","local_token_list":["old-a","old-b"],"upload_full_url":"https://cdn.example/upload?signature=url-secret"}}`))
	}))
	defer server.Close()

	err := NewClient().SendMessage(context.Background(), server.URL, "request-token", map[string]interface{}{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	for _, secret := range []string{"token-secret", "old-a", "old-b", "url-secret"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(apiErr.Message, secret) {
			t.Errorf("API error contains %q: %v", secret, err)
		}
	}
	if apiErr.HTTPStatus != http.StatusBadGateway || apiErr.RetCode != -2 || apiErr.ErrCode != 401 {
		t.Fatalf("API error lost response dimensions: %+v", apiErr)
	}
	if !strings.Contains(apiErr.Message, `"status":"failed"`) {
		t.Fatalf("API error lost non-sensitive body diagnostics: %s", apiErr.Message)
	}
}

func TestAPIErrorRedactsMalformedResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`partial={"verify_code":"code-secret","aes_key":"aes-secret"`))
	}))
	defer server.Close()

	_, err := NewClient().GetQRCode(context.Background(), server.URL, nil)
	if err == nil {
		t.Fatal("expected API error")
	}
	if strings.Contains(err.Error(), "code-secret") || strings.Contains(err.Error(), "aes-secret") {
		t.Fatalf("API error leaked malformed response body: %v", err)
	}
	if !strings.Contains(err.Error(), "partial=") || !strings.Contains(err.Error(), "http=500") {
		t.Fatalf("API error lost diagnostics: %v", err)
	}
}

func TestAPIErrorRawFallbackIsBoundedUTF8(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ret":-2,"errcode":401,"bot_token":"token-secret","padding":"` + strings.Repeat("界", maxAPIErrorMessageBytes) + `"}`))
	}))
	defer server.Close()

	err := NewClient().SendMessage(context.Background(), server.URL, "request-token", map[string]interface{}{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if len(apiErr.Message) > maxAPIErrorMessageBytes || !utf8.ValidString(apiErr.Message) {
		t.Fatalf("raw fallback is not bounded valid UTF-8: bytes=%d valid=%v", len(apiErr.Message), utf8.ValidString(apiErr.Message))
	}
	if strings.Contains(apiErr.Message, "token-secret") || !strings.HasSuffix(apiErr.Message, "…(truncated)") {
		t.Fatalf("raw fallback was unsafe or not marked as truncated: %s", apiErr.Message)
	}
	if apiErr.HTTPStatus != http.StatusBadGateway || apiErr.RetCode != -2 || apiErr.ErrCode != 401 {
		t.Fatalf("API error lost response dimensions: %+v", apiErr)
	}
	for _, diagnostic := range []string{"http=502", "ret=-2", "errcode=401"} {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Fatalf("API error lost %q: %v", diagnostic, err)
		}
	}
}

func TestAPIErrorEnvelopeMessageIsBoundedUTF8(t *testing.T) {
	apiErr := newAPIError("/ilink/bot/sendmessage", http.StatusBadRequest, nil, apiEnvelope{
		Ret:     -2,
		ErrCode: 422,
		ErrMsg:  "authorization=Bearer token-secret status=401 " + string([]byte{0xff}) + strings.Repeat("界", maxAPIErrorMessageBytes),
	})
	if len(apiErr.Message) > maxAPIErrorMessageBytes || !utf8.ValidString(apiErr.Message) {
		t.Fatalf("envelope errmsg is not bounded valid UTF-8: bytes=%d valid=%v", len(apiErr.Message), utf8.ValidString(apiErr.Message))
	}
	if strings.Contains(apiErr.Message, "token-secret") || !strings.HasSuffix(apiErr.Message, "…(truncated)") {
		t.Fatalf("envelope errmsg was unsafe or not marked as truncated: %s", apiErr.Message)
	}
	if apiErr.HTTPStatus != http.StatusBadRequest || apiErr.RetCode != -2 || apiErr.ErrCode != 422 {
		t.Fatalf("API error lost response dimensions: %+v", apiErr)
	}
	for _, diagnostic := range []string{"http=400", "ret=-2", "errcode=422"} {
		if !strings.Contains(apiErr.Error(), diagnostic) {
			t.Fatalf("API error lost %q: %v", diagnostic, apiErr)
		}
	}
}

func TestAPIErrorEnvelopeRedactsDelimitedCredentialsAndRelativeURLs(t *testing.T) {
	apiErr := newAPIError("/ilink/bot/sendmessage", http.StatusBadGateway, nil, apiEnvelope{
		Ret:    -2,
		ErrMsg: `password="correct horse battery staple" proxy=//user:pass@proxy.example/%zz?signature=signed-secret status=502`,
	})
	for _, secret := range []string{"correct horse", "//user", ":pass@", "signed-secret"} {
		if strings.Contains(apiErr.Message, secret) || strings.Contains(apiErr.Error(), secret) {
			t.Fatalf("API error contains %q: %v", secret, apiErr)
		}
	}
	for _, diagnostic := range []string{"proxy.example/%zz", "status=502", "http=502", "ret=-2"} {
		if !strings.Contains(apiErr.Error(), diagnostic) {
			t.Fatalf("API error lost %q: %v", diagnostic, apiErr)
		}
	}
}

func TestGetQRCodeRequestErrorRedactsURL(t *testing.T) {
	_, err := NewClient().GetQRCode(context.Background(), "https://api.example/\n?bot_token=token-secret", nil)
	if err == nil {
		t.Fatal("expected request construction error")
	}
	if strings.Contains(err.Error(), "token-secret") || !strings.Contains(err.Error(), "api.example") {
		t.Fatalf("request construction error was unsafe or incomplete: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || strings.Contains(urlErr.URL, "token-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
}

func TestPollQRStatusNetworkErrorRedactsCredentials(t *testing.T) {
	cause := errors.New("network unavailable")
	client := NewClient()
	client.HTTP = &http.Client{Transport: protocolRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}

	_, err := client.PollQRStatus(context.Background(), "https://api.example", "qr-secret", "code-secret")
	if err == nil {
		t.Fatal("expected polling error")
	}
	if strings.Contains(err.Error(), "qr-secret") || strings.Contains(err.Error(), "code-secret") {
		t.Fatalf("QR polling error leaked credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "api.example/ilink/bot/get_qrcode_status") || !errors.Is(err, cause) {
		t.Fatalf("QR polling error lost diagnostics or cause: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || strings.Contains(urlErr.URL, "qr-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
}

func TestUploadToCDNRedactsNetworkAndResponseErrors(t *testing.T) {
	t.Run("network URL", func(t *testing.T) {
		cause := errors.New("network unavailable")
		client := NewClient()
		client.HTTP = &http.Client{Transport: protocolRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, cause
		})}

		_, err := client.UploadToCDN(context.Background(), "https://cdn.example/upload?signature=signed-secret", []byte("encrypted"))
		if err == nil || strings.Contains(err.Error(), "signed-secret") || !errors.Is(err, cause) {
			t.Fatalf("unsafe or incomplete CDN network error: %v", err)
		}
	})

	t.Run("response header", func(t *testing.T) {
		client := NewClient()
		client.HTTP = &http.Client{Transport: protocolRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header: http.Header{
					"X-Error-Message": []string{`upload denied x-encrypted-param=download-secret x_encrypted_param=alternate-secret status=failed`},
				},
				Body: http.NoBody,
			}, nil
		})}

		_, err := client.UploadToCDN(context.Background(), "https://cdn.example/upload", []byte("encrypted"))
		if err == nil || strings.Contains(err.Error(), "download-secret") || strings.Contains(err.Error(), "alternate-secret") {
			t.Fatalf("CDN response error leaked credentials: %v", err)
		}
		if !strings.Contains(err.Error(), "client error 403") || !strings.Contains(err.Error(), "upload denied") || !strings.Contains(err.Error(), "status=failed") {
			t.Fatalf("CDN response error lost diagnostics: %v", err)
		}
	})

	t.Run("delimited response header", func(t *testing.T) {
		client := NewClient()
		client.HTTP = &http.Client{Transport: protocolRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header: http.Header{
					"X-Error-Message": []string{`upload denied password=abc,def proxy=//user:pass@proxy.example/%zz?signature=signed-secret status=failed`},
				},
				Body: http.NoBody,
			}, nil
		})}

		_, err := client.UploadToCDN(context.Background(), "https://cdn.example/upload", []byte("encrypted"))
		if err == nil {
			t.Fatal("expected CDN response error")
		}
		for _, secret := range []string{"abc,def", "//user", ":pass@", "signed-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("CDN response error contains %q: %v", secret, err)
			}
		}
		for _, diagnostic := range []string{"proxy.example/%zz", "status=failed"} {
			if !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("CDN response error lost %q: %v", diagnostic, err)
			}
		}
	})
}

type protocolRoundTripFunc func(*http.Request) (*http.Response, error)

func (f protocolRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
