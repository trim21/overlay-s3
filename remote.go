package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Store talks to a real S3-compatible backend.
//
// It serves both overlay roles: with no mapping configured it addresses the
// backend one-to-one (baseline mode); when a physical bucket and key prefix
// are configured, every client-visible (bucket, key) is stored as
// prefix/bucket/key inside that single bucket (overlay mode). In overlay
// mode client buckets are key namespaces: CreateBucket writes a zero-byte
// directory marker at prefix/bucket/ and existence is a prefix listing.
type s3Store struct {
	client *s3.Client

	mappedBucket string
	mappedPrefix string // without trailing slash; may be empty
}

func newS3Client(ctx context.Context, endpoint, region, accessKey, secretKey string) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return nil, err
	}
	opts := []func(*s3.Options){}
	if endpoint != "" {
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}
	return s3.NewFromConfig(cfg, opts...), nil
}

// newRemoteStore returns a store addressing the backend one-to-one.
func newRemoteStore(ctx context.Context, endpoint, region, accessKey, secretKey string) (*s3Store, error) {
	client, err := newS3Client(ctx, endpoint, region, accessKey, secretKey)
	if err != nil {
		return nil, err
	}
	return &s3Store{client: client}, nil
}

// newMappedStore returns an overlay-mode store nesting all client buckets
// under prefix inside bucket.
func newMappedStore(ctx context.Context, endpoint, region, accessKey, secretKey, bucket, prefix string) (*s3Store, error) {
	client, err := newS3Client(ctx, endpoint, region, accessKey, secretKey)
	if err != nil {
		return nil, err
	}
	return &s3Store{
		client:       client,
		mappedBucket: bucket,
		mappedPrefix: strings.TrimRight(prefix, "/"),
	}, nil
}

// EnsureBucket creates the physical backing bucket if needed. It is meant
// for overlay mode and tolerates buckets that already exist.
func (s *s3Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.mappedBucket),
	})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	return err
}

func joinKey(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

// mapKey translates a client (bucket, key); identity in baseline mode.
func (s *s3Store) mapKey(bucket, key string) (string, string) {
	if s.mappedBucket == "" {
		return bucket, key
	}
	return s.mappedBucket, joinKey(s.mappedPrefix, bucket, key)
}

// listPrefix is the key root holding one client bucket in overlay mode.
func (s *s3Store) listPrefix(bucket string) string {
	if s.mappedBucket == "" {
		return ""
	}
	return joinKey(s.mappedPrefix, bucket) + "/"
}

// unmapKey reverses mapKey for listing results; ok is false for keys outside
// the mapping root. A bare bucket segment decodes as the directory-marker
// object written by CreateBucket (empty object key).
func (s *s3Store) unmapKey(key string) (bucket, obj string, ok bool) {
	if s.mappedBucket == "" {
		return "", key, true
	}
	rest := key
	if s.mappedPrefix != "" {
		if !strings.HasPrefix(rest, s.mappedPrefix+"/") {
			return "", "", false
		}
		rest = rest[len(s.mappedPrefix)+1:]
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "", rest != ""
	}
	if i == 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

func mapS3Error(err error) error {
	var nsk *types.NoSuchKey
	var nsb *types.NoSuchBucket
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nsb) || errors.As(err, &nf) {
		return ErrNotFound
	}
	return err
}

func (s *s3Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	b, k := s.mapKey(bucket, key)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b),
		Key:    aws.String(k),
	})
	if err != nil {
		return nil, nil, mapS3Error(err)
	}
	meta := &ObjectMeta{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
	}
	return out.Body, meta, nil
}

func (s *s3Store) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	b, k := s.mapKey(bucket, key)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b),
		Key:    aws.String(k),
	})
	if err != nil {
		return nil, mapS3Error(err)
	}
	return &ObjectMeta{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
	}, nil
}

func (s *s3Store) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	b, k := s.mapKey(bucket, key)
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b),
		Key:         aws.String(k),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: time.Now().UTC(),
	}, nil
}

func (s *s3Store) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	b, _ := s.mapKey(bucket, "")
	prefix := s.listPrefix(bucket)
	var out []ObjectMeta
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Error(err)
		}
		for _, obj := range resp.Contents {
			ck, ok := s.unmap(bucket, aws.ToString(obj.Key))
			if !ok {
				continue
			}
			out = append(out, ObjectMeta{
				Key:          ck,
				Size:         aws.ToInt64(obj.Size),
				ETag:         strings.Trim(aws.ToString(obj.ETag), `"`),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}

func (s *s3Store) ListBuckets(ctx context.Context) ([]string, error) {
	if s.mappedBucket == "" {
		out, err := s.client.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil {
			return nil, err
		}
		var buckets []string
		for _, b := range out.Buckets {
			buckets = append(buckets, aws.ToString(b.Name))
		}
		return buckets, nil
	}
	// client buckets are the first-level common prefixes under the mapping
	// root; the zero-byte directory markers written by CreateBucket also
	// surface there
	var buckets []string
	seen := map[string]bool{}
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.mappedBucket),
			Prefix:            aws.String(s.mappedPrefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, p := range resp.CommonPrefixes {
			name, ok := s.unmapPrefixRoot(aws.ToString(p.Prefix))
			if ok && !seen[name] {
				seen[name] = true
				buckets = append(buckets, name)
			}
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return buckets, nil
}

// unmap translates a physical key back into a client object key for the
// given client bucket; ok is false for keys outside the mapping root,
// directory markers, and anything else without a usable object key.
func (s *s3Store) unmap(bucket, key string) (string, bool) {
	if s.mappedBucket == "" {
		return key, key != ""
	}
	cb, obj, ok := s.unmapKey(key)
	if !ok || cb != bucket || obj == "" {
		return "", false
	}
	return obj, true
}

// unmapPrefixRoot recovers the client bucket name from a first-level common
// prefix like "prefix/bucket/".
func (s *s3Store) unmapPrefixRoot(prefix string) (string, bool) {
	bucket, _, ok := s.unmapKey(strings.TrimSuffix(prefix, "/"))
	return bucket, ok
}

func (s *s3Store) BucketExists(ctx context.Context, bucket string) (bool, error) {
	if s.mappedBucket == "" {
		_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		if err == nil {
			return true, nil
		}
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.mappedBucket),
		Prefix:  aws.String(s.listPrefix(bucket)),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	return len(resp.Contents) > 0 || len(resp.CommonPrefixes) > 0, nil
}

func (s *s3Store) CreateBucket(ctx context.Context, bucket string) error {
	if s.mappedBucket == "" {
		_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return err
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.mappedBucket),
		Key:    aws.String(joinKey(s.mappedPrefix, bucket) + "/"),
		Body:   bytes.NewReader(nil),
	})
	return err
}

func (s *s3Store) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	b, k := s.mapKey(bucket, key)
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(b),
		Key:         aws.String(k),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.UploadId), nil
}

func (s *s3Store) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	b, k := s.mapKey(bucket, key)
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(b),
		Key:        aws.String(k),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
		Body:       body,
	})
	if err != nil {
		return "", err
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

func (s *s3Store) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	b, k := s.mapKey(bucket, key)
	var completed []types.CompletedPart
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(p.PartNumber),
			ETag:       aws.String(`"` + p.ETag + `"`),
		})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b),
		Key:             aws.String(k),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: time.Now().UTC(),
	}, nil
}

func (s *s3Store) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	b, k := s.mapKey(bucket, key)
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(b),
		Key:      aws.String(k),
		UploadId: aws.String(uploadID),
	})
	return err
}

func (s *s3Store) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	b, k := s.mapKey(bucket, key)
	var out []PartInfo
	var marker *string
	for {
		resp, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(b),
			Key:              aws.String(k),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, p := range resp.Parts {
			out = append(out, PartInfo{
				PartNumber:   aws.ToInt32(p.PartNumber),
				ETag:         strings.Trim(aws.ToString(p.ETag), `"`),
				Size:         aws.ToInt64(p.Size),
				LastModified: aws.ToTime(p.LastModified),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		marker = resp.NextPartNumberMarker
	}
	return out, nil
}

func (s *s3Store) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	b, _ := s.mapKey(bucket, "")
	prefix := s.listPrefix(bucket)
	var out []MultipartInfo
	var keyMarker, uploadIDMarker *string
	for {
		resp, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(b),
			Prefix:         aws.String(prefix),
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if err != nil {
			return nil, err
		}
		for _, u := range resp.Uploads {
			ck, ok := s.unmap(bucket, aws.ToString(u.Key))
			if !ok {
				continue
			}
			out = append(out, MultipartInfo{
				Key:       ck,
				UploadID:  aws.ToString(u.UploadId),
				Initiated: aws.ToTime(u.Initiated),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		keyMarker = resp.NextKeyMarker
		uploadIDMarker = resp.NextUploadIdMarker
	}
	return out, nil
}
