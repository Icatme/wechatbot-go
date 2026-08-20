package wechatbot

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestCDNDownloadNetworkErrorRedactsSignedURL(t *testing.T) {
	cause := errors.New("network unavailable")
	bot := New()
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}

	_, err := bot.DownloadRaw(context.Background(), &CDNMedia{
		FullURL: "https://cdn.example/download?signature=signed-secret&token=token-secret",
		AESKey:  "unused",
	}, "")
	if err == nil {
		t.Fatal("expected CDN download error")
	}
	if strings.Contains(err.Error(), "signed-secret") || strings.Contains(err.Error(), "token-secret") {
		t.Fatalf("CDN download error leaked signed URL: %v", err)
	}
	if !strings.Contains(err.Error(), "cdn.example/download") || !errors.Is(err, cause) {
		t.Fatalf("CDN download error lost diagnostics or cause: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || strings.Contains(urlErr.URL, "signed-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
}
