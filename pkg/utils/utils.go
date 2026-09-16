package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const maxRetries = 3

// httpClient is a shared HTTP client.
// The global Timeout (10 min) acts as a safety net against indefinitely stalled transfers.
// ResponseHeaderTimeout (30s) ensures we fail fast if the server never starts responding.
var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// DownloadFile opens a source for reading, wherever it lives: behind HTTP(S),
// or on the local filesystem.
func DownloadFile(ctx context.Context, source string) (io.ReadCloser, error) {
	switch scheme, path := splitSource(source); scheme {
	case "", "file":
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		return file, nil
	case "http", "https":
		return httpGet(ctx, source)
	default:
		return nil, fmt.Errorf("unsupported scheme %q in %s", scheme, source)
	}
}

// splitSource classifies a source: the scheme it names, and the filesystem path
// it points at when it names no scheme at all or the "file" one.
//
// A mirror maintained with rsync is a directory, so firmirror reads it where it
// sits instead of asking for a web server in front of it.
func splitSource(source string) (scheme, path string) {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" {
		// Not a URL: a plain path, absolute or relative.
		return "", source
	}
	return parsed.Scheme, parsed.Path
}

func httpGet(ctx context.Context, url string) (io.ReadCloser, error) {
	var resp *http.Response
	var err error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("User-Agent", "firmirror")

		resp, err = httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries+1, err)
		}

		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}

		resp.Body.Close()

		// Retry on 5xx errors and rate limiting
		if (resp.StatusCode >= 500 || resp.StatusCode == 429) && attempt < maxRetries {
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries+1, err)
}

// DownloadFileToDest copies a source to a local file. A local source is copied
// too: the firmware is unpacked and repackaged in the temporary directory the
// caller owns, so the mirror it came from is never written to.
func DownloadFileToDest(ctx context.Context, source, file string) error {
	out, err := os.Create(file)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := DownloadFile(ctx, source)
	if err != nil {
		return err
	}
	defer resp.Close()

	_, err = io.Copy(out, resp)
	return err
}
