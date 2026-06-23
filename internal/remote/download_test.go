package remote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	_, _, err := Download(context.Background(), "ftp://example.com/a.png")
	if err == nil {
		t.Fatal("expected error for invalid scheme")
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
