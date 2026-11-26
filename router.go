package main

import (
	"context"
	"errors"
	"io"
	"sort"
)

// errBucketNotConfigured reports a client bucket the config does not declare.
// The gateway serves exactly the configured buckets, so it surfaces to clients
// as NoSuchBucket.
var errBucketNotConfigured = errors.New("bucket not configured")

// bucketRouter dispatches bucket-scoped operations to the overlay store
// configured for that bucket. Every configured bucket therefore has its own
// overlay and baseline backend, credentials and key mapping.
type bucketRouter struct {
	stores map[string]*overlayStore
}

func newBucketRouter(stores map[string]*overlayStore) *bucketRouter {
	return &bucketRouter{stores: stores}
}

func (r *bucketRouter) store(bucket string) (*overlayStore, error) {
	s, ok := r.stores[bucket]
	if !ok {
		return nil, errBucketNotConfigured
	}
	return s, nil
}

// ListBuckets reports the configured buckets: the gateway's bucket set is the
// config, not whatever the backends happen to contain.
func (r *bucketRouter) ListBuckets(context.Context) ([]string, error) {
	names := make([]string, 0, len(r.stores))
	for name := range r.stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (r *bucketRouter) BucketExists(_ context.Context, bucket string) (bool, error) {
	_, ok := r.stores[bucket]
	return ok, nil
}

func (r *bucketRouter) Get(ctx context.Context, bucket, key string, rng *ByteRange) (io.ReadCloser, *ObjectMeta, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, nil, err
	}
	return s.Get(ctx, bucket, key, rng)
}

func (r *bucketRouter) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, err
	}
	return s.Head(ctx, bucket, key)
}

func (r *bucketRouter) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, err
	}
	return s.Put(ctx, bucket, key, body, contentType)
}

func (r *bucketRouter) ListPage(ctx context.Context, bucket string, p ListParams) (ListPage, error) {
	s, err := r.store(bucket)
	if err != nil {
		return ListPage{}, err
	}
	return s.ListPage(ctx, bucket, p)
}

func (r *bucketRouter) CreateBucket(ctx context.Context, bucket string) error {
	s, err := r.store(bucket)
	if err != nil {
		return err
	}
	return s.CreateBucket(ctx, bucket)
}

func (r *bucketRouter) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	s, err := r.store(bucket)
	if err != nil {
		return "", err
	}
	return s.InitiateMultipart(ctx, bucket, key, contentType)
}

func (r *bucketRouter) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	s, err := r.store(bucket)
	if err != nil {
		return "", err
	}
	return s.UploadPart(ctx, bucket, key, uploadID, partNumber, body)
}

func (r *bucketRouter) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, err
	}
	return s.CompleteMultipart(ctx, bucket, key, uploadID, parts)
}

func (r *bucketRouter) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	s, err := r.store(bucket)
	if err != nil {
		return err
	}
	return s.AbortMultipart(ctx, bucket, key, uploadID)
}

func (r *bucketRouter) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, err
	}
	return s.ListParts(ctx, bucket, key, uploadID)
}

func (r *bucketRouter) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	s, err := r.store(bucket)
	if err != nil {
		return nil, err
	}
	return s.ListMultipartUploads(ctx, bucket)
}
