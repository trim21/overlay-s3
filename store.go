package main

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when an object or bucket does not exist.
var ErrNotFound = errors.New("not found")

// ObjectMeta describes a stored object.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string // hex MD5, without quotes
	LastModified time.Time
	ContentType  string
}

// CompletedPart names one part in a CompleteMultipartUpload request.
type CompletedPart struct {
	PartNumber int32
	ETag       string // hex MD5, without quotes
}

// PartInfo describes one uploaded part.
type PartInfo struct {
	PartNumber   int32
	ETag         string
	Size         int64
	LastModified time.Time
}

// MultipartInfo describes an in-progress multipart upload.
type MultipartInfo struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

var (
	// errUploadNotFound is returned for operations on an unknown upload ID.
	errUploadNotFound = errors.New("multipart upload not found")
	// errInvalidPart is returned when CompleteMultipartUpload names parts
	// that were not staged or carry a mismatched ETag.
	errInvalidPart = errors.New("invalid part")
)

// Store is the minimal object-store surface the overlay composes. Methods
// taking a size expect the payload length in bytes, or -1 when unknown;
// stores must then buffer the body themselves to derive a length.
type Store interface {
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error)
	Head(ctx context.Context, bucket, key string) (*ObjectMeta, error)
	Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (*ObjectMeta, error)
	List(ctx context.Context, bucket string) ([]ObjectMeta, error)
	ListBuckets(ctx context.Context) ([]string, error)
	BucketExists(ctx context.Context, bucket string) (bool, error)
	CreateBucket(ctx context.Context, bucket string) error

	InitiateMultipart(ctx context.Context, bucket, key, contentType string) (uploadID string, err error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader, size int64) (etag string, err error)
	CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error)
	AbortMultipart(ctx context.Context, bucket, key, uploadID string) error
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error)
}
