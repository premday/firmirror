package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxRetries = 3

// schemeInvalid is the scheme splitSource reports for a malformed URL. It cannot
// collide with a real scheme.
const schemeInvalid = "\x00invalid"

// retryBackoff is the wait before the first retry; it grows linearly.
var retryBackoff = time.Second

// errInsecureRedirect is not retried: asking again gets the same answer.
var errInsecureRedirect = errors.New("refusing to follow a redirect from HTTPS to plain HTTP")

// The default transport brings the proxy from the environment and the dial and TLS
// handshake timeouts; ResponseHeaderTimeout (30s) fails fast if the server never
// starts responding.
var httpTransport http.RoundTripper = func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return transport
}()

// sharedTransport defers to httpTransport so a test can replace it after the
// clients are built.
type sharedTransport struct{}

func (sharedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return httpTransport.RoundTrip(req)
}

// newHTTPClient returns a client whose global Timeout (10 min) is a safety net
// against stalled transfers. A strict client refuses a redirect from HTTPS to
// plain HTTP, so what was asked for over HTTPS is never read in the clear.
func newHTTPClient(strict bool) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Minute,
		Transport: sharedTransport{},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// The previous URL may hold a token, and the next host is not owed it.
			req.Header.Del("Referer")
			if strict && req.URL.Scheme == "http" && via[len(via)-1].URL.Scheme == "https" {
				return fmt.Errorf("%w (%s)", errInsecureRedirect, Redacted(req.URL.String()))
			}
			return nil
		},
	}
}

// Vendor catalogs and firmware use the lenient client; the sources that decide
// what hosts are served use the strict one.
var (
	lenientClient = newHTTPClient(false)
	strictClient  = newHTTPClient(true)
)

// ErrPlainHTTP is returned by a strict read of a plain HTTP source.
var ErrPlainHTTP = errors.New("is read over plain HTTP, which whoever is on the way can change: use HTTPS, or allow it if the network is trusted")

// DownloadFile opens a source for reading, wherever it lives: behind HTTP(S),
// or on the local filesystem.
func DownloadFile(ctx context.Context, source string) (io.ReadCloser, error) {
	return open(ctx, source, lenientClient)
}

func open(ctx context.Context, source string, client *http.Client) (io.ReadCloser, error) {
	source = strings.TrimSpace(source)
	switch scheme, path := splitSource(source); scheme {
	case "", "file":
		if path == "" {
			return nil, fmt.Errorf("%s names a host, or no path: a file URL is file:///absolute/path or file:relative/path", Redacted(source))
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		return file, nil
	case "http", "https":
		return httpGet(ctx, source, client)
	case schemeInvalid:
		return nil, fmt.Errorf("%s is not a valid URL", Redacted(source))
	default:
		return nil, fmt.Errorf("unsupported scheme %q in %s", scheme, Redacted(source))
	}
}

// Redacted returns source with the credentials, query and fragment of a URL
// removed, as a token is likely to be there. A malformed URL is replaced by a
// placeholder. Paths and the file: sources that name one are returned as they are.
func Redacted(source string) string {
	source = strings.TrimSpace(source)
	prefix := schemePattern.FindString(source)
	if prefix == "" && brokenHTTP.MatchString(source) {
		return "(not a valid URL)"
	}
	if prefix == "" {
		return source
	}
	if strings.EqualFold(prefix, "file://") {
		if _, path := splitSource(source); path != "" {
			return source
		}
	}
	scheme := strings.TrimSuffix(prefix, "://")
	parsed, err := url.Parse(source)
	if err != nil {
		return scheme + "://(not a valid URL)"
	}
	if parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" {
		return source
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return parsed.String()
}

// redactedReference is Redacted for a URL that may be relative, which a redirect
// to a path leaves.
func redactedReference(reference string) string {
	parsed, err := url.Parse(reference)
	if err != nil {
		return "(not a valid URL)"
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return parsed.String()
}

// redactedError reports a redacted message and unwraps to the original error.
type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr replaces source in err's message by its redacted form.
func redactErr(err error, source string) error {
	shown := Redacted(source)
	if shown == source {
		return err
	}
	return &redactedError{message: strings.ReplaceAll(err.Error(), source, shown), err: err}
}

// ReadSource reads a whole source, as DownloadFile opens it. A positive limit
// caps the size; 0 is no limit. With strict, plain HTTP and redirects from HTTPS
// to HTTP are refused. Errors never contain the credentials or query of a URL.
func ReadSource(ctx context.Context, source string, limit int64, strict bool) ([]byte, error) {
	source = strings.TrimSpace(source)
	client := lenientClient
	if strict {
		if isPlainHTTP(source) {
			return nil, fmt.Errorf("%s %w", Redacted(source), ErrPlainHTTP)
		}
		client = strictClient
	}
	reader, err := open(ctx, source, client)
	if err != nil {
		return nil, redactErr(err, source)
	}
	defer reader.Close()
	data, err := ReadAllLimited(reader, limit, Redacted(source))
	if err != nil {
		return nil, redactErr(err, source)
	}
	return data, nil
}

// ErrTooLarge is returned by ReadAllLimited when the reader exceeds the limit.
var ErrTooLarge = errors.New("too large")

// ReadAllLimited reads all of reader, failing with ErrTooLarge when it holds
// more than limit bytes; 0 is no limit. what names the source in errors.
func ReadAllLimited(reader io.Reader, limit int64, what string) ([]byte, error) {
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is more than %d bytes: %w", what, limit, ErrTooLarge)
	}
	return data, nil
}

func isPlainHTTP(source string) bool {
	scheme, _ := splitSource(source)
	return scheme == "http"
}

// IsRemote reports whether a source is an HTTP(S) URL.
func IsRemote(source string) bool {
	scheme, _ := splitSource(source)
	return scheme == "http" || scheme == "https"
}

// LocalPath returns the filesystem path of a plain path or file: source, and
// false for anything else.
func LocalPath(source string) (string, bool) {
	scheme, path := splitSource(source)
	return path, (scheme == "" || scheme == "file") && path != ""
}

var schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// brokenHTTP matches an http: or https: source missing its //, which is a typo
// and not a relative path.
var brokenHTTP = regexp.MustCompile(`^(?i)https?:`)

// splitSource classifies a source: the scheme it names, and the filesystem path
// it points at when it names no scheme at all or the "file" one.
//
// A mirror maintained with rsync is a directory, so firmirror reads it where it
// sits instead of asking for a web server in front of it.
func splitSource(source string) (scheme, path string) {
	source = strings.TrimSpace(source)
	// A file: source is a path, so a # or ? in it is not a fragment or query. It is
	// file:///abs/path or relative as in file:path. What follows two slashes is a
	// host, so file://dir/path is refused. Percent escapes are decoded unless
	// invalid, as in file:///tmp/100%.yml.
	if len(source) >= 5 && strings.EqualFold(source[:5], "file:") {
		rest := source[5:]
		if strings.HasPrefix(rest, "//") {
			// Only the local host can serve a file.
			rest = strings.TrimPrefix(rest, "//")
			if strings.HasPrefix(rest, "localhost/") {
				rest = strings.TrimPrefix(rest, "localhost")
			} else if !strings.HasPrefix(rest, "/") {
				return "file", ""
			}
		}
		if decoded, err := url.PathUnescape(rest); err == nil {
			rest = decoded
		}
		return "file", rest
	}
	// A path can hold a colon (a:b.yml) or a "://" past its first element
	// (./dir/a://b); only a leading scheme followed by "://" makes a URL.
	if !schemePattern.MatchString(source) {
		if brokenHTTP.MatchString(source) {
			return schemeInvalid, ""
		}
		return "", source
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" {
		return schemeInvalid, ""
	}
	return parsed.Scheme, parsed.Path
}

func httpGet(ctx context.Context, address string, client *http.Client) (io.ReadCloser, error) {
	var resp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("User-Agent", "firmirror")

		resp, err = client.Do(req)
		if err != nil {
			// The error carries the URL it failed on, including a redirect target, which
			// may be presigned, and the text of a Location it could not parse.
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				urlErr.URL = redactedReference(urlErr.URL)
				// What is not a network error may quote a Location, which is not ours to
				// show: it may be presigned.
				var netErr net.Error
				if !errors.Is(urlErr.Err, errInsecureRedirect) && !errors.As(urlErr.Err, &netErr) && strings.ContainsAny(urlErr.Err.Error(), "?#@") {
					urlErr.Err = errors.New("invalid redirect location")
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errInsecureRedirect) {
				return nil, err
			}
			if attempt < maxRetries {
				if err := Sleep(ctx, time.Duration(attempt+1)*retryBackoff); err != nil {
					return nil, err
				}
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
			if err := Sleep(ctx, time.Duration(attempt+1)*retryBackoff); err != nil {
				return nil, err
			}
			continue
		}

		return nil, fmt.Errorf("%s: unexpected status code: %d", Redacted(address), resp.StatusCode)
	}
	panic("unreachable: the last attempt returns")
}

// Sleep waits for d, or until the context ends.
func Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
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

const temporaryPrefix = ".firmirror-tmp-"

// WriteFileAtomic replaces the file at path with data, all or nothing, through a
// temporary file renamed over it. The data is synced before the rename so a crash
// leaves the old or the new file, never an empty one.
func WriteFileAtomic(path string, mode os.FileMode, data io.Reader) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), temporaryPrefix+"*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(temporary.Name())
	if _, err := io.Copy(temporary, data); err != nil {
		temporary.Close()
		return fmt.Errorf("failed to write data: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("failed to flush temp file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Chmod(temporary.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
