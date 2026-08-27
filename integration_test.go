package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// These tests exercise the overlay semantics against a real S3 backend.
// They are skipped unless S3_TEST_ENDPOINT, S3_TEST_ACCESS_KEY and
// S3_TEST_SECRET_KEY are set (the CI workflow provides them via a silo
// container). One backend plays both roles: client-visible buckets map
// one-to-one for the baseline store, while the overlay store nests
// everything under prefix/bucket/key inside a dedicated physical bucket.

func integrationEnv(t *testing.T) (endpoint, accessKey, secretKey, region string) {
	t.Helper()
	endpoint = os.Getenv("S3_TEST_ENDPOINT")
	accessKey = os.Getenv("S3_TEST_ACCESS_KEY")
	secretKey = os.Getenv("S3_TEST_SECRET_KEY")
	region = os.Getenv("S3_TEST_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("S3_TEST_ENDPOINT/ACCESS_KEY/SECRET_KEY not set; skipping integration test")
	}
	return endpoint, accessKey, secretKey, region
}

type integrationStores struct {
	seed     *s3.Client
	overlay  *s3Store
	baseline *s3Store

	baselineBucket string
	overlayBucket  string
	overlayPrefix  string
}

func newIntegrationStores(t *testing.T) *integrationStores {
	t.Helper()
	endpoint, accessKey, secretKey, region := integrationEnv(t)
	ctx := context.Background()

	s := &integrationStores{
		seed:           newSDKClientForEndpoint(t, endpoint, accessKey, secretKey, region),
		baselineBucket: fmt.Sprintf("overlay-it-base-%d", time.Now().UnixNano()),
		overlayBucket:  fmt.Sprintf("overlay-it-ov-%d", time.Now().UnixNano()),
		overlayPrefix:  "gw",
	}

	var err error
	s.overlay, err = newMappedStore(ctx, endpoint, region, accessKey, secretKey,
		s.overlayBucket, s.overlayPrefix, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.overlay.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket overlay: %v", err)
	}
	if _, err := s.seed.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.baselineBucket),
	}); err != nil {
		t.Fatalf("CreateBucket baseline: %v", err)
	}
	s.baseline, err = newRemoteStore(ctx, endpoint, region, accessKey, secretKey, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, bucket := range []string{s.baselineBucket, s.overlayBucket} {
			purgeBucket(t, s.seed, bucket)
		}
	})
	return s
}

func purgeBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	var token *string
	for {
		list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), ContinuationToken: token,
		})
		if err != nil {
			return
		}
		for _, o := range list.Contents {
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket), Key: o.Key,
			})
		}
		if !aws.ToBool(list.IsTruncated) {
			break
		}
		token = list.NextContinuationToken
	}
	_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
}

func (s *integrationStores) startGateway(t *testing.T) *s3.Client {
	t.Helper()
	handler := newS3Server(newOverlayStore(s.overlay, s.baseline))
	handler = sigv4Middleware(handler, "test", "test-secret")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return newSDKClientForEndpoint(t, ts.URL, "test", "test-secret", "us-east-1")
}

func readBody(t *testing.T, out *s3.GetObjectOutput) string {
	t.Helper()
	data, err := io.ReadAll(out.Body)
	out.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustGet(t *testing.T, client *s3.Client, bucket, key string) string {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	return readBody(t, out)
}

func TestIntegrationOverlayAgainstRealS3(t *testing.T) {
	ctx := context.Background()
	s := newIntegrationStores(t)

	// seed the baseline with pre-existing data, as if it had been used
	// before the overlay was introduced
	for key, data := range map[string]string{
		"remote-only": "remote-data",
		"dir/remote":  "nested-remote",
		"shared":      "remote-shared",
	} {
		if _, err := s.seed.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.baselineBucket), Key: aws.String(key),
			Body: strings.NewReader(data),
		}); err != nil {
			t.Fatalf("seed PutObject %s: %v", key, err)
		}
	}
	// a second, baseline-only bucket exercises the bucket-listing union
	other := fmt.Sprintf("overlay-it-other-%d", time.Now().UnixNano())
	if _, err := s.seed.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(other)}); err != nil {
		t.Fatalf("CreateBucket other: %v", err)
	}
	t.Cleanup(func() { purgeBucket(t, s.seed, other) })
	if _, err := s.seed.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(other), Key: aws.String("in-other"),
		Body: strings.NewReader("other-data"),
	}); err != nil {
		t.Fatalf("PutObject other: %v", err)
	}

	client := s.startGateway(t)

	// create-bucket writes a directory marker under the overlay prefix
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.baselineBucket),
	}); err != nil {
		t.Fatalf("gateway CreateBucket: %v", err)
	}
	markerKey := s.overlayPrefix + "/" + s.baselineBucket + "/"
	if _, err := s.seed.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.overlayBucket), Key: aws.String(markerKey),
	}); err != nil {
		t.Fatalf("bucket marker missing at %s: %v", markerKey, err)
	}

	// 1. reads fall back to the baseline while the overlay is empty
	if got := mustGet(t, client, s.baselineBucket, "remote-only"); got != "remote-data" {
		t.Fatalf("remote fallback read = %q", got)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("dir/remote"),
	})
	if err != nil {
		t.Fatalf("HeadObject dir/remote: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len("nested-remote")) {
		t.Fatalf("head size = %d", aws.ToInt64(head.ContentLength))
	}
	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.baselineBucket),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != 3 {
		t.Fatalf("seeded listing = %d objects, want 3", len(list.Contents))
	}

	// 2. writes land in the overlay and shadow the baseline
	for key, data := range map[string]string{
		"shared":    "local-shared",
		"new-local": "local-new",
	} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.baselineBucket), Key: aws.String(key),
			Body: strings.NewReader(data),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	if got := mustGet(t, client, s.baselineBucket, "shared"); got != "local-shared" {
		t.Fatalf("overlay should shadow baseline, got %q", got)
	}

	// 3. the baseline is untouched; the write landed under the overlay
	// prefix instead
	if got := mustGet(t, s.seed, s.baselineBucket, "shared"); got != "remote-shared" {
		t.Fatalf("baseline was modified, got %q", got)
	}
	if got := mustGet(t, s.seed, s.overlayBucket,
		s.overlayPrefix+"/"+s.baselineBucket+"/shared"); got != "local-shared" {
		t.Fatalf("overlay prefix mapping broken, got %q", got)
	}

	// 4. listings merge: 4 keys, overlay shadows shared
	list, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.baselineBucket),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2 after writes: %v", err)
	}
	seen := map[string]string{}
	for _, o := range list.Contents {
		seen[aws.ToString(o.Key)] = aws.ToString(o.ETag)
	}
	if len(seen) != 4 {
		t.Fatalf("merged listing = %d objects, want 4: %v", len(seen), seen)
	}
	overlayShared, err := s.seed.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.overlayBucket),
		Key:    aws.String(s.overlayPrefix + "/" + s.baselineBucket + "/shared"),
	})
	if err != nil {
		t.Fatalf("HeadObject overlay shared: %v", err)
	}
	if etag, ok := seen["shared"]; !ok || etag != aws.ToString(overlayShared.ETag) {
		t.Fatalf("shared should carry the overlay etag, got %q want %q", etag, aws.ToString(overlayShared.ETag))
	}
	if _, ok := seen["remote-only"]; !ok {
		t.Fatalf("remote-only missing from merged listing")
	}

	// 5. bucket listings are the union of both stores
	buckets, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	names := map[string]bool{}
	for _, b := range buckets.Buckets {
		names[aws.ToString(b.Name)] = true
	}
	if !names[s.baselineBucket] || !names[other] {
		t.Fatalf("expected %s and %s in bucket list, got %v",
			s.baselineBucket, other, names)
	}
	if got := mustGet(t, client, other, "in-other"); got != "other-data" {
		t.Fatalf("baseline-only bucket read = %q", got)
	}

	// 6. multipart uploads proxy into the overlay namespace
	init, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("multi"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	// Silo/MinIO (like AWS) rejects CompleteMultipartUpload when any
	// non-final part is smaller than 5 MiB.
	part1 := bytes.Repeat([]byte("a"), 5*1024*1024)
	part2 := bytes.Repeat([]byte("b"), 100)
	p1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("multi"),
		UploadId: init.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader(part1),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	p2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("multi"),
		UploadId: init.UploadId, PartNumber: aws.Int32(2),
		Body: bytes.NewReader(part2),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("multi"),
		UploadId: init.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: p1.ETag},
				{PartNumber: aws.Int32(2), ETag: p2.ETag},
			},
		},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	want := append(append([]byte{}, part1...), part2...)
	if got := mustGet(t, client, s.baselineBucket, "multi"); got != string(want) {
		t.Fatalf("multipart assembled body mismatch: %d bytes", len(got))
	}
	// the completed multipart object lives under the overlay prefix
	if got := mustGet(t, s.seed, s.overlayBucket,
		s.overlayPrefix+"/"+s.baselineBucket+"/multi"); got != string(want) {
		t.Fatalf("multipart object not stored under overlay prefix")
	}

	// 7. keys that exist nowhere are 404 (NoSuchKey)
	if _, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("no-such-key"),
	}); err == nil {
		t.Fatal("GetObject on missing key should fail")
	} else {
		var nsk *types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Fatalf("expected NoSuchKey, got %v", err)
		}
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.baselineBucket), Key: aws.String("no-such-key"),
	}); err == nil {
		t.Fatal("HeadObject on missing key should fail")
	}
}

func TestIntegrationListPagination(t *testing.T) {
	ctx := context.Background()
	s := newIntegrationStores(t)

	// seed more than one backend page (1000) so s3Store.List must walk
	// continuation tokens
	const n = 1001
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%05d", i)
		if _, err := s.seed.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.baselineBucket), Key: aws.String(key),
			Body: strings.NewReader(key),
		}); err != nil {
			t.Fatalf("seed PutObject %s: %v", key, err)
		}
	}

	client := s.startGateway(t)

	var got []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.baselineBucket),
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
	if len(got) != n {
		t.Fatalf("merged listing = %d objects, want %d", len(got), n)
	}
	for i, want := range got {
		if want != fmt.Sprintf("key-%05d", i) {
			t.Fatalf("listing[%d] = %q, want %q", i, want, fmt.Sprintf("key-%05d", i))
		}
	}
}
