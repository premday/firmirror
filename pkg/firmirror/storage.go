package firmirror

import (
	"context"
	"errors"
	"io"
	"time"
)

const repositoryLockKey = ".firmirror.lock"

var ErrRepositoryLocked = errors.New("another firmirror operation holds the repository lock")

// UnlockRepository releases a lock acquired through Storage.Lock.
type UnlockRepository func(context.Context) error

type repositoryLocker interface {
	Lock(ctx context.Context) (*RepositoryLock, error)
}

// RepositoryLock is a held repository lock, and the two contexts an operation
// running under it needs.
type RepositoryLock struct {
	// Work is canceled when the process is asked to stop, and when the lock
	// is lost.
	Work context.Context

	// Held is canceled only when the lock is lost. The metadata a run owes
	// its repository is committed after the signal that ended the run, so
	// that commit must outlive Work; it must not outlive the lock, because
	// from then on another process may be writing the same documents.
	Held context.Context

	release UnlockRepository
}

// Release gives the lock back. It is safe to call on a lock that was never
// taken, so a command can defer it unconditionally.
func (l *RepositoryLock) Release(ctx context.Context) error {
	if l == nil || l.release == nil {
		return nil
	}
	return l.release(ctx)
}

// Interface for different storage backends
type Storage interface {
	// Write stores data with the given key
	Write(ctx context.Context, key string, data io.Reader) error

	// Read retrieves data for the given key
	Read(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists checks if a key exists
	Exists(ctx context.Context, key string) (bool, error)
}

// LockRepository serializes an operation that can change repository state.
func (f *FirmirrorSyncer) LockRepository(ctx context.Context) (*RepositoryLock, error) {
	locker, ok := f.Storage.(repositoryLocker)
	if !ok {
		return nil, errors.New("the storage backend does not support repository locking")
	}
	return locker.Lock(ctx)
}

// UnlockedRepository runs an operation without taking any lock, for a storage
// endpoint that cannot provide one. Nothing then stops a second firmirror from
// writing the same repository at the same time, which is why it is opt-in.
func UnlockedRepository(ctx context.Context) *RepositoryLock {
	return &RepositoryLock{Work: ctx, Held: context.WithoutCancel(ctx)}
}

// StoredObject is one object in a storage backend. The modification time is
// what tells a package that nothing references yet from one that nothing
// references any more: a refresh uploads every cabinet as it builds it and
// only publishes the metadata naming them when the run ends.
type StoredObject struct {
	Key        string
	ModifiedAt time.Time
}

// lister is implemented by storage backends that can enumerate their objects.
// Ring snapshots are discovered rather than configured, and the packages a
// repository must keep are checked against what is actually stored, so both
// backends implement it.
type lister interface {
	List(ctx context.Context, prefix string) ([]StoredObject, error)
}

// packageCleaner is implemented by storage backends that support removing
// unreferenced firmware packages. Local storage deliberately does not
// implement Delete so local repositories retain old packages.
type packageCleaner interface {
	lister
	Delete(ctx context.Context, key string) error
}
