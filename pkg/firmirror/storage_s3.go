package firmirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

const (
	repositoryLeaseDuration = 30 * time.Minute
	repositoryRenewInterval = 5 * time.Minute
)

type repositoryLease struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

// S3Storage implements Storage interface for AWS S3 or S3-compatible storage
type S3Storage struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string // optional prefix for all keys
}

func conditionalRequestFailed(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "PreconditionFailed" || apiErr.ErrorCode() == "ConditionalRequestConflict"
}

// conditionalWritesUnsupported reports whether the endpoint turned down the
// precondition itself rather than the state it guards. The lock is built on
// conditional writes, so an S3-compatible endpoint without them cannot provide
// one at all, which is worth saying plainly instead of failing every command
// with a bare NotImplemented.
func conditionalWritesUnsupported(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "NotImplemented" || apiErr.ErrorCode() == "MethodNotAllowed"
}

// objectNotFound tells an object that is not there from one that cannot be
// read.
func objectNotFound(err error) bool {
	var notFound *types.NotFound
	var noSuchKey *types.NoSuchKey
	return errors.As(err, &notFound) || errors.As(err, &noSuchKey)
}

func encodeRepositoryLease(holder string) []byte {
	body, _ := json.Marshal(repositoryLease{
		Holder:    holder,
		ExpiresAt: time.Now().Add(repositoryLeaseDuration).UTC(),
	})
	return body
}

func (s *S3Storage) putRepositoryLease(ctx context.Context, holder string, ifMatch, ifNoneMatch *string) (*string, error) {
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.buildKey(repositoryLockKey)),
		Body:        bytes.NewReader(encodeRepositoryLease(holder)),
		ContentType: aws.String("application/json"),
		IfMatch:     ifMatch,
		IfNoneMatch: ifNoneMatch,
	})
	if err != nil {
		return nil, err
	}
	return output.ETag, nil
}

func (s *S3Storage) readRepositoryLease(ctx context.Context) (repositoryLease, *string, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.buildKey(repositoryLockKey)),
	})
	if err != nil {
		return repositoryLease{}, nil, err
	}
	defer output.Body.Close()

	var lease repositoryLease
	if err := json.NewDecoder(output.Body).Decode(&lease); err != nil {
		return repositoryLease{}, nil, fmt.Errorf("decoding repository lock: %w", err)
	}
	return lease, output.ETag, nil
}

// Lock creates a renewable lease object with conditional S3 writes. The ETag
// preconditions make acquisition, renewal and release safe across processes;
// an abandoned lease can be replaced after its expiry.
func (s *S3Storage) Lock(ctx context.Context) (*RepositoryLock, error) {
	holder := uuid.NewString()
	lockKey := s.buildKey(repositoryLockKey)
	var etag *string

	for range 3 {
		createdETag, err := s.putRepositoryLease(ctx, holder, nil, aws.String("*"))
		if err == nil {
			etag = createdETag
			break
		}
		if conditionalWritesUnsupported(err) {
			return nil, fmt.Errorf("the endpoint does not support the conditional writes the repository lock is built on: %w (upgrade the endpoint, or pass --no-lock and make sure a single firmirror runs at a time)", err)
		}
		if !conditionalRequestFailed(err) {
			return nil, fmt.Errorf("acquiring repository lock: %w", err)
		}

		lease, existingETag, err := s.readRepositoryLease(ctx)
		if err != nil {
			if objectNotFound(err) {
				// Released between the two calls, so the next attempt
				// creates it again.
				continue
			}
			// Retrying cannot help a lease that is there but unreadable, and
			// reporting it as a held lock would hide both the reason and the
			// object an operator has to deal with.
			return nil, fmt.Errorf("reading repository lock %s: %w", lockKey, err)
		}
		if time.Now().Before(lease.ExpiresAt) {
			return nil, fmt.Errorf("%w (holder %s, expires %s)", ErrRepositoryLocked, lease.Holder, lease.ExpiresAt.Format(time.RFC3339))
		}

		createdETag, err = s.putRepositoryLease(ctx, holder, existingETag, nil)
		if err == nil {
			etag = createdETag
			break
		}
		if !conditionalRequestFailed(err) {
			return nil, fmt.Errorf("replacing expired repository lock: %w", err)
		}
	}
	if etag == nil {
		return nil, fmt.Errorf("%w: %s stayed contended over 3 attempts", ErrRepositoryLocked, lockKey)
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	// Losing the lease must also stop the commit a run makes once a signal
	// ended it, which is what the two contexts differ on.
	heldCtx, cancelHeld := context.WithCancel(context.WithoutCancel(ctx))
	cancel := func() {
		cancelHeld()
		cancelWork()
	}
	stopRenewing := make(chan struct{})
	renewed := make(chan struct{})
	var lockState sync.Mutex
	var lostErr error
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(repositoryRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewing:
				return
			case <-ticker.C:
				lockState.Lock()
				currentETag := etag
				lockState.Unlock()
				renewCtx, renewCancel := context.WithTimeout(context.Background(), time.Minute)
				newETag, err := s.putRepositoryLease(renewCtx, holder, currentETag, nil)
				renewCancel()
				if err != nil {
					lockState.Lock()
					lostErr = fmt.Errorf("renewing repository lock: %w", err)
					lockState.Unlock()
					cancel()
					return
				}
				lockState.Lock()
				etag = newETag
				lockState.Unlock()
			}
		}
	}()

	var releaseOnce sync.Once
	var releaseErr error
	release := func(releaseCtx context.Context) error {
		releaseOnce.Do(func() {
			close(stopRenewing)
			<-renewed
			cancel()

			lockState.Lock()
			currentETag := etag
			releaseErr = lostErr
			lockState.Unlock()
			if releaseErr != nil {
				return
			}
			_, err := s.client.DeleteObject(releaseCtx, &s3.DeleteObjectInput{
				Bucket:  aws.String(s.bucket),
				Key:     aws.String(s.buildKey(repositoryLockKey)),
				IfMatch: currentETag,
			})
			if err != nil {
				releaseErr = fmt.Errorf("releasing repository lock: %w", err)
			}
		})
		return releaseErr
	}
	return &RepositoryLock{Work: workCtx, Held: heldCtx, release: release}, nil
}

func NewS3Storage(ctx context.Context, bucket, prefix, region, endpoint string) (*S3Storage, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}

	var opts []func(*config.LoadOptions) error

	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}
	})

	// Verify bucket exists
	if _, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return nil, fmt.Errorf("failed to access bucket %s: %w", bucket, err)
	}

	return &S3Storage{
		client:   client,
		uploader: manager.NewUploader(client),
		bucket:   bucket,
		prefix:   prefix,
	}, nil
}

// buildKey constructs the full S3 key with optional prefix
func (s *S3Storage) buildKey(key string) string {
	if s.prefix != "" {
		return s.prefix + "/" + key
	}
	return key
}

// Write stores data with the given key to S3
func (s *S3Storage) Write(ctx context.Context, key string, data io.Reader) error {
	fullKey := s.buildKey(key)

	// Read data into buffer to determine size (needed for some S3-compatible services)
	// This also allows us to retry in case of transient errors
	buf, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}

	const maxRetries = 3
	for attempt := range maxRetries {
		_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(fullKey),
			Body:   bytes.NewReader(buf),
		})
		if err == nil {
			return nil
		}
		if attempt < maxRetries-1 && ctx.Err() == nil {
			continue
		}
	}

	return fmt.Errorf("failed to upload to S3 after %d attempts: %w", maxRetries, err)
}

// Delete removes data with the given key from S3.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	fullKey := s.buildKey(key)
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
	}); err != nil {
		return fmt.Errorf("failed to delete object from S3: %w", err)
	}
	return nil
}

// Read retrieves data for the given key from S3
func (s *S3Storage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	fullKey := s.buildKey(key)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download from S3: %w", err)
	}

	return resp.Body, nil
}

// Exists checks if a key exists in S3
func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	fullKey := s.buildKey(key)

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		// Check if it's a not found error
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
}

// List returns all keys with the given prefix (useful for debugging and management)
func (s *S3Storage) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	fullPrefix := s.buildKey(prefix)

	var objects []StoredObject
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Key != nil {
				// Remove the storage prefix from the returned keys
				key := *obj.Key
				if s.prefix != "" && len(key) > len(s.prefix)+1 {
					key = key[len(s.prefix)+1:]
				}
				object := StoredObject{Key: key}
				if obj.LastModified != nil {
					object.ModifiedAt = *obj.LastModified
				}
				objects = append(objects, object)
			}
		}
	}

	return objects, nil
}
