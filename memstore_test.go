package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// memStore is an in-memory Store used by unit tests in place of a real S3
// backend. It keeps the same observable semantics as s3Store: opaque upload
// IDs, InvalidPart validation on completion, and multipart ETags of the form
// md5(concat(part-md5s))-N.
type memStore struct {
	mu      sync.Mutex
	buckets map[string]bool
	objects map[string]map[string]*memObject
	uploads map[string]*memUpload
	seq     int
}

type memObject struct {
	data         []byte
	etag         string
	contentType  string
	lastModified time.Time
}

type memUpload struct {
	bucket      string
	key         string
	contentType string
	initiated   time.Time
	parts       map[int32]memPart
}

type memPart struct {
	data []byte
	etag string
}

func newMemStore() *memStore {
	return &memStore{
		buckets: map[string]bool{},
		objects: map[string]map[string]*memObject{},
		uploads: map[string]*memUpload{},
	}
}

func (s *memStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.buckets[bucket] {
		return nil, ErrNotFound
	}
	if s.objects[bucket] == nil {
		s.objects[bucket] = map[string]*memObject{}
	}
	lm := time.Now().UTC()
	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])
	s.objects[bucket][key] = &memObject{data: data, etag: etag, contentType: contentType, lastModified: lm}
	return &ObjectMeta{Key: key, Size: int64(len(data)), ETag: etag, LastModified: lm, ContentType: contentType}, nil
}

func (s *memStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	s.mu.Lock()
	obj, ok := s.objects[bucket][key]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	meta := &ObjectMeta{
		Key:          key,
		Size:         int64(len(obj.data)),
		ETag:         obj.etag,
		LastModified: obj.lastModified,
		ContentType:  obj.contentType,
	}
	rc := io.NopCloser(strings.NewReader(string(obj.data)))
	s.mu.Unlock()
	return rc, meta, nil
}

func (s *memStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[bucket][key]
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectMeta{
		Key:          key,
		Size:         int64(len(obj.data)),
		ETag:         obj.etag,
		LastModified: obj.lastModified,
		ContentType:  obj.contentType,
	}, nil
}

func (s *memStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.buckets[bucket] {
		return nil, ErrNotFound
	}
	var out []ObjectMeta
	for key, obj := range s.objects[bucket] {
		out = append(out, ObjectMeta{
			Key:          key,
			Size:         int64(len(obj.data)),
			ETag:         obj.etag,
			LastModified: obj.lastModified,
			ContentType:  obj.contentType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *memStore) ListBuckets(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for b := range s.buckets {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (s *memStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buckets[bucket], nil
}

func (s *memStore) CreateBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket] = true
	return nil
}

func (s *memStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "memupload-" + strconv.Itoa(s.seq)
	s.uploads[id] = &memUpload{
		bucket:      bucket,
		key:         key,
		contentType: contentType,
		initiated:   time.Now().UTC(),
		parts:       map[int32]memPart{},
	}
	return id, nil
}

func (s *memStore) uploadLocked(id, bucket, key string) (*memUpload, bool) {
	u, ok := s.uploads[id]
	if !ok || u.bucket != bucket || u.key != key {
		return nil, false
	}
	return u, true
}

func (s *memStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploadLocked(uploadID, bucket, key)
	if !ok {
		return "", errUploadNotFound
	}
	if partNumber < 1 || partNumber > 10000 {
		return "", errInvalidPart
	}
	sum := md5.Sum(data)
	u.parts[partNumber] = memPart{data: data, etag: hex.EncodeToString(sum[:])}
	return u.parts[partNumber].etag, nil
}

func (s *memStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploadLocked(uploadID, bucket, key)
	if !ok {
		return nil, errUploadNotFound
	}
	if len(parts) == 0 || len(parts) != len(u.parts) {
		return nil, errInvalidPart
	}
	sorted := append([]CompletedPart{}, parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PartNumber < sorted[j].PartNumber })
	var concat, data []byte
	for _, p := range sorted {
		staged, ok := u.parts[p.PartNumber]
		if !ok || !strings.EqualFold(staged.etag, p.ETag) {
			return nil, errInvalidPart
		}
		b, _ := hex.DecodeString(staged.etag)
		concat = append(concat, b...)
		data = append(data, staged.data...)
	}
	sum := md5.Sum(concat)
	etag := hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(sorted))
	delete(s.uploads, uploadID)
	if s.objects[bucket] == nil {
		s.objects[bucket] = map[string]*memObject{}
	}
	lm := time.Now().UTC()
	s.objects[bucket][key] = &memObject{data: data, etag: etag, contentType: u.contentType, lastModified: lm}
	return &ObjectMeta{Key: key, Size: int64(len(data)), ETag: etag, LastModified: lm, ContentType: u.contentType}, nil
}

func (s *memStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.uploadLocked(uploadID, bucket, key); !ok {
		return errUploadNotFound
	}
	delete(s.uploads, uploadID)
	return nil
}

func (s *memStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploadLocked(uploadID, bucket, key)
	if !ok {
		return nil, errUploadNotFound
	}
	nums := make([]int, 0, len(u.parts))
	for n := range u.parts {
		nums = append(nums, int(n))
	}
	sort.Ints(nums)
	out := make([]PartInfo, 0, len(nums))
	for _, n := range nums {
		p := u.parts[int32(n)]
		out = append(out, PartInfo{PartNumber: int32(n), ETag: p.etag, Size: int64(len(p.data))})
	}
	return out, nil
}

func (s *memStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MultipartInfo
	for id, u := range s.uploads {
		if u.bucket != bucket {
			continue
		}
		out = append(out, MultipartInfo{Key: u.key, UploadID: id, Initiated: u.initiated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadID < out[j].UploadID })
	return out, nil
}
