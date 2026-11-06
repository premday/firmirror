package firmirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalStorageWritesFilesAnythingServingTheDirectoryCanRead(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewLocalStorage(dir)
	require.NoError(t, err)

	require.NoError(t, storage.Write(context.Background(), "metadata.xml.zst", strings.NewReader("x")))

	info, err := os.Stat(filepath.Join(dir, "metadata.xml.zst"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestRemovingStaleTemporariesIsAskedForNotDoneByOpeningTheStorage(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".firmirror-tmp-123")
	require.NoError(t, os.WriteFile(stale, []byte("half"), 0o600))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	storage, err := NewLocalStorage(dir)
	require.NoError(t, err)
	assert.FileExists(t, stale, "a command that only reads must not change the storage")

	storage.RemoveStaleTemporaries()
	assert.NoFileExists(t, stale)
}
