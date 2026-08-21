package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Multipart uploads live under baseDir/.multipart/{bucket}/{key}/{uploadID}/
// with the uploaded parts as part-{n} files. Completion assembles the parts
// into the regular object tree and removes the staging directory.

type multipartMeta struct {
	Key         string    `json:"key"`
	ContentType string    `json:"contentType,omitempty"`
	Initiated   time.Time `json:"initiated"`
}

func (s *localStore) multipartDir(bucket, key, uploadID string) (string, error) {
	if !validPathSegments(bucket, key) || !validUploadID(uploadID) {
		return "", fmt.Errorf("invalid multipart path %q %q %q", bucket, key, uploadID)
	}
	return filepath.Join(s.baseDir, ".multipart",
		filepath.FromSlash(bucket), filepath.FromSlash(key), uploadID), nil
}

func validUploadID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func newUploadID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d%d", time.Now().UnixNano(),
		binary.BigEndian.Uint64(b)%1000000), nil
}

func (s *localStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	uploadID, err := newUploadID()
	if err != nil {
		return "", err
	}
	dir, err := s.multipartDir(bucket, key, uploadID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.Marshal(multipartMeta{
		Key:         key,
		ContentType: contentType,
		Initiated:   time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644); err != nil {
		return "", err
	}
	return uploadID, nil
}

func (s *localStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("invalid part number %d", partNumber)
	}
	dir, err := s.multipartDir(bucket, key, uploadID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errUploadNotFound
		}
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	h := md5.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	etag := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(dir, partFileName(partNumber))); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return etag, nil
}

func partFileName(n int32) string {
	return "part-" + strconv.FormatInt(int64(n), 10)
}

var errUploadNotFound = errors.New("multipart upload not found")

func (s *localStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	dir, err := s.multipartDir(bucket, key, uploadID)
	if err != nil {
		return nil, err
	}
	var meta multipartMeta
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errUploadNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	// The client's part list must exactly match the staged parts.
	staged := make(map[int32]string)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "part-") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(name, "part-"), 10, 32)
		if err != nil {
			continue
		}
		sum, err := fileSum(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		staged[int32(n)] = sum
	}
	complete := make([]CompletedPart, 0, len(parts))
	for _, p := range parts {
		etag, ok := staged[p.PartNumber]
		if !ok || !strings.EqualFold(strings.Trim(p.ETag, `"`), etag) {
			return nil, errInvalidPart
		}
		complete = append(complete, p)
	}
	if len(complete) != len(staged) {
		return nil, errInvalidPart
	}
	sort.Slice(complete, func(i, j int) bool {
		return complete[i].PartNumber < complete[j].PartNumber
	})

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
	concat := make([]byte, 0, len(complete)*md5.Size)
	var size int64
	for _, p := range complete {
		f, err := os.Open(filepath.Join(dir, partFileName(p.PartNumber)))
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		n, err := io.Copy(tmp, f)
		f.Close()
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		size += n
		b, err := hex.DecodeString(staged[p.PartNumber])
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		concat = append(concat, b...)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	sum := md5.Sum(concat)
	etag := hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(complete))
	if err := os.Rename(tmp.Name(), objPath); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}

	lm := time.Now().UTC()
	if err := s.writeMeta(bucket, key, localMeta{
		ETag:         etag,
		ContentType:  meta.ContentType,
		LastModified: lm,
	}); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		Size:         size,
		ETag:         etag,
		LastModified: lm,
		ContentType:  meta.ContentType,
	}, nil
}

var errInvalidPart = errors.New("invalid part")

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *localStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	dir, err := s.multipartDir(bucket, key, uploadID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errUploadNotFound
		}
		return err
	}
	return os.RemoveAll(dir)
}

func (s *localStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	dir, err := s.multipartDir(bucket, key, uploadID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errUploadNotFound
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []PartInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "part-") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(name, "part-"), 10, 32)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		sum, err := fileSum(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, PartInfo{
			PartNumber:   int32(n),
			ETag:         sum,
			Size:         info.Size(),
			LastModified: info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (s *localStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	root := filepath.Join(s.baseDir, ".multipart", filepath.FromSlash(bucket))
	var out []MultipartInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "meta.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m multipartMeta
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		uploadID := filepath.Base(filepath.Dir(path))
		keyRel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		// the staging tree is {key}/{uploadID}/meta.json; drop the
		// uploadID segment to recover the key
		keyRel = filepath.Dir(keyRel)
		out = append(out, MultipartInfo{
			Key:       filepath.ToSlash(keyRel),
			UploadID:  uploadID,
			Initiated: m.Initiated,
		})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].UploadID < out[j].UploadID
	})
	return out, nil
}
