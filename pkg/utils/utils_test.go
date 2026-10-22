package utils

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fwrepo.json")
	assert.NoError(t, os.WriteFile(path, []byte("mirrored"), 0644))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("served"))
	}))
	defer server.Close()

	t.Run("HTTP", func(t *testing.T) {
		body, err := DownloadFile(context.Background(), server.URL+"/fwrepo.json")
		assert.NoError(t, err, "An HTTP source should be downloaded")
		defer body.Close()

		content, err := io.ReadAll(body)
		assert.NoError(t, err)
		assert.Equal(t, "served", string(content))
	})

	t.Run("PlainPath", func(t *testing.T) {
		body, err := DownloadFile(context.Background(), path)
		assert.NoError(t, err, "A plain path should be read from the filesystem")
		defer body.Close()

		content, err := io.ReadAll(body)
		assert.NoError(t, err)
		assert.Equal(t, "mirrored", string(content))
	})

	t.Run("FileURL", func(t *testing.T) {
		body, err := DownloadFile(context.Background(), "file://"+path)
		assert.NoError(t, err, "A file:// URL should be read from the filesystem")
		defer body.Close()

		content, err := io.ReadAll(body)
		assert.NoError(t, err)
		assert.Equal(t, "mirrored", string(content))
	})

	t.Run("MissingLocalFile", func(t *testing.T) {
		_, err := DownloadFile(context.Background(), filepath.Join(dir, "absent.json"))
		assert.ErrorIs(t, err, os.ErrNotExist, "A missing local file should report why it could not be opened")
	})

	t.Run("UnsupportedScheme", func(t *testing.T) {
		// rsync is how the mirror is fetched, not how it is read back.
		_, err := DownloadFile(context.Background(), "rsync://rsync.linux.hpe.com/SDR/fwrepo.json")
		assert.ErrorContains(t, err, `unsupported scheme "rsync"`, "An unsupported scheme should fail without retrying")
	})
}

func TestDownloadFileToDest(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "firmware.fwpkg")
	assert.NoError(t, os.WriteFile(source, []byte("firmware"), 0644))

	dest := filepath.Join(t.TempDir(), "firmware.fwpkg")
	assert.NoError(t, DownloadFileToDest(context.Background(), source, dest))

	content, err := os.ReadFile(dest)
	assert.NoError(t, err)
	assert.Equal(t, "firmware", string(content), "A local source should be copied to the destination")

	// The mirror is read-only as far as firmirror is concerned: the copy must
	// not have replaced the source with a link to itself.
	info, err := os.Lstat(source)
	assert.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "The source should still be a regular file")
}

func TestIsRemoteAndLocalPath(t *testing.T) {
	assert.True(t, IsRemote("https://example.com/a.yml"))
	assert.False(t, IsRemote("file:///etc/a.yml"))
	assert.False(t, IsRemote("a.yml"))

	path, local := LocalPath("file:///etc/a.yml")
	assert.True(t, local)
	assert.Equal(t, "/etc/a.yml", path)
	path, local = LocalPath("rings/a.yml")
	assert.True(t, local)
	assert.Equal(t, "rings/a.yml", path)
	_, local = LocalPath("https://example.com/a.yml")
	assert.False(t, local)
}

func TestReadSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o644))

	data, err := ReadSource(context.Background(), path, 0, false)
	require.NoError(t, err)
	assert.Equal(t, "content", string(data))

	_, err = ReadSource(context.Background(), path+".missing", 0, false)
	assert.Error(t, err)
}

func TestReadSourceHasALimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o644))

	data, err := ReadSource(context.Background(), path, 10, false)
	require.NoError(t, err)
	assert.Len(t, data, 10)
	_, err = ReadSource(context.Background(), path, 9, false)
	assert.ErrorContains(t, err, "more than 9 bytes")
}

func TestLocalPathReadsEveryFormOfAFileURL(t *testing.T) {
	for source, want := range map[string]string{
		"file:///etc/a.yml":          "/etc/a.yml",
		"file:a.yml":                 "a.yml",
		"FILE:a.yml":                 "a.yml",
		"file:///tmp/a#b.yml":        "/tmp/a#b.yml",
		"file:///tmp/100%.yml":       "/tmp/100%.yml",
		"file://localhost/etc/a.yml": "/etc/a.yml",
		"rings:v1.yml":               "rings:v1.yml",
		"a.yml":                      "a.yml",
	} {
		path, local := LocalPath(source)
		assert.True(t, local, source)
		assert.Equal(t, want, path, source)
	}
	for _, source := range []string{"file://", "file://dir/a.yml", "file://fileserver/share/a.yml"} {
		_, local := LocalPath(source)
		assert.False(t, local, "%s names a host, or no path, and is not read as a file of the working directory", source)
	}
	_, err := DownloadFile(context.Background(), "file://fileserver/share/a.yml")
	assert.ErrorContains(t, err, "names a host")
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	require.NoError(t, WriteFileAtomic(path, 0o644, strings.NewReader("new")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left")
}

func TestRedactedSaysNoCredentialsAndNoToken(t *testing.T) {
	assert.Equal(t, "https://host/a.yml", Redacted("https://user:pw@host/a.yml?token=x#f"))
	assert.Equal(t, "dir/a.yml", Redacted("dir/a.yml"))
}

func TestReadSourceErrorsHoldNoCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("0123456789")) }))
	defer server.Close()
	saved := httpTransport
	t.Cleanup(func() { httpTransport = saved })
	httpTransport = server.Client().Transport
	source := strings.Replace(server.URL, "https://", "https://user:pw@", 1) + "/a.yml?token=secret"

	_, err := ReadSource(context.Background(), source, 5, true)

	require.ErrorIs(t, err, ErrTooLarge, "the body is read, which is the path under test")
	assert.NotContains(t, err.Error(), "secret")
	assert.NotContains(t, err.Error(), "pw")
}

func TestARedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("clear")) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) }))
	defer secure.Close()
	saved := httpTransport
	t.Cleanup(func() { httpTransport = saved })
	httpTransport = secure.Client().Transport

	_, err := ReadSource(context.Background(), secure.URL, 0, true)

	assert.ErrorContains(t, err, "HTTPS to plain HTTP")
	assert.NotContains(t, err.Error(), "attempts", "asking again would get the same answer")
}

func TestAHTTPErrorSaysNoTokenInAnyFormOfTheURL(t *testing.T) {
	saved := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = saved })
	for _, source := range []string{
		"https://user:pw@127.0.0.1:1/x?token=SECRET",
		"HTTPS://127.0.0.1:1/x?token=SECRET",
		"https://127.0.0.1:1/a b?token=SECRET",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := ReadSource(ctx, source, 0, true)
		cancel()
		require.Error(t, err, source)
		assert.NotContains(t, err.Error(), "SECRET", source)
		assert.NotContains(t, err.Error(), "pw", source)
	}
}

func TestRedactedHidesWhatAFileURLNamingAHostHolds(t *testing.T) {
	assert.Equal(t, "file://host/a.yml", Redacted("file://user:pw@host/a.yml?token=x"))
	assert.Equal(t, "file:///tmp/a?b.yml", Redacted("file:///tmp/a?b.yml"), "a file URL naming a path is that path")
}

func TestARefusedRedirectToPlainHTTPKeepsItsReasonWhateverThePath(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("clear")) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/@user/rings.yml?x=1", http.StatusFound)
	}))
	defer secure.Close()
	saved := httpTransport
	t.Cleanup(func() { httpTransport = saved })
	httpTransport = secure.Client().Transport

	_, err := ReadSource(context.Background(), secure.URL, 0, true)

	assert.ErrorContains(t, err, "HTTPS to plain HTTP")
}

func TestARedirectToARelativeOrMalformedLocationSaysNoToken(t *testing.T) {
	saved := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = saved })
	for name, handler := range map[string]http.HandlerFunc{
		"relative loop": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", r.URL.RequestURI())
			w.WriteHeader(http.StatusFound)
		},
		"malformed": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "%zz?token=SECRET")
			w.WriteHeader(http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()

			_, err := DownloadFile(context.Background(), server.URL+"/a.yml?token=SECRET")

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "SECRET")
		})
	}
}

func TestARedirectDoesNotSendTheURLItCameFromToTheNextHost(t *testing.T) {
	var referer string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer = r.Referer()
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()

	_, err := ReadSource(context.Background(), origin.URL+"/a.yml?token=SECRET", 0, false)

	require.NoError(t, err)
	assert.Empty(t, referer)
}

func TestRedactedLeavesAPathAlone(t *testing.T) {
	assert.Equal(t, "/tmp/a?b#c", Redacted("/tmp/a?b#c"))
}

func TestOnlyAStrictReadRefusesARedirectToPlainHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("clear")) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) }))
	defer secure.Close()
	saved := httpTransport
	t.Cleanup(func() { httpTransport = saved })
	httpTransport = secure.Client().Transport

	data, err := ReadSource(context.Background(), secure.URL, 0, false)

	require.NoError(t, err, "what the vendors serve is read as it always was")
	assert.Equal(t, "clear", string(data))
}

func TestAnUnparsableOrUnsupportedURLSaysNoCredentials(t *testing.T) {
	for _, source := range []string{"https://user:pw@host/%zz?token=abc", "ftp://user:pw@host/x?token=abc"} {
		_, err := ReadSource(context.Background(), source, 0, true)

		require.Error(t, err, source)
		assert.NotContains(t, err.Error(), "pw", source)
		assert.NotContains(t, err.Error(), "abc", source)
	}
}

func TestAStrictReadRefusesPlainHTTPAltogether(t *testing.T) {
	_, err := ReadSource(context.Background(), "http://127.0.0.1:1/a.yml", 0, true)

	assert.ErrorIs(t, err, ErrPlainHTTP)
}

func TestAPathWithASlashSlashPastItsStartIsAPath(t *testing.T) {
	path, local := LocalPath("./dir/a://b")

	assert.True(t, local)
	assert.Equal(t, "./dir/a://b", path)
	assert.Equal(t, "./dir/a://b", Redacted("./dir/a://b"))
}

func TestARetryDoesNotOutlastItsContext(t *testing.T) {
	saved := retryBackoff
	retryBackoff = time.Minute
	t.Cleanup(func() { retryBackoff = saved })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ReadSource(ctx, server.URL, 0, false)

	assert.Error(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "a minute of backoff is not slept through")
}

func TestAHTTPURLMissingASlashIsInvalidAndNotAPath(t *testing.T) {
	_, local := LocalPath("https:/example.com/bkc.yml")
	assert.False(t, local)
	_, err := ReadSource(context.Background(), "http:/example.com/bkc.yml", 0, true)
	assert.ErrorContains(t, err, "not a valid URL")
}

func TestASourceWithWhitespaceAroundItIsStillAURL(t *testing.T) {
	saved := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = saved })
	_, err := ReadSource(context.Background(), " \thttps://user:pw@127.0.0.1:1/x?token=SECRET\n", 0, true)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
	assert.NotContains(t, err.Error(), "pw")
	assert.Equal(t, "https://127.0.0.1:1/x", Redacted(" https://user:pw@127.0.0.1:1/x?token=SECRET "))
}

func TestARealSchemeNamedLikeTheInvalidOneIsJustUnsupported(t *testing.T) {
	_, err := DownloadFile(context.Background(), "invalid://host/x")

	assert.ErrorContains(t, err, "unsupported scheme")
}
