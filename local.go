package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// localStore keeps objects on disk under baseDir/bucket/key, with an
// etag/content-type sidecar under baseDir/.meta/bucket/key.
type localStore struct {
	baseDir string
	metaDir string
}

func newLocalStore(dir string) (*localStore, error) {
	metaDir := filepath.Join(dir, ".meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return nil, err
	}
	return &localStore{baseDir: dir, metaDir: metaDir}, nil
}

type localMeta struct {
	ETag         string    `json:"etag"`
	ContentType  string    `json:"contentType,omitempty"`
	LastModified time.Time `json:"lastModified"`
}

func validPathSegments(parts ...string) bool {
	for _, p := range parts {
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return false
			}
		}
	}
	return true
}

func (s *localStore) objectPath(bucket, key string) (string, error) {
	if !validPathSegments(bucket, key) {
		return "", fmt.Errorf("invalid bucket or key %q %q", bucket, key)
	}
	return filepath.Join(s.baseDir, filepath.FromSlash(bucket), filepath.FromSlash(key)), nil
}

func (s *localStore) metaPath(bucket, key string) (string, error) {
	if !validPathSegments(bucket, key) {
		return "", fmt.Errorf("invalid bucket or key %q %q", bucket, key)
	}
	return filepath.Join(s.metaDir, filepath.FromSlash(bucket), filepath.FromSlash(key)), nil
}

func (s *localStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	objPath, err := s.objectPath(bucket, key)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(objPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	meta, err := s.readMeta(bucket, key, objPath)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, meta, nil
}

func (s *localStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	objPath, err := s.objectPath(bucket, key)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(objPath); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.readMeta(bucket, key, objPath)
}

func (s *localStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	objPath, err := s.objectPath(bucket, key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(objPath), ".tmp-*")
	if err != nil {
		return nil, err
	}
	h := md5.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	etag := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), objPath); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	lm := time.Now().UTC()
	meta := localMeta{ETag: etag, ContentType: contentType, LastModified: lm}
	if err := s.writeMeta(bucket, key, meta); err != nil {
		return nil, err
	}
	info, err := os.Stat(objPath)
	if err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		Size:         info.Size(),
		ETag:         etag,
		LastModified: lm,
		ContentType:  contentType,
	}, nil
}

// readMeta prefers the sidecar, and falls back to hashing the object itself
// for files written directly into the tree.
func (s *localStore) readMeta(bucket, key, objPath string) (*ObjectMeta, error) {
	mp, err := s.metaPath(bucket, key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(mp)
	if err == nil {
		var m localMeta
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		info, err := os.Stat(objPath)
		if err != nil {
			return nil, err
		}
		return &ObjectMeta{
			Key:          key,
			Size:         info.Size(),
			ETag:         m.ETag,
			LastModified: m.LastModified,
			ContentType:  m.ContentType,
		}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.Open(objPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		Size:         info.Size(),
		ETag:         hex.EncodeToString(h.Sum(nil)),
		LastModified: info.ModTime().UTC(),
	}, nil
}

func (s *localStore) writeMeta(bucket, key string, m localMeta) error {
	mp, err := s.metaPath(bucket, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(mp, data, 0o644)
}

func (s *localStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	dir := filepath.Join(s.baseDir, filepath.FromSlash(bucket))
	var out []ObjectMeta
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		meta, err := s.readMeta(bucket, key, path)
		if err != nil {
			return err
		}
		out = append(out, *meta)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *localStore) ListBuckets(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var buckets []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			buckets = append(buckets, e.Name())
		}
	}
	sort.Strings(buckets)
	return buckets, nil
}

func (s *localStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	if !validPathSegments(bucket) {
		return false, nil
	}
	info, err := os.Stat(filepath.Join(s.baseDir, filepath.FromSlash(bucket)))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func (s *localStore) CreateBucket(ctx context.Context, bucket string) error {
	if !validPathSegments(bucket) {
		return fmt.Errorf("invalid bucket %q", bucket)
	}
	return os.MkdirAll(filepath.Join(s.baseDir, filepath.FromSlash(bucket)), 0o755)
}
