package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	zlog "github.com/rs/zerolog/log"
)

// s3Error is a structured S3 API error rendered as the standard
// <Error><Code>...</Code><Message>...</Message></Error> XML body.
type s3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *s3Error) Error() string {
	return e.Code + ": " + e.Message
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	writeXML(w, status, sigError{Code: code, Message: message, RequestID: "overlay-s3"})
}

// s3Server is the HTTP layer of the gateway. It routes S3 requests and
// maps them onto the Store abstraction; protocol details (routing, XML)
// live here while storage semantics live in Store implementations.
type s3Server struct {
	store Store
}

func newS3Server(store Store) http.Handler {
	return &s3Server{store: store}
}

// backendAPIError maps an S3 API error surfaced by a backend store onto the
// client-facing error it should have produced. Without it a backend 4xx —
// e.g. MinIO rejecting a listing prefix containing "//" — is logged and
// returned as a misleading 500 InternalError.
func backendAPIError(err error) *s3Error {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return nil
	}
	switch apiErr.ErrorCode() {
	case "AccessDenied":
		return &s3Error{http.StatusForbidden, "AccessDenied", apiErr.ErrorMessage()}
	case "InvalidObjectName", "XMinioInvalidObjectName":
		return &s3Error{http.StatusBadRequest, "InvalidObjectName", apiErr.ErrorMessage()}
	}
	return nil
}

func (s *s3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op, bucket, key := parseS3Request(r)
	if err := s.dispatch(w, r, op, bucket, key); err != nil {
		var se *s3Error
		if errors.As(err, &se) {
			writeS3Error(w, se.Status, se.Code, se.Message)
			return
		}
		if be := backendAPIError(err); be != nil {
			writeS3Error(w, be.Status, be.Code, be.Message)
			return
		}
		zlog.Error().Str("method", r.Method).Str("path", r.URL.Path).Err(err).Msg("s3 handler failed")
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
	}
}

// noSuchBucket maps ErrNotFound to a NoSuchBucket error for operations
// that require the bucket itself to exist (listings, writes, multipart
// initiation). The overlay bucket is ensured at startup, so backend
// NoSuchBucket here means a client-visible bucket that does not exist.
func noSuchBucket(err error) error {
	if errors.Is(err, ErrNotFound) {
		return &s3Error{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist"}
	}
	return err
}

func (s *s3Server) dispatch(w http.ResponseWriter, r *http.Request, op s3Op, bucket, key string) error {
	switch op {
	case opListBuckets:
		return s.handleListBuckets(w, r)
	case opCreateBucket:
		return s.handleCreateBucket(w, r, bucket)
	case opHeadBucket:
		return s.handleHeadBucket(w, r, bucket)
	case opBucketLocation:
		return s.handleBucketLocation(w, r, bucket)
	case opBucketMeta:
		return s.handleBucketMeta(w, r, bucket)
	case opListObjectsV2:
		return s.handleListObjectsV2(w, r, bucket)
	case opListMultipartUploads:
		return s.handleListMultipartUploads(w, r, bucket)
	case opGetObject:
		return s.handleGetObject(w, r, bucket, key)
	case opHeadObject:
		return s.handleHeadObject(w, r, bucket, key)
	case opPutObject:
		return s.handlePutObject(w, r, bucket, key)
	case opCopyObject:
		return s.handleCopyObject(w, r, bucket, key)
	case opCreateMultipartUpload:
		return s.handleCreateMultipartUpload(w, r, bucket, key)
	case opUploadPart:
		return s.handleUploadPart(w, r, bucket, key)
	case opCompleteMultipartUpload:
		return s.handleCompleteMultipartUpload(w, r, bucket, key)
	case opAbortMultipartUpload:
		return s.handleAbortMultipartUpload(w, r, bucket, key)
	case opListParts:
		return s.handleListParts(w, r, bucket, key)
	default:
		return &s3Error{http.StatusNotImplemented, "NotImplemented",
			fmt.Sprintf("operation not supported: %s %s", r.Method, r.URL.Path)}
	}
}

func (s *s3Server) handleListBuckets(w http.ResponseWriter, r *http.Request) error {
	names, err := s.store.ListBuckets(r.Context())
	if err != nil {
		return err
	}
	res := listAllMyBucketsResult{
		Xmlns: s3NS,
		Owner: owner{ID: "overlay-s3", DisplayName: "overlay-s3"},
	}
	now := rfc3339(time.Now())
	for _, n := range names {
		res.Buckets = append(res.Buckets, bucketEntry{Name: n, CreationDate: now})
	}
	writeXML(w, http.StatusOK, res)
	return nil
}

func (s *s3Server) handleCreateBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	if err := s.store.CreateBucket(r.Context(), bucket); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *s3Server) handleHeadBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	// a client-visible bucket is assumed to exist: it was either created
	// through the gateway (overlay) or is served read-only from the
	// baseline. Probing existence would require permissions (HeadBucket)
	// on a store that may not hold the bucket at all.
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *s3Server) handleBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) error {
	// a client-visible bucket is assumed to exist (created through the
	// gateway or served from the baseline); answering location for any
	// bucket keeps minio-go's getBucketLocation preflight from aborting
	// uploads to not-yet-created buckets
	writeXML(w, http.StatusOK, locationConstraint{Xmlns: s3NS})
	return nil
}

// handleBucketMeta answers the bucket-metadata queries that clients (mc,
// SDKs) send eagerly; the gateway has no versioning or object-lock
// configuration, so it reports the unconfigured defaults.
func (s *s3Server) handleBucketMeta(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	switch {
	case q.Has("versioning"):
		writeXML(w, http.StatusOK, versioningConfiguration{Xmlns: s3NS})
	case q.Has("object-lock"):
		writeXML(w, http.StatusOK, objectLockConfiguration{
			Xmlns: s3NS, ObjectLockEnabled: "Enabled",
		})
	default:
		return &s3Error{http.StatusNotImplemented, "NotImplemented",
			"bucket metadata query not supported"}
	}
	return nil
}

func (s *s3Server) handleListObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	p := ListParams{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		After:     q.Get("continuation-token"),
		MaxKeys:   1000,
	}
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return &s3Error{http.StatusBadRequest, "InvalidArgument", "max-keys"}
		}
		p.MaxKeys = n
	}

	page, err := s.store.ListPage(r.Context(), bucket, p)
	if err != nil {
		return noSuchBucket(err)
	}

	res := listBucketResult{
		Xmlns:             s3NS,
		Name:              bucket,
		Prefix:            p.Prefix,
		Delimiter:         p.Delimiter,
		KeyCount:          len(page.Objects) + len(page.CommonPrefixes),
		MaxKeys:           p.MaxKeys,
		IsTruncated:       page.Truncated,
		ContinuationToken: p.After,
	}
	for i := range page.Objects {
		o := page.Objects[i]
		res.Contents = append(res.Contents, objectEntry{
			Key:          o.Key,
			LastModified: rfc3339(o.LastModified),
			ETag:         `"` + o.ETag + `"`,
			Size:         o.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range page.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{Prefix: cp})
	}
	if page.Truncated {
		res.NextContinuationToken = page.NextToken
	}
	writeXML(w, http.StatusOK, res)
	return nil
}

type httpRange struct {
	start, length int64
}

func parseRange(h string, size int64) (*httpRange, error) {
	if !strings.HasPrefix(h, "bytes=") {
		return nil, fmt.Errorf("unsupported range %q", h)
	}
	spec := strings.TrimPrefix(h, "bytes=")
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return nil, fmt.Errorf("invalid range %q", h)
	}
	if startStr == "" {
		// suffix range: last N bytes
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("invalid suffix range %q", h)
		}
		if n > size {
			n = size
		}
		return &httpRange{start: size - n, length: n}, nil
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= size {
		return nil, fmt.Errorf("invalid range %q", h)
	}
	end := size - 1
	if endStr != "" {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || end < start {
			return nil, fmt.Errorf("invalid range %q", h)
		}
		if end >= size {
			end = size - 1
		}
	}
	return &httpRange{start: start, length: end - start + 1}, nil
}

func writeObjectHeaders(w http.ResponseWriter, meta *ObjectMeta, status int) {
	h := w.Header()
	h.Set("ETag", `"`+meta.ETag+`"`)
	ct := meta.ContentType
	if ct == "" {
		ct = "binary/octet-stream"
	}
	h.Set("Content-Type", ct)
	if !meta.LastModified.IsZero() {
		h.Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(status)
}

func (s *s3Server) handleGetObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	rc, meta, err := s.store.Get(r.Context(), bucket, key)
	if err == ErrNotFound {
		return &s3Error{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	}
	if err != nil {
		return err
	}
	defer rc.Close()

	if rngHdr := r.Header.Get("Range"); rngHdr != "" {
		rng, err := parseRange(rngHdr, meta.Size)
		if err != nil {
			return &s3Error{http.StatusRequestedRangeNotSatisfiable, "InvalidRange",
				"The requested range is not satisfiable"}
		}
		total := meta.Size
		if seeker, ok := rc.(io.ReadSeeker); ok {
			if _, err := seeker.Seek(rng.start, io.SeekStart); err != nil {
				return err
			}
			rc = io.NopCloser(io.LimitReader(seeker, rng.length))
		}
		meta.Size = rng.length
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, total))
		writeObjectHeaders(w, meta, http.StatusPartialContent)
	} else {
		writeObjectHeaders(w, meta, http.StatusOK)
	}
	_, err = io.Copy(w, rc)
	return err
}

func (s *s3Server) handleHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	meta, err := s.store.Head(r.Context(), bucket, key)
	if err == ErrNotFound {
		return &s3Error{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	}
	if err != nil {
		return err
	}
	writeObjectHeaders(w, meta, http.StatusOK)
	return nil
}

func (s *s3Server) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Header.Get("If-None-Match") == "*" {
		_, err := s.store.Head(r.Context(), bucket, key)
		if err == nil {
			return &s3Error{http.StatusPreconditionFailed, "PreconditionFailed",
				"object already exists"}
		}
		if err != ErrNotFound {
			return err
		}
	}
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		existing, err := s.store.Head(r.Context(), bucket, key)
		if err != nil && err != ErrNotFound {
			return err
		}
		if err == ErrNotFound || existing.ETag != strings.Trim(ifMatch, `"`) {
			return &s3Error{http.StatusPreconditionFailed, "PreconditionFailed",
				"etag mismatch"}
		}
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	meta, err := s.store.Put(r.Context(), bucket, key, r.Body, contentType)
	if err != nil {
		return noSuchBucket(err)
	}
	w.Header().Set("ETag", `"`+meta.ETag+`"`)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *s3Server) handleCopyObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	src := strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/")
	if i := strings.IndexByte(src, '?'); i >= 0 {
		src = src[:i]
	}
	srcBucket, srcKey, ok := strings.Cut(src, "/")
	if !ok || srcBucket == "" || srcKey == "" {
		return &s3Error{http.StatusBadRequest, "InvalidArgument", "X-Amz-Copy-Source"}
	}
	rc, srcMeta, err := s.store.Get(r.Context(), srcBucket, srcKey)
	if err == ErrNotFound {
		return &s3Error{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	contentType := srcMeta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	dstMeta, err := s.store.Put(r.Context(), bucket, key, rc, contentType)
	if err != nil {
		return noSuchBucket(err)
	}
	writeXML(w, http.StatusOK, copyObjectResult{
		Xmlns:        s3NS,
		ETag:         `"` + dstMeta.ETag + `"`,
		LastModified: rfc3339(dstMeta.LastModified),
	})
	return nil
}

func (s *s3Server) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	id, err := s.store.InitiateMultipart(r.Context(), bucket, key, contentType)
	if err != nil {
		return noSuchBucket(err)
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{
		Xmlns: s3NS, Bucket: bucket, Key: key, UploadID: id,
	})
	return nil
}

func (s *s3Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	uploadID := r.URL.Query().Get("uploadId")
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber <= 0 {
		return &s3Error{http.StatusBadRequest, "InvalidArgument", "partNumber"}
	}
	etag, err := s.store.UploadPart(r.Context(), bucket, key, uploadID, int32(partNumber), r.Body)
	if errors.Is(err, errUploadNotFound) {
		return &s3Error{http.StatusNotFound, "NoSuchUpload",
			"The specified multipart upload does not exist"}
	}
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
	return nil
}

type completeMultipartUploadRequest struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

func (s *s3Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	uploadID := r.URL.Query().Get("uploadId")
	var req completeMultipartUploadRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		return &s3Error{http.StatusBadRequest, "MalformedXML", err.Error()}
	}
	parts := make([]CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, CompletedPart{
			PartNumber: int32(p.PartNumber),
			ETag:       strings.Trim(p.ETag, `"`),
		})
	}
	meta, err := s.store.CompleteMultipart(r.Context(), bucket, key, uploadID, parts)
	if errors.Is(err, errUploadNotFound) {
		return &s3Error{http.StatusNotFound, "NoSuchUpload",
			"The specified multipart upload does not exist"}
	}
	if errors.Is(err, errInvalidPart) {
		return &s3Error{http.StatusBadRequest, "InvalidPart",
			"One or more of the specified parts could not be found or have the wrong ETag"}
	}
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		Xmlns:    s3NS,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     `"` + meta.ETag + `"`,
	})
	return nil
}

func (s *s3Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	err := s.store.AbortMultipart(r.Context(), bucket, key, r.URL.Query().Get("uploadId"))
	if errors.Is(err, errUploadNotFound) {
		return &s3Error{http.StatusNotFound, "NoSuchUpload",
			"The specified multipart upload does not exist"}
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *s3Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	uploadID := r.URL.Query().Get("uploadId")
	parts, err := s.store.ListParts(r.Context(), bucket, key, uploadID)
	if errors.Is(err, errUploadNotFound) {
		return &s3Error{http.StatusNotFound, "NoSuchUpload",
			"The specified multipart upload does not exist"}
	}
	if err != nil {
		return err
	}
	res := listPartsResult{Xmlns: s3NS, Bucket: bucket, Key: key, UploadID: uploadID}
	for _, p := range parts {
		res.Parts = append(res.Parts, partEntry{
			PartNumber:   int(p.PartNumber),
			LastModified: rfc3339(p.LastModified),
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
		})
	}
	writeXML(w, http.StatusOK, res)
	return nil
}

func (s *s3Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) error {
	uploads, err := s.store.ListMultipartUploads(r.Context(), bucket)
	if err != nil {
		return err
	}
	res := listMultipartUploadsResult{Xmlns: s3NS, Bucket: bucket}
	for _, u := range uploads {
		res.Uploads = append(res.Uploads, multipartUploadEntry{
			Key:          u.Key,
			UploadID:     u.UploadID,
			Initiated:    rfc3339(u.Initiated),
			StorageClass: "STANDARD",
		})
	}
	writeXML(w, http.StatusOK, res)
	return nil
}
