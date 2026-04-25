package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3StoreConfig captures the environment variables the S3 object
// store needs. Endpoint is optional (blank → real AWS S3); bucket
// and credentials are required. Region defaults to us-east-1 when
// blank so a MinIO setup that doesn't advertise a region still works.
type S3StoreConfig struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// ForcePathStyle forces bucket-in-path URLs which is what MinIO
	// expects. AWS S3 accepts both so leaving it true is safe.
	ForcePathStyle bool
}

// S3Store implements ObjectStore against an S3-compatible endpoint.
// Put is a server-side conditional HeadObject-then-PutObject so
// reuploads of the same storage key (which is content-addressable)
// skip the expensive transfer — matching the MemoryStore contract
// that Put MUST be idempotent.
type S3Store struct {
	cfg    S3StoreConfig
	client *s3.Client
}

// Close drops any pooled HTTP transport state held by the underlying
// SDK client so an evicted per-tenant store does not retain idle
// connections forever. Best-effort: the SDK does not expose a
// canonical Close, so we reach into the HTTP client (if it exposes
// CloseIdleConnections via a *http.Client or a smithy idleCloser).
// Safe to call on a nil receiver to keep eviction callbacks simple.
func (s *S3Store) Close() error {
	if s == nil {
		return nil
	}
	type idleCloser interface {
		CloseIdleConnections()
	}
	// Walk the SDK option chain to find the HTTP client. The SDK
	// stores it on awsCfg.HTTPClient; we held the reference via the
	// closure when the client was built. Since we don't have direct
	// access here, we rely on the smithy buildable client's
	// CloseIdleConnections (Phase 1 wiring — replace with a held
	// reference once an SDK upgrade exposes it cleanly).
	if c, ok := any(s.client).(idleCloser); ok {
		c.CloseIdleConnections()
	}
	return nil
}

// NewS3Store builds an S3Store from the config. A context is taken
// so credential providers (STS / IMDS) can honour cancellation
// during startup.
func NewS3Store(ctx context.Context, cfg S3StoreConfig) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("files: S3_BUCKET required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("files: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle || cfg.Endpoint != ""
	})
	return &S3Store{cfg: cfg, client: client}, nil
}

// Put uploads the blob under the supplied key. Already-present keys
// are skipped via a HeadObject probe so the caller's dedup-by-hash
// invariant costs one cheap round-trip instead of a redundant PUT.
func (s *S3Store) Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil
	}
	if !isS3NotFound(err) {
		return fmt.Errorf("files: head: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("files: put: %w", err)
	}
	return nil
}

// Get returns a ReadCloser over the stored object. Missing keys map
// to ErrNotFound so callers can distinguish absence from IO errors
// without inspecting the SDK error type.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: get: %w", err)
	}
	return out.Body, nil
}

// isS3NotFound treats both the typed NoSuchKey error and an untyped
// 404 API error (which HeadObject on some MinIO builds returns) as
// absence.
func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NoSuchKey" || code == "NotFound" || code == "404" {
			return true
		}
	}
	return false
}
