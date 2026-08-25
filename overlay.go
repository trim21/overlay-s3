package main

import (
	"context"
	"io"
	"sort"
)

// overlayStore presents a write-coalescing overlay: every write lands on the
// overlay store, reads prefer the overlay store and fall back to the
// baseline, and listings merge the two (overlay keys win on ties).
type overlayStore struct {
	overlay  Store // all writes land here
	baseline Store // reads fall back here; never modified
}

func newOverlayStore(overlay, baseline Store) *overlayStore {
	return &overlayStore{overlay: overlay, baseline: baseline}
}

func (o *overlayStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	rc, meta, err := o.overlay.Get(ctx, bucket, key)
	if err == nil {
		return rc, meta, nil
	}
	if err != ErrNotFound {
		return nil, nil, err
	}
	return o.baseline.Get(ctx, bucket, key)
}

func (o *overlayStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	meta, err := o.overlay.Head(ctx, bucket, key)
	if err == nil {
		return meta, nil
	}
	if err != ErrNotFound {
		return nil, err
	}
	return o.baseline.Head(ctx, bucket, key)
}

func (o *overlayStore) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (*ObjectMeta, error) {
	return o.overlay.Put(ctx, bucket, key, body, size, contentType)
}

func (o *overlayStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	overlay, err := o.overlay.List(ctx, bucket)
	if err != nil {
		return nil, err
	}
	baseline, err := o.baseline.List(ctx, bucket)
	if err != nil && err != ErrNotFound {
		return nil, err
	}
	merged := make(map[string]ObjectMeta, len(overlay)+len(baseline))
	for _, m := range baseline {
		merged[m.Key] = m
	}
	for _, m := range overlay {
		merged[m.Key] = m // overlay wins
	}
	out := make([]ObjectMeta, 0, len(merged))
	for _, m := range merged {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (o *overlayStore) ListBuckets(ctx context.Context) ([]string, error) {
	overlay, err := o.overlay.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	baseline, err := o.baseline.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(overlay)+len(baseline))
	for _, b := range overlay {
		set[b] = true
	}
	for _, b := range baseline {
		set[b] = true
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (o *overlayStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	ok, err := o.overlay.BucketExists(ctx, bucket)
	if err != nil || ok {
		return ok, err
	}
	return o.baseline.BucketExists(ctx, bucket)
}

func (o *overlayStore) CreateBucket(ctx context.Context, bucket string) error {
	return o.overlay.CreateBucket(ctx, bucket)
}

func (o *overlayStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	return o.overlay.InitiateMultipart(ctx, bucket, key, contentType)
}

func (o *overlayStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader, size int64) (string, error) {
	return o.overlay.UploadPart(ctx, bucket, key, uploadID, partNumber, body, size)
}

func (o *overlayStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	return o.overlay.CompleteMultipart(ctx, bucket, key, uploadID, parts)
}

func (o *overlayStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	return o.overlay.AbortMultipart(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	return o.overlay.ListParts(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	return o.overlay.ListMultipartUploads(ctx, bucket)
}
