package main

import (
	"context"
	"io"
	"sort"
)

// overlayStore presents a write-coalescing overlay: every write lands on the
// local store, reads prefer the local store and fall back to the remote, and
// listings merge the two (local keys win on ties).
type overlayStore struct {
	local  Store
	remote Store
}

func newOverlayStore(local, remote Store) *overlayStore {
	return &overlayStore{local: local, remote: remote}
}

func (o *overlayStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	rc, meta, err := o.local.Get(ctx, bucket, key)
	if err == nil {
		return rc, meta, nil
	}
	if err != ErrNotFound {
		return nil, nil, err
	}
	return o.remote.Get(ctx, bucket, key)
}

func (o *overlayStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	meta, err := o.local.Head(ctx, bucket, key)
	if err == nil {
		return meta, nil
	}
	if err != ErrNotFound {
		return nil, err
	}
	return o.remote.Head(ctx, bucket, key)
}

func (o *overlayStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	return o.local.Put(ctx, bucket, key, body, contentType)
}

func (o *overlayStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	local, err := o.local.List(ctx, bucket)
	if err != nil {
		return nil, err
	}
	remote, err := o.remote.List(ctx, bucket)
	if err != nil && err != ErrNotFound {
		return nil, err
	}
	merged := make(map[string]ObjectMeta, len(local)+len(remote))
	for _, m := range remote {
		merged[m.Key] = m
	}
	for _, m := range local {
		merged[m.Key] = m // local wins
	}
	out := make([]ObjectMeta, 0, len(merged))
	for _, m := range merged {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (o *overlayStore) ListBuckets(ctx context.Context) ([]string, error) {
	local, err := o.local.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	remote, err := o.remote.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(local)+len(remote))
	for _, b := range local {
		set[b] = true
	}
	for _, b := range remote {
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
	ok, err := o.local.BucketExists(ctx, bucket)
	if err != nil || ok {
		return ok, err
	}
	return o.remote.BucketExists(ctx, bucket)
}

func (o *overlayStore) CreateBucket(ctx context.Context, bucket string) error {
	return o.local.CreateBucket(ctx, bucket)
}

func (o *overlayStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	return o.local.InitiateMultipart(ctx, bucket, key, contentType)
}

func (o *overlayStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	return o.local.UploadPart(ctx, bucket, key, uploadID, partNumber, body)
}

func (o *overlayStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	return o.local.CompleteMultipart(ctx, bucket, key, uploadID, parts)
}

func (o *overlayStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	return o.local.AbortMultipart(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	return o.local.ListParts(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	return o.local.ListMultipartUploads(ctx, bucket)
}
