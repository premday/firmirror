package firmirror

import (
	"context"
	"io"
)

// Interface for different storage backends
type Storage interface {
	// Write stores data with the given key
	Write(ctx context.Context, key string, data io.Reader) error

	// Read retrieves data for the given key
	Read(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists checks if a key exists
	Exists(ctx context.Context, key string) (bool, error)
}

// packageCleaner is implemented by storage backends that support removing
// unreferenced firmware packages. Local storage deliberately does not
// implement this interface so local repositories retain old packages.
type packageCleaner interface {
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}
