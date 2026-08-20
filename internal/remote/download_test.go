package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDownload(t *testing.T) {
	data := []byte("hello image")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment; filename=photo.png")
		w.Write(data)
	}))
	defer ts.Close()

	got, name, err := Download(context.Background(), ts.URL+"/pic.png")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("data mismatch")
	}
	if name != "photo.png" {
		t.Fatalf("expected photo.png, got %q", name)
	}
}

func TestDownloadFromURLPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "body")
	}))
	defer ts.Close()

	_, name, err := Download(context.Background(), ts.URL+"/report.pdf")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if name != "report.pdf" {
		t.Fatalf("expected report.pdf, got %q", name)
	}
}

func TestDownloadInvalidURL(t *testing.T) {
	_, _, err := Download(context.Background(), "ftp://example.com/a.png?signature=signed-secret")
	if err == nil {
		t.Fatal("expected error for invalid scheme")
	}
	if strings.Contains(err.Error(), "signed-secret") || !strings.Contains(err.Error(), "example.com/a.png") {
		t.Fatalf("invalid URL error was unsafe or incomplete: %v", err)
	}
}

func TestDownloadInvalidAuthorityURLRedactsUserinfo(t *testing.T) {
	_, _, err := Download(context.Background(), "//user:pass@host.example/%zz")
	if err == nil {
		t.Fatal("expected error for malformed authority URL")
	}
	if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "pass") {
		t.Fatalf("invalid authority URL error leaked userinfo: %v", err)
	}
	if !strings.Contains(err.Error(), "host.example/%zz") {
		t.Fatalf("invalid authority URL error lost diagnostics: %v", err)
	}
}

func TestDownloadNetworkErrorRedactsSignedURL(t *testing.T) {
	cause := errors.New("network unavailable")
	client := &http.Client{Transport: remoteRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}

	_, _, err := DownloadWithClient(context.Background(), client, "https://media.example/file?signature=signed-secret")
	if err == nil || strings.Contains(err.Error(), "signed-secret") {
		t.Fatalf("remote download leaked signed URL: %v", err)
	}
	if !strings.Contains(err.Error(), "media.example/file") || !errors.Is(err, cause) {
		t.Fatalf("remote download lost diagnostics or cause: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || strings.Contains(urlErr.URL, "signed-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
}

func TestDownloadContextTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte("late"))
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, _, err := Download(ctx, ts.URL)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDownloadMaxSize(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", maxDownloadBytes+1))
	}))
	defer ts.Close()

	_, _, err := Download(context.Background(), ts.URL)
	if err == nil {
		t.Fatal("expected max size error")
	}
}

type remoteRoundTripFunc func(*http.Request) (*http.Response, error)

func (f remoteRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
