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
	"time"
)

// Download fetches a remote URL and returns the body bytes plus a suggested filename.
// The context controls the request timeout/cancellation.
func Download(ctx context.Context, rawURL string) (data []byte, fileName string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("invalid remote URL %q", rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch remote media: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("remote media download failed: HTTP %d", resp.StatusCode)
	}

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read remote media: %w", err)
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
