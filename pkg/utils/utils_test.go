package utils

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
