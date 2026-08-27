package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func etagOf(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}

func newTestOverlay(t *testing.T) (*overlayStore, *memStore, *memStore) {
	t.Helper()
	overlay := newMemStore()
	baseline := newMemStore()
	ctx := context.Background()
	for _, m := range []*memStore{overlay, baseline} {
		if err := m.CreateBucket(ctx, "bucket"); err != nil {
			t.Fatal(err)
		}
	}
	return newOverlayStore(overlay, baseline), overlay, baseline
}

func TestOverlayGetPrefersLocal(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	remote.Put(context.Background(), "bucket", "a", strings.NewReader("remote-a"), "text/plain")
	remote.Put(context.Background(), "bucket", "c", strings.NewReader("remote-c"), "text/plain")

	if _, err := ov.Put(context.Background(), "bucket", "b", strings.NewReader("local-b"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.Put(context.Background(), "bucket", "a", strings.NewReader("local-a"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	rc, meta, err := ov.Get(context.Background(), "bucket", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "local-a" {
		t.Fatalf("local should win, got %q", data)
	}
	if meta.ETag != etagOf([]byte("local-a")) {
		t.Fatalf("etag should reflect local content, got %s", meta.ETag)
	}

	rc, meta, err = ov.Get(context.Background(), "bucket", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ = io.ReadAll(rc)
	if string(data) != "remote-c" {
		t.Fatalf("fallback to remote, got %q", data)
	}

	if _, _, err := ov.Get(context.Background(), "bucket", "missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOverlayPutOnlyWritesLocal(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	if _, err := ov.Put(context.Background(), "bucket", "k", strings.NewReader("x"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(context.Background(), "bucket", "k"); err != ErrNotFound {
		t.Fatalf("put must not touch the remote store, head err = %v", err)
	}
}

func TestOverlayHeadFallback(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	ctx := context.Background()
	if _, err := remote.Put(ctx, "bucket", "remote-key", strings.NewReader("remote-data"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.Put(ctx, "bucket", "local-key", strings.NewReader("local-data"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	meta, err := ov.Head(ctx, "bucket", "local-key")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ETag != etagOf([]byte("local-data")) {
		t.Fatalf("local head etag = %q", meta.ETag)
	}

	meta, err = ov.Head(ctx, "bucket", "remote-key")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ETag != etagOf([]byte("remote-data")) {
		t.Fatalf("remote head etag = %q", meta.ETag)
	}

	if _, err := ov.Head(ctx, "bucket", "missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOverlayListMissingBucket(t *testing.T) {
	ov, _, _ := newTestOverlay(t)
	if _, err := ov.ListPage(context.Background(), "no-such-bucket", ListParams{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOverlayListMerges(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	remote.Put(context.Background(), "bucket", "a", strings.NewReader("remote-a"), "text/plain")
	remote.Put(context.Background(), "bucket", "c", strings.NewReader("remote-c"), "text/plain")
	remote.Put(context.Background(), "bucket", "d", strings.NewReader("remote-d"), "text/plain")

	if _, err := ov.Put(context.Background(), "bucket", "b", strings.NewReader("local-b"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.Put(context.Background(), "bucket", "a", strings.NewReader("local-a"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	objs, err := ov.ListPage(context.Background(), "bucket", ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs.Objects {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "a,b,c,d" {
		t.Fatalf("unexpected merge order: %v", keys)
	}
	if objs.Objects[0].ETag != etagOf([]byte("local-a")) {
		t.Fatalf("local etag should win for key a, got %s", objs.Objects[0].ETag)
	}
}

func TestOverlayListBuckets(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	if err := ov.CreateBucket(context.Background(), "local-bucket"); err != nil {
		t.Fatal(err)
	}
	if err := remote.CreateBucket(context.Background(), "remote-bucket"); err != nil {
		t.Fatal(err)
	}
	buckets, err := ov.ListBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(buckets, ",") != "bucket,local-bucket,remote-bucket" {
		t.Fatalf("unexpected buckets: %v", buckets)
	}
}

func TestOverlayBucketExists(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	if err := remote.CreateBucket(context.Background(), "rb"); err != nil {
		t.Fatal(err)
	}
	if ok, err := ov.BucketExists(context.Background(), "rb"); err != nil || !ok {
		t.Fatalf("remote bucket should exist: ok=%v err=%v", ok, err)
	}
	if ok, err := ov.BucketExists(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing bucket: ok=%v err=%v", ok, err)
	}
}

func TestOverlayBucketExistsLocal(t *testing.T) {
	ov, _, _ := newTestOverlay(t)
	if err := ov.CreateBucket(context.Background(), "lb"); err != nil {
		t.Fatal(err)
	}
	if ok, err := ov.BucketExists(context.Background(), "lb"); err != nil || !ok {
		t.Fatalf("local bucket should exist: ok=%v err=%v", ok, err)
	}
}

func TestOverlayMultipartRoundTrip(t *testing.T) {
	ov, _, _ := newTestOverlay(t)
	ctx := context.Background()

	uploadID, err := ov.InitiateMultipart(ctx, "bucket", "big", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	part1 := strings.Repeat("a", 100)
	part2 := strings.Repeat("b", 100)
	e1, err := ov.UploadPart(ctx, "bucket", "big", uploadID, 1, strings.NewReader(part1))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := ov.UploadPart(ctx, "bucket", "big", uploadID, 2, strings.NewReader(part2))
	if err != nil {
		t.Fatal(err)
	}

	parts, err := ov.ListParts(ctx, "bucket", "big", uploadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Fatalf("unexpected parts: %+v", parts)
	}

	if _, err := ov.CompleteMultipart(ctx, "bucket", "big", uploadID, []CompletedPart{
		{PartNumber: 1, ETag: "wrong"},
		{PartNumber: 2, ETag: e2},
	}); !errors.Is(err, errInvalidPart) {
		t.Fatalf("expected InvalidPart, got %v", err)
	}

	meta, err := ov.CompleteMultipart(ctx, "bucket", "big", uploadID, []CompletedPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := hex.DecodeString(etagOf([]byte(part1)))
	b2, _ := hex.DecodeString(etagOf([]byte(part2)))
	concat := append(b1, b2...)
	want := hex.EncodeToString(md5sum(concat)) + "-2"
	if meta.ETag != want {
		t.Fatalf("etag = %q, want %q", meta.ETag, want)
	}

	rc, m, err := ov.Get(ctx, "bucket", "big")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != part1+part2 {
		t.Fatalf("assembled body mismatch: %d bytes", len(data))
	}
	if m.ETag != meta.ETag {
		t.Fatalf("get etag = %q, want %q", m.ETag, meta.ETag)
	}

	uploads, err := ov.ListMultipartUploads(ctx, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 0 {
		t.Fatalf("completed upload still listed: %+v", uploads)
	}
}

func TestOverlayMultipartAbort(t *testing.T) {
	ov, _, _ := newTestOverlay(t)
	ctx := context.Background()

	uploadID, err := ov.InitiateMultipart(ctx, "bucket", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ov.UploadPart(ctx, "bucket", "k", uploadID, 1, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	uploads, err := ov.ListMultipartUploads(ctx, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 {
		t.Fatalf("expected one pending upload, got %d", len(uploads))
	}

	if err := ov.AbortMultipart(ctx, "bucket", "k", uploadID); err != nil {
		t.Fatal(err)
	}
	uploads, _ = ov.ListMultipartUploads(ctx, "bucket")
	if len(uploads) != 0 {
		t.Fatalf("aborted upload still listed: %+v", uploads)
	}
	if _, _, err := ov.Get(ctx, "bucket", "k"); err != ErrNotFound {
		t.Fatalf("aborted upload must not create an object, got %v", err)
	}
	if _, err := ov.CompleteMultipart(ctx, "bucket", "k", uploadID, nil); !errors.Is(err, errUploadNotFound) {
		t.Fatalf("expected NoSuchUpload, got %v", err)
	}
}

func md5sum(b []byte) []byte {
	h := md5.Sum(b)
	return h[:]
}
