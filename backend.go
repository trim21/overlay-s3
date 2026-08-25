package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/johannesboyne/gofakes3"
)

// overlayBackend adapts the overlay Store to gofakes3's Backend and
// MultipartBackend interfaces. gofakes3 owns the protocol (routing, XML,
// multipart flow, pagination); this type only maps its calls onto the
// local-over-remote overlay semantics.
type overlayBackend struct {
	store Store
}

func newOverlayBackend(store Store) *overlayBackend {
	return &overlayBackend{store: store}
}

func notSupported(what string) error {
	return gofakes3.ErrorMessage(gofakes3.ErrNotImplemented,
		what+" not supported")
}

func (b *overlayBackend) ListBuckets() ([]gofakes3.BucketInfo, error) {
	names, err := b.store.ListBuckets(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]gofakes3.BucketInfo, 0, len(names))
	for _, n := range names {
		out = append(out, gofakes3.BucketInfo{
			Name:         n,
			CreationDate: gofakes3.NewContentTime(time.Now().UTC()),
		})
	}
	return out, nil
}

func (b *overlayBackend) ListBucket(name string, prefix *gofakes3.Prefix, page gofakes3.ListBucketPage) (*gofakes3.ObjectList, error) {
	objs, err := b.store.List(context.Background(), name)
	if err != nil {
		return nil, err
	}
	if prefix == nil {
		prefix = &gofakes3.Prefix{}
	}
	type item struct {
		key    string
		prefix bool
		obj    gofakes3.Content
	}
	var items []item
	for _, obj := range objs {
		var match gofakes3.PrefixMatch
		if !prefix.Match(obj.Key, &match) {
			continue
		}
		if match.CommonPrefix {
			items = append(items, item{key: match.MatchedPart, prefix: true})
			continue
		}
		items = append(items, item{
			key: obj.Key,
			obj: gofakes3.Content{
				Key:          obj.Key,
				LastModified: gofakes3.NewContentTime(obj.LastModified),
				ETag:         `"` + obj.ETag + `"`,
				Size:         obj.Size,
				StorageClass: gofakes3.StorageStandard,
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	if page.HasMarker {
		i := sort.Search(len(items), func(i int) bool { return items[i].key > page.Marker })
		items = items[i:]
	}
	var truncated bool
	if page.MaxKeys > 0 && int64(len(items)) > page.MaxKeys {
		items = items[:page.MaxKeys]
		truncated = true
	}
	list := gofakes3.NewObjectList()
	for _, it := range items {
		if it.prefix {
			list.AddPrefix(it.key)
		} else {
			list.Add(&it.obj)
		}
	}
	list.IsTruncated = truncated
	if truncated && len(items) > 0 {
		list.NextMarker = items[len(items)-1].key
	}
	return list, nil
}

func (b *overlayBackend) CreateBucket(name string) error {
	if err := b.store.CreateBucket(context.Background(), name); err != nil {
		return err
	}
	return nil
}

func (b *overlayBackend) BucketExists(name string) (bool, error) {
	return b.store.BucketExists(context.Background(), name)
}

func (b *overlayBackend) DeleteBucket(name string) error {
	return notSupported("bucket deletion")
}

func (b *overlayBackend) ForceDeleteBucket(name string) error {
	return notSupported("bucket deletion")
}

func etagToHash(etag string) ([]byte, error) {
	// multipart etags carry a "-N" suffix that gofakes3 cannot express in
	// an Object.Hash; the GET/HEAD ETag falls back to the digest alone
	if i := strings.IndexByte(etag, '-'); i >= 0 {
		etag = etag[:i]
	}
	h, err := hex.DecodeString(etag)
	if err != nil {
		return nil, fmt.Errorf("invalid etag %q", etag)
	}
	return h, nil
}

func objectMetadata(meta *ObjectMeta) map[string]string {
	m := make(map[string]string, 2)
	if meta.ContentType != "" {
		m["Content-Type"] = meta.ContentType
	}
	if !meta.LastModified.IsZero() {
		m["Last-Modified"] = meta.LastModified.UTC().Format(http.TimeFormat)
	}
	return m
}

func (b *overlayBackend) GetObject(bucketName, objectName string, rng *gofakes3.ObjectRangeRequest) (*gofakes3.Object, error) {
	rc, meta, err := b.store.Get(context.Background(), bucketName, objectName)
	if err == ErrNotFound {
		return nil, gofakes3.KeyNotFound(objectName)
	}
	if err != nil {
		return nil, err
	}
	hash, err := etagToHash(meta.ETag)
	if err != nil {
		rc.Close()
		return nil, err
	}
	obj := &gofakes3.Object{
		Name:     objectName,
		Metadata: objectMetadata(meta),
		Size:     meta.Size,
		Contents: rc,
		Hash:     hash,
	}
	if rng != nil {
		or, err := rng.Range(meta.Size)
		if err != nil {
			rc.Close()
			return nil, err
		}
		if seeker, ok := rc.(io.ReadSeeker); ok {
			if _, err := seeker.Seek(or.Start, io.SeekStart); err != nil {
				rc.Close()
				return nil, err
			}
			obj.Contents = io.NopCloser(io.LimitReader(seeker, or.Length))
			obj.Range = or
			obj.Size = or.Length
		}
	}
	return obj, nil
}

func (b *overlayBackend) HeadObject(bucketName, objectName string) (*gofakes3.Object, error) {
	meta, err := b.store.Head(context.Background(), bucketName, objectName)
	if err == ErrNotFound {
		return nil, gofakes3.KeyNotFound(objectName)
	}
	if err != nil {
		return nil, err
	}
	hash, err := etagToHash(meta.ETag)
	if err != nil {
		return nil, err
	}
	return &gofakes3.Object{
		Name:     objectName,
		Metadata: objectMetadata(meta),
		Size:     meta.Size,
		Contents: io.NopCloser(strings.NewReader("")),
		Hash:     hash,
	}, nil
}

func (b *overlayBackend) DeleteObject(bucketName, objectName string) (gofakes3.ObjectDeleteResult, error) {
	return gofakes3.ObjectDeleteResult{}, notSupported("object deletion")
}

func (b *overlayBackend) PutObject(bucketName, key string, meta map[string]string, input io.Reader, size int64, conditions *gofakes3.PutConditions) (gofakes3.PutObjectResult, error) {
	if conditions != nil {
		if conditions.IfNoneMatch != nil && *conditions.IfNoneMatch == "*" {
			if _, err := b.store.Head(context.Background(), bucketName, key); err == nil {
				return gofakes3.PutObjectResult{}, gofakes3.ErrorMessage(
					gofakes3.ErrPreconditionFailed, "object already exists")
			} else if err != ErrNotFound {
				return gofakes3.PutObjectResult{}, err
			}
		}
		if conditions.IfMatch != nil {
			existing, err := b.store.Head(context.Background(), bucketName, key)
			if err == ErrNotFound ||
				existing.ETag != strings.Trim(*conditions.IfMatch, `"`) {
				return gofakes3.PutObjectResult{}, gofakes3.ErrorMessage(
					gofakes3.ErrPreconditionFailed, "etag mismatch")
			}
		}
	}
	contentType := meta["Content-Type"]
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	if _, err := b.store.Put(context.Background(), bucketName, key, input, size, contentType); err != nil {
		return gofakes3.PutObjectResult{}, err
	}
	return gofakes3.PutObjectResult{}, nil
}

func (b *overlayBackend) DeleteMulti(bucketName string, objects ...string) (gofakes3.MultiDeleteResult, error) {
	return gofakes3.MultiDeleteResult{}, notSupported("object deletion")
}

func (b *overlayBackend) CopyObject(srcBucket, srcKey, dstBucket, dstKey string, meta map[string]string) (gofakes3.CopyObjectResult, error) {
	rc, srcMeta, err := b.store.Get(context.Background(), srcBucket, srcKey)
	if err == ErrNotFound {
		return gofakes3.CopyObjectResult{}, gofakes3.KeyNotFound(srcKey)
	}
	if err != nil {
		return gofakes3.CopyObjectResult{}, err
	}
	defer rc.Close()
	contentType := srcMeta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	dstMeta, err := b.store.Put(context.Background(), dstBucket, dstKey, rc, srcMeta.Size, contentType)
	if err != nil {
		return gofakes3.CopyObjectResult{}, err
	}
	return gofakes3.CopyObjectResult{
		ETag:         `"` + dstMeta.ETag + `"`,
		LastModified: gofakes3.NewContentTime(dstMeta.LastModified),
	}, nil
}

func (b *overlayBackend) CreateMultipartUpload(bucket, object string, meta map[string]string) (gofakes3.UploadID, error) {
	contentType := meta["Content-Type"]
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	id, err := b.store.InitiateMultipart(context.Background(), bucket, object, contentType)
	if err != nil {
		return "", err
	}
	return gofakes3.UploadID(id), nil
}

func (b *overlayBackend) UploadPart(bucket, object string, id gofakes3.UploadID, partNumber int, contentLength int64, input io.Reader) (string, error) {
	return b.store.UploadPart(context.Background(), bucket, object, string(id),
		int32(partNumber), input, contentLength)
}

func (b *overlayBackend) CompleteMultipartUpload(bucket, object string, id gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	parts := make([]CompletedPart, 0, len(input.Parts))
	for _, p := range input.Parts {
		parts = append(parts, CompletedPart{PartNumber: int32(p.PartNumber), ETag: p.ETag})
	}
	meta, err := b.store.CompleteMultipart(context.Background(), bucket, object, string(id), parts)
	if err != nil {
		if errors.Is(err, errUploadNotFound) {
			return "", "", gofakes3.ErrorMessage(gofakes3.ErrNoSuchUpload,
				"The specified multipart upload does not exist")
		}
		if errors.Is(err, errInvalidPart) {
			return "", "", gofakes3.ErrorMessage(gofakes3.ErrInvalidPart,
				"One or more of the specified parts could not be found or have the wrong ETag")
		}
		return "", "", err
	}
	return "", meta.ETag, nil
}

func (b *overlayBackend) AbortMultipartUpload(bucket, object string, id gofakes3.UploadID) error {
	err := b.store.AbortMultipart(context.Background(), bucket, object, string(id))
	if errors.Is(err, errUploadNotFound) {
		return gofakes3.ErrorMessage(gofakes3.ErrNoSuchUpload,
			"The specified multipart upload does not exist")
	}
	return err
}

func (b *overlayBackend) ListParts(bucket, object string, uploadID gofakes3.UploadID, marker int, limit int64) (*gofakes3.ListMultipartUploadPartsResult, error) {
	parts, err := b.store.ListParts(context.Background(), bucket, object, string(uploadID))
	if err != nil {
		if errors.Is(err, errUploadNotFound) {
			return nil, gofakes3.ErrorMessage(gofakes3.ErrNoSuchUpload,
				"The specified multipart upload does not exist")
		}
		return nil, err
	}
	var items []gofakes3.ListMultipartUploadPartItem
	for _, p := range parts {
		if p.PartNumber <= int32(marker) {
			continue
		}
		items = append(items, gofakes3.ListMultipartUploadPartItem{
			PartNumber:   int(p.PartNumber),
			LastModified: gofakes3.NewContentTime(p.LastModified),
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
		})
	}
	var truncated bool
	if limit > 0 && int64(len(items)) > limit {
		items = items[:limit]
		truncated = true
	}
	result := &gofakes3.ListMultipartUploadPartsResult{
		Bucket:           bucket,
		Key:              object,
		UploadID:         uploadID,
		StorageClass:     gofakes3.StorageStandard,
		PartNumberMarker: marker,
		MaxParts:         limit,
		IsTruncated:      truncated,
		Parts:            items,
	}
	if truncated && len(items) > 0 {
		result.NextPartNumberMarker = items[len(items)-1].PartNumber
	}
	return result, nil
}

func (b *overlayBackend) ListMultipartUploads(bucket string, marker *gofakes3.UploadListMarker, prefix gofakes3.Prefix, limit int64) (*gofakes3.ListMultipartUploadsResult, error) {
	uploads, err := b.store.ListMultipartUploads(context.Background(), bucket)
	if err != nil {
		return nil, err
	}
	result := &gofakes3.ListMultipartUploadsResult{
		Bucket:     bucket,
		MaxUploads: limit,
		Prefix:     prefix.Prefix,
		Delimiter:  prefix.Delimiter,
	}
	if marker != nil {
		result.KeyMarker = marker.Object
		result.UploadIDMarker = marker.UploadID
	}
	for _, u := range uploads {
		if marker != nil && (u.Key < marker.Object ||
			(u.Key == marker.Object && u.UploadID <= string(marker.UploadID))) {
			continue
		}
		var match gofakes3.PrefixMatch
		if !prefix.Match(u.Key, &match) {
			continue
		}
		result.Uploads = append(result.Uploads, gofakes3.ListMultipartUploadItem{
			Key:          u.Key,
			UploadID:     gofakes3.UploadID(u.UploadID),
			StorageClass: gofakes3.StorageStandard,
			Initiated:    gofakes3.NewContentTime(u.Initiated),
		})
	}
	var truncated bool
	if limit > 0 && int64(len(result.Uploads)) > limit {
		result.Uploads = result.Uploads[:limit]
		truncated = true
	}
	result.IsTruncated = truncated
	if truncated && len(result.Uploads) > 0 {
		last := result.Uploads[len(result.Uploads)-1]
		result.NextKeyMarker = last.Key
		result.NextUploadIDMarker = last.UploadID
	}
	return result, nil
}
