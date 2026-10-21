package firmirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
