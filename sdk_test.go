package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func newSDKClient(t *testing.T, ts *httptest.Server, key, secret string) *s3.Client {
	t.Helper()
	return newSDKClientForEndpoint(t, ts.URL, key, secret, "us-east-1")
}

func newSDKClientForEndpoint(t *testing.T, endpoint, key, secret, region string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(key, secret, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

func TestSDKEndToEnd(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")

	client := newSDKClient(t, ts, "AKID", "SECRET")
	ctx := context.Background()

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("bucket"),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String("bucket"),
		Key:         aws.String("dir/key"),
		Body:        strings.NewReader("hello"),
		ContentType: aws.String("text/plain"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got, want := aws.ToString(put.ETag), `"`+etagOf([]byte("hello"))+`"`; got != want {
		t.Fatalf("etag = %q, want %q", got, want)
	}

	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("dir/key"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	data, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("body = %q", data)
	}
	if ct := aws.ToString(got.ContentType); ct != "text/plain" {
		t.Fatalf("content-type = %q", ct)
	}
	if etag := aws.ToString(got.ETag); etag != `"`+etagOf([]byte("hello"))+`"` {
		t.Fatalf("etag = %q", etag)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("dir/key"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != 5 {
		t.Fatalf("head content-length = %d", aws.ToInt64(head.ContentLength))
	}

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != 1 || aws.ToString(list.Contents[0].Key) != "dir/key" {
		t.Fatalf("unexpected listing: %+v", list.Contents)
	}

	buckets, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets.Buckets) != 1 || aws.ToString(buckets.Buckets[0].Name) != "bucket" {
		t.Fatalf("unexpected buckets: %+v", buckets.Buckets)
	}

	if _, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("missing"),
	}); err == nil {
		t.Fatal("GetObject on missing key should fail")
	} else {
		var nsk *types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Fatalf("expected NoSuchKey, got %v", err)
		}
	}
}

func TestSDKWrongCredentials(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")

	bad := newSDKClient(t, ts, "AKID", "WRONG")
	if _, err := bad.ListBuckets(context.Background(), &s3.ListBucketsInput{}); err == nil {
		t.Fatal("wrong credentials accepted")
	}
}

func TestSDKMultipartEndToEnd(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")

	client := newSDKClient(t, ts, "AKID", "SECRET")
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("bucket"),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	init, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String("bucket"),
		Key:         aws.String("big"),
		ContentType: aws.String("text/plain"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(init.UploadId)
	if uploadID == "" {
		t.Fatal("empty upload id")
	}

	part1 := bytes.Repeat([]byte("a"), 100)
	part2 := bytes.Repeat([]byte("b"), 100)
	p1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String("bucket"),
		Key:        aws.String("big"),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(part1),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	p2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String("bucket"),
		Key:        aws.String("big"),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(2),
		Body:       bytes.NewReader(part2),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	parts, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String("bucket"),
		Key:      aws.String("big"),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts.Parts) != 2 {
		t.Fatalf("ListParts returned %d parts", len(parts.Parts))
	}

	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String("bucket"),
	})
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads.Uploads) != 1 || aws.ToString(uploads.Uploads[0].Key) != "big" {
		t.Fatalf("unexpected uploads: %+v", uploads.Uploads)
	}

	comp, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String("bucket"),
		Key:      aws.String("big"),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: p1.ETag},
				{PartNumber: aws.Int32(2), ETag: p2.ETag},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("big"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	data, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, part1...), part2...)
	if !bytes.Equal(data, want) {
		t.Fatalf("assembled body mismatch: %d bytes", len(data))
	}
	completeETag := aws.ToString(comp.ETag)
	getETag := aws.ToString(got.ETag)
	// S3 returns the full "digest-N" multipart ETag on GET/HEAD as well
	if strings.Trim(completeETag, `"`) != strings.Trim(getETag, `"`) {
		t.Fatalf("complete etag = %q, get etag = %q", completeETag, getETag)
	}

	uploads, err = client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String("bucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads.Uploads) != 0 {
		t.Fatalf("completed upload still listed: %+v", uploads.Uploads)
	}

	// abort flow
	init2, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("aborted"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String("bucket"),
		Key:        aws.String("aborted"),
		UploadId:   aws.String(aws.ToString(init2.UploadId)),
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(part1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String("bucket"),
		Key:      aws.String("aborted"),
		UploadId: aws.String(aws.ToString(init2.UploadId)),
	}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	uploads, err = client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String("bucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads.Uploads) != 0 {
		t.Fatalf("aborted upload still listed: %+v", uploads.Uploads)
	}
}

func TestSDKListObjectsPagination(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")
	client := newSDKClient(t, ts, "AKID", "SECRET")
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("bucket"),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("bucket"), Key: aws.String(k),
			Body: strings.NewReader(k),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", k, err)
		}
	}

	var got []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String("bucket"),
			MaxKeys:           aws.Int32(3),
			ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("ListObjectsV2: %v", err)
		}
		for _, o := range out.Contents {
			got = append(got, aws.ToString(o.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		if out.NextContinuationToken == nil {
			t.Fatal("truncated listing without NextContinuationToken")
		}
		token = out.NextContinuationToken
	}
	if want := "a,b,c,d,e,f,g"; strings.Join(got, ",") != want {
		t.Fatalf("paginated listing = %v, want %v", got, want)
	}
}

func TestSDKListObjectsPaginationMerged(t *testing.T) {
	ctx := context.Background()
	baseline := newMemStore()
	if err := baseline.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"r-a", "r-b", "r-c", "r-d"} {
		if _, err := baseline.Put(ctx, "bucket", k, strings.NewReader(k), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	handler := newS3Server(newOverlayStore(newMemStore(), baseline))
	handler = sigv4Middleware(handler, "AKID", "SECRET")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := newSDKClient(t, ts, "AKID", "SECRET")

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bucket")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for _, k := range []string{"l-a", "l-b", "l-c", "l-d"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("bucket"), Key: aws.String(k),
			Body: strings.NewReader(k),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", k, err)
		}
	}

	var got []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String("bucket"),
			MaxKeys:           aws.Int32(3),
			ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("ListObjectsV2: %v", err)
		}
		for _, o := range out.Contents {
			got = append(got, aws.ToString(o.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	if want := "l-a,l-b,l-c,l-d,r-a,r-b,r-c,r-d"; strings.Join(got, ",") != want {
		t.Fatalf("merged paginated listing = %v, want %v", got, want)
	}
}

func TestSDKProxyFallback(t *testing.T) {
	ctx := context.Background()
	baseline := newMemStore()
	if err := baseline.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.Put(ctx, "bucket", "proxy-key", strings.NewReader("remote-data"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	handler := newS3Server(newOverlayStore(newMemStore(), baseline))
	handler = sigv4Middleware(handler, "AKID", "SECRET")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := newSDKClient(t, ts, "AKID", "SECRET")

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bucket")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// GET falls back to the remote store
	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bucket"), Key: aws.String("proxy-key"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	data, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "remote-data" {
		t.Fatalf("proxy body = %q", data)
	}
	wantETag := `"` + etagOf([]byte("remote-data")) + `"`
	if etag := aws.ToString(got.ETag); etag != wantETag {
		t.Fatalf("proxy etag = %q, want %q", etag, wantETag)
	}

	// HEAD falls back too
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("bucket"), Key: aws.String("proxy-key"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len("remote-data")) {
		t.Fatalf("proxy head size = %d", aws.ToInt64(head.ContentLength))
	}
	if etag := aws.ToString(head.ETag); etag != wantETag {
		t.Fatalf("proxy head etag = %q, want %q", etag, wantETag)
	}

	// neither store has the key
	if _, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bucket"), Key: aws.String("missing"),
	}); err == nil {
		t.Fatal("GetObject on missing key should fail")
	} else {
		var nsk *types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Fatalf("expected NoSuchKey, got %v", err)
		}
	}
}

func TestSDKMissingBucket(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")
	client := newSDKClient(t, ts, "AKID", "SECRET")
	ctx := context.Background()

	assertAPIError := func(err error, wantCode string) {
		t.Helper()
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != wantCode {
			t.Fatalf("expected %s error, got %v", wantCode, err)
		}
	}

	_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("missing"),
	})
	assertAPIError(err, "NoSuchBucket")

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("missing"), Key: aws.String("key"),
		Body: strings.NewReader("x"),
	})
	assertAPIError(err, "NoSuchBucket")

	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("missing"), Key: aws.String("key"),
	})
	assertAPIError(err, "NotFound")
}
