package main

import (
	"net/http"
	"strings"
)

// s3Op identifies an S3 API operation after request routing.
type s3Op int

const (
	opNotSupported s3Op = iota
	opUnknown
	opListBuckets
	opCreateBucket
	opHeadBucket
	opBucketLocation
	opBucketMeta
	opListObjectsV2
	opListMultipartUploads
	opGetObject
	opHeadObject
	opPutObject
	opCopyObject
	opCreateMultipartUpload
	opUploadPart
	opCompleteMultipartUpload
	opAbortMultipartUpload
	opListParts
)

// bucketMetaParams are the bucket-metadata queries clients (mc, SDKs)
// send eagerly; they map to opBucketMeta which reports unconfigured
// defaults instead of a full feature implementation.
var bucketMetaParams = map[string]bool{
	"object-lock":       true,
	"versioning":        true,
	"policy":            true,
	"tagging":           true,
	"lifecycle":         true,
	"cors":              true,
	"encryption":        true,
	"replication":       true,
	"accelerate":        true,
	"logging":           true,
	"notification":      true,
	"website":           true,
	"acl":               true,
	"requestPayment":    true,
	"inventory":         true,
	"metrics":           true,
	"analytics":         true,
	"intelligent-tiering": true,
}

// splitPath splits the request path into bucket and key. Path-style
// addressing is assumed: the first segment is the bucket.
func splitPath(r *http.Request) (bucket, key string) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// parseS3Request maps an HTTP request to an S3 operation plus the bucket
// and key it addresses. Unsupported operations map to opNotSupported.
func parseS3Request(r *http.Request) (op s3Op, bucket, key string) {
	bucket, key = splitPath(r)
	q := r.URL.Query()
	switch {
	case bucket == "":
		if r.Method == http.MethodGet {
			return opListBuckets, "", ""
		}
	case key == "":
		switch {
		case r.Method == http.MethodPut:
			return opCreateBucket, bucket, ""
		case r.Method == http.MethodHead:
			return opHeadBucket, bucket, ""
		case r.Method == http.MethodGet && q.Has("location"):
			return opBucketLocation, bucket, ""
		case r.Method == http.MethodGet && q.Has("uploads"):
			return opListMultipartUploads, bucket, ""
		case r.Method == http.MethodGet:
			for k := range bucketMetaParams {
				if q.Has(k) {
					return opBucketMeta, bucket, ""
				}
			}
			return opListObjectsV2, bucket, ""
		}
	default:
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			return opCreateMultipartUpload, bucket, key
		case r.Method == http.MethodPut && q.Has("partNumber"):
			return opUploadPart, bucket, key
		case r.Method == http.MethodPost && q.Has("uploadId"):
			return opCompleteMultipartUpload, bucket, key
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			return opAbortMultipartUpload, bucket, key
		case r.Method == http.MethodGet && q.Has("uploadId"):
			return opListParts, bucket, key
		case r.Method == http.MethodGet:
			return opGetObject, bucket, key
		case r.Method == http.MethodHead:
			return opHeadObject, bucket, key
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			return opCopyObject, bucket, key
		case r.Method == http.MethodPut:
			return opPutObject, bucket, key
		case r.Method == http.MethodDelete:
			return opNotSupported, bucket, key
		}
	}
	return opUnknown, bucket, key
}
