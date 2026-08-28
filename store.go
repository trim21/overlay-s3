package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrNotFound is returned when an object or bucket does not exist.
var ErrNotFound = errors.New("not found")

// ObjectMeta describes a stored object.
type ObjectMeta struct {
	Key          string
	Size         int64  // bytes the returned body carries: the whole object, or a range slice
	ETag         string // hex MD5, without quotes
	LastModified time.Time
	ContentType  string
}

// ByteRange selects a slice of an object: Length bytes starting at Start.
type ByteRange struct {
	Start  int64
	Length int64
}

// header renders the range in the HTTP Range syntax S3 backends accept.
func (b *ByteRange) header() string {
	return fmt.Sprintf("bytes=%d-%d", b.Start, b.Start+b.Length-1)
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

// ListParams describes one page of a paginated listing.
type ListParams struct {
	Prefix    string
	Delimiter string
	After     string // resume position (opaque); empty starts from the beginning
	MaxKeys   int    // max entries (objects + common prefixes) per page
}

// ListPage is one page of a paginated listing in key order.
type ListPage struct {
	Objects        []ObjectMeta
	CommonPrefixes []string
	Truncated      bool
	NextToken      string // opaque resume position; meaningful when Truncated
}

// Store is the minimal object-store surface the overlay composes.
type Store interface {
	// Get returns an object body. With rng set the body carries exactly that
	// slice and meta.Size is rng.Length: a store whose backend ignores the
	// range must slice the response itself.
	Get(ctx context.Context, bucket, key string, rng *ByteRange) (io.ReadCloser, *ObjectMeta, error)
	Head(ctx context.Context, bucket, key string) (*ObjectMeta, error)
	Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error)
	ListPage(ctx context.Context, bucket string, p ListParams) (ListPage, error)
	ListBuckets(ctx context.Context) ([]string, error)
	BucketExists(ctx context.Context, bucket string) (bool, error)
	CreateBucket(ctx context.Context, bucket string) error

	InitiateMultipart(ctx context.Context, bucket, key, contentType string) (uploadID string, err error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (etag string, err error)
	CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error)
	AbortMultipart(ctx context.Context, bucket, key, uploadID string) error
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error)
}
