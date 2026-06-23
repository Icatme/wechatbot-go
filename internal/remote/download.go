// Package remote downloads media from HTTP(S) URLs for WeChat upload.
package remote

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
)

const maxDownloadBytes = 100 * 1024 * 1024

// Download fetches a remote URL and returns the body bytes plus a suggested filename.
// The context controls the request timeout/cancellation.
// Downloads are capped at 100 MiB to avoid unbounded memory use.
func Download(ctx context.Context, rawURL string) (data []byte, fileName string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("invalid remote URL %q", rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch remote media: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("remote media download failed: HTTP %d", resp.StatusCode)
	}

	reader := io.LimitReader(resp.Body, maxDownloadBytes+1)
	data, err = io.ReadAll(reader)
	if err != nil {
		return nil, "", fmt.Errorf("read remote media: %w", err)
	}
	if len(data) > maxDownloadBytes {
		return nil, "", fmt.Errorf("remote media exceeds %d bytes", maxDownloadBytes)
	}

	fileName = suggestedFileName(resp, u)
	return data, fileName, nil
}

func suggestedFileName(resp *http.Response, u *url.URL) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				return path.Base(name)
			}
		}
	}
	if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
		return base
	}
	return ""
}
