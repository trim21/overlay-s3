package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory Store used to stand in for the remote backend.
type memStore struct {
	mu         sync.Mutex
	buckets    map[string]map[string]memObj
	multiparts map[string]*memMultipart
}

type memObj struct {
	data        []byte
	contentType string
	lm          time.Time
}

type memMultipart struct {
	key         string
	contentType string
	initiated   time.Time
	parts       map[int32][]byte
}

func newMemStore() *memStore {
	return &memStore{buckets: map[string]map[string]memObj{}}
}

func (m *memStore) obj(bucket, key string) (memObj, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[bucket]
	if !ok {
		return memObj{}, false
	}
	o, ok := b[key]
	return o, ok
}

func (m *memStore) put(bucket, key string, data []byte, contentType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[bucket] == nil {
		m.buckets[bucket] = map[string]memObj{}
	}
	m.buckets[bucket][key] = memObj{
		data:        append([]byte(nil), data...),
		contentType: contentType,
		lm:          time.Now().UTC(),
	}
}

func etagOf(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}

func (m *memStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	o, ok := m.obj(bucket, key)
	if !ok {
		return nil, nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), &ObjectMeta{
		Key:          key,
		Size:         int64(len(o.data)),
		ETag:         etagOf(o.data),
		LastModified: o.lm,
		ContentType:  o.contentType,
	}, nil
}

func (m *memStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	o, ok := m.obj(bucket, key)
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectMeta{
		Key:          key,
		Size:         int64(len(o.data)),
		ETag:         etagOf(o.data),
		LastModified: o.lm,
		ContentType:  o.contentType,
	}, nil
}

func (m *memStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	m.put(bucket, key, data, contentType)
	return &ObjectMeta{
		Key:          key,
		Size:         int64(len(data)),
		ETag:         etagOf(data),
		LastModified: time.Now().UTC(),
		ContentType:  contentType,
	}, nil
}

func (m *memStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[bucket]
	if !ok {
		return nil, nil
	}
	var out []ObjectMeta
	for key, o := range b {
		out = append(out, ObjectMeta{
			Key:          key,
			Size:         int64(len(o.data)),
			ETag:         etagOf(o.data),
			LastModified: o.lm,
			ContentType:  o.contentType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStore) ListBuckets(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for b := range m.buckets {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (m *memStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.buckets[bucket]
	return ok, nil
}

func (m *memStore) CreateBucket(ctx context.Context, bucket string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[bucket] == nil {
		m.buckets[bucket] = map[string]memObj{}
	}
	return nil
}

func (m *memStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	uploadID, err := newUploadID()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.multiparts == nil {
		m.multiparts = map[string]*memMultipart{}
	}
	m.multiparts[uploadID] = &memMultipart{
		key:         key,
		contentType: contentType,
		initiated:   time.Now().UTC(),
		parts:       map[int32][]byte{},
	}
	return uploadID, nil
}

func (m *memStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	m.mu.Lock()
	mp, ok := m.multiparts[uploadID]
	m.mu.Unlock()
	if !ok {
		return "", errUploadNotFound
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	mp.parts[partNumber] = data
	m.mu.Unlock()
	return etagOf(data), nil
}

func (m *memStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	m.mu.Lock()
	mp, ok := m.multiparts[uploadID]
	if !ok {
		m.mu.Unlock()
		return nil, errUploadNotFound
	}
	complete := make([]CompletedPart, 0, len(parts))
	for _, p := range parts {
		data, ok := mp.parts[p.PartNumber]
		if !ok || etagOf(data) != strings.Trim(p.ETag, `"`) {
			m.mu.Unlock()
			return nil, errInvalidPart
		}
		complete = append(complete, p)
	}
	if len(complete) != len(mp.parts) {
		m.mu.Unlock()
		return nil, errInvalidPart
	}
	sort.Slice(complete, func(i, j int) bool { return complete[i].PartNumber < complete[j].PartNumber })
	var data []byte
	var concat []byte
	for _, p := range complete {
		part := mp.parts[p.PartNumber]
		data = append(data, part...)
		b, _ := hex.DecodeString(etagOf(part))
		concat = append(concat, b...)
	}
	sum := md5.Sum(concat)
	etag := hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(complete))
	delete(m.multiparts, uploadID)
	if m.buckets[bucket] == nil {
		m.buckets[bucket] = map[string]memObj{}
	}
	m.buckets[bucket][key] = memObj{data: data, contentType: mp.contentType, lm: time.Now().UTC()}
	m.mu.Unlock()
	return &ObjectMeta{
		Key:          key,
		Size:         int64(len(data)),
		ETag:         etag,
		LastModified: time.Now().UTC(),
		ContentType:  mp.contentType,
	}, nil
}

func (m *memStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.multiparts[uploadID]; !ok {
		return errUploadNotFound
	}
	delete(m.multiparts, uploadID)
	return nil
}

func (m *memStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	m.mu.Lock()
	mp, ok := m.multiparts[uploadID]
	if !ok {
		m.mu.Unlock()
		return nil, errUploadNotFound
	}
	var out []PartInfo
	for n, data := range mp.parts {
		out = append(out, PartInfo{
			PartNumber:   n,
			ETag:         etagOf(data),
			Size:         int64(len(data)),
			LastModified: mp.initiated,
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (m *memStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MultipartInfo
	for id, mp := range m.multiparts {
		out = append(out, MultipartInfo{Key: mp.key, UploadID: id, Initiated: mp.initiated})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].UploadID < out[j].UploadID
	})
	return out, nil
}

func newTestOverlay(t *testing.T) (*overlayStore, *localStore, *memStore) {
	t.Helper()
	local, err := newLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote := newMemStore()
	return newOverlayStore(local, remote), local, remote
}

func TestOverlayGetPrefersLocal(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	remote.put("bucket", "a", []byte("remote-a"), "text/plain")
	remote.put("bucket", "c", []byte("remote-c"), "text/plain")

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
	if _, ok := remote.obj("bucket", "k"); ok {
		t.Fatal("put must not touch the remote store")
	}
}

func TestOverlayListMerges(t *testing.T) {
	ov, _, remote := newTestOverlay(t)
	remote.put("bucket", "a", []byte("remote-a"), "text/plain")
	remote.put("bucket", "c", []byte("remote-c"), "text/plain")
	remote.put("bucket", "d", []byte("remote-d"), "text/plain")

	if _, err := ov.Put(context.Background(), "bucket", "b", strings.NewReader("local-b"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.Put(context.Background(), "bucket", "a", strings.NewReader("local-a"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	objs, err := ov.List(context.Background(), "bucket")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "a,b,c,d" {
		t.Fatalf("unexpected merge order: %v", keys)
	}
	if objs[0].ETag != etagOf([]byte("local-a")) {
		t.Fatalf("local etag should win for key a, got %s", objs[0].ETag)
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
	if strings.Join(buckets, ",") != "local-bucket,remote-bucket" {
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
