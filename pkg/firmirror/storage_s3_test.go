package firmirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lockObjectServer struct {
	mu     sync.Mutex
	body   []byte
	etag   string
	serial int
}

func (s *lockObjectServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	preconditionFailed := func() {
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(http.StatusPreconditionFailed)
		_, _ = writer.Write([]byte("<Error><Code>PreconditionFailed</Code></Error>"))
	}

	switch request.Method {
	case http.MethodPut:
		if request.Header.Get("If-None-Match") == "*" && s.etag != "" {
			preconditionFailed()
			return
		}
		if match := request.Header.Get("If-Match"); match != "" && match != s.etag {
			preconditionFailed()
			return
		}
		s.serial++
		s.etag = fmt.Sprintf("\"lock-%d\"", s.serial)
		s.body, _ = io.ReadAll(request.Body)
		writer.Header().Set("ETag", s.etag)
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if s.etag == "" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("ETag", s.etag)
		_, _ = writer.Write(s.body)
	case http.MethodDelete:
		if request.Header.Get("If-Match") != s.etag {
			preconditionFailed()
			return
		}
		s.etag = ""
		s.body = nil
		writer.WriteHeader(http.StatusNoContent)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// lockTestStorage opens a storage backend against the fake lock object server.
func lockTestStorage(t *testing.T, backend *lockObjectServer) *S3Storage {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			"test", "test", "test",
		)),
		UsePathStyle: true,
	})
	return &S3Storage{client: client, bucket: "bucket", prefix: "repository"}
}

func TestS3StorageLock(t *testing.T) {
	backend := &lockObjectServer{}
	first := lockTestStorage(t, backend)
	second := lockTestStorage(t, backend)
	ctx := context.Background()

	lock, err := first.Lock(ctx)
	require.NoError(t, err)
	_, err = second.Lock(ctx)
	assert.ErrorIs(t, err, ErrRepositoryLocked)

	require.NoError(t, lock.Release(ctx))
	lock, err = second.Lock(ctx)
	require.NoError(t, err)
	require.NoError(t, lock.Release(ctx))
}

func TestS3StorageLockRefusesAnUnreadableLease(t *testing.T) {
	backend := &lockObjectServer{etag: `"lock-written-by-something-else"`, body: []byte("not a lease")}
	storage := lockTestStorage(t, backend)

	// A lock object that cannot be decoded is not a held lease: reporting it
	// as one would hide both the reason and the object to go and look at,
	// and no expiry could ever take it over.
	_, err := storage.Lock(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRepositoryLocked)
	assert.ErrorContains(t, err, "repository/.firmirror.lock")
}
