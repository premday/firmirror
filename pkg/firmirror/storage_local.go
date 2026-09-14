package firmirror

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LocalStorage implements Storage interface for local filesystem
type LocalStorage struct {
	basePath string
}

// Lock takes an advisory lock in the repository itself. flock locks are
// released by the kernel if the process exits, so a killed run cannot leave a
// local repository permanently locked.
func (s *LocalStorage) Lock(ctx context.Context) (*RepositoryLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(s.basePath, repositoryLockKey)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening repository lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrRepositoryLocked
		}
		return nil, fmt.Errorf("locking repository: %w", err)
	}

	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		_ = file.Sync()
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	// The lock cannot be lost while the process runs, so what the commit
	// context has to survive is the signal that ended the run.
	heldCtx, cancelHeld := context.WithCancel(context.WithoutCancel(ctx))
	return &RepositoryLock{Work: workCtx, Held: heldCtx, release: func(context.Context) error {
		cancelWork()
		cancelHeld()
		unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil {
			return fmt.Errorf("unlocking repository: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing repository lock: %w", closeErr)
		}
		return nil
	}}, nil
}

// NewLocalStorage creates a new LocalStorage instance
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	// Create base directory if it doesn't exist
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base path: %w", err)
	}
	return &LocalStorage{basePath: basePath}, nil
}

// Write stores data with the given key to the filesystem
func (s *LocalStorage) Write(ctx context.Context, key string, data io.Reader) error {
	fullPath := filepath.Join(s.basePath, key)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(fullPath), ".firmirror-tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := io.Copy(tmpFile, data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write data: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
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

// List returns the objects stored under the given prefix. Local storage
// supports listing, unlike deleting, so a local repository can still report
// which of its packages the metadata no longer points at without ever
// removing one.
func (s *LocalStorage) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	var objects []StoredObject
	err := filepath.WalkDir(s.basePath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(s.basePath, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		objects = append(objects, StoredObject{Key: key, ModifiedAt: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}
	return objects, nil
}
