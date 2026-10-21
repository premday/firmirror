package firmirror

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/premday/firmirror/pkg/utils"
)

// staleTemporaryAge is how old a temporary file is before no write can be using it.
const staleTemporaryAge = 24 * time.Hour

// LocalStorage implements Storage interface for local filesystem
type LocalStorage struct {
	basePath string
}

// NewLocalStorage creates a new LocalStorage instance
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	// Create base directory if it doesn't exist
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}
	return &LocalStorage{basePath: basePath}, nil
}

// RemoveStaleTemporaries removes the temporary files a write that was killed left
// in the storage, which nothing else removes. Only the process that writes to the
// storage calls it, as opening one to read must not change it.
func (s *LocalStorage) RemoveStaleTemporaries() {
	if removed := utils.RemoveStaleTemporaries(s.basePath, staleTemporaryAge); removed > 0 {
		slog.Warn("Removed temporary files left by an interrupted write", "path", s.basePath, "files", removed)
	}
}

// Write stores data with the given key to the filesystem
func (s *LocalStorage) Write(ctx context.Context, key string, data io.Reader) error {
	fullPath := filepath.Join(s.basePath, key)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// What is stored is published, and read by whatever serves the directory. A
	// crash must not leave empty a package or the metadata that refers to it.
	return utils.WriteFileAtomic(fullPath, 0o644, data)
}

// Read retrieves data for the given key from the filesystem
func (s *LocalStorage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	fullPath := filepath.Join(s.basePath, key)
	file, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	return file, nil
}

// Exists checks if a key exists in the filesystem
func (s *LocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	fullPath := filepath.Join(s.basePath, key)
	_, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to stat file: %w", err)
	}
	return true, nil
}
