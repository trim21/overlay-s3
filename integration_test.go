package main

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/johannesboyne/gofakes3"
)

// These tests exercise the overlay semantics against a real S3 backend.
// They are skipped unless S3_TEST_ENDPOINT, S3_TEST_ACCESS_KEY and
// S3_TEST_SECRET_KEY are set (the CI workflow provides them via a silo
// container). The tests seed the backend with "existing" data first, then
// verify that reads fall back to the remote store, writes land only in the
// local overlay, and listings merge both.

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

func newRemoteClient(t *testing.T, endpoint, accessKey, secretKey, region string) *s3.Client {
	t.Helper()
	return newSDKClientForEndpoint(t, endpoint, accessKey, secretKey, region)
}

func TestIntegrationOverlayAgainstRealS3(t *testing.T) {
	endpoint, accessKey, secretKey, region := integrationEnv(t)
	ctx := context.Background()

	seed := newRemoteClient(t, endpoint, accessKey, secretKey, region)
	bucket := fmt.Sprintf("overlay-it-%d", time.Now().UnixNano())
	if _, err := seed.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		list, err := seed.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err == nil {
			for _, o := range list.Contents {
				_, _ = seed.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(bucket), Key: o.Key,
				})
			}
		}
		_, _ = seed.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})

	// seed the backend with pre-existing data, as if the remote had been
	// used before the overlay was introduced
	seedData := map[string]string{
		"remote-only": "remote-data",
		"dir/remote":  "nested-remote",
		"shared":      "remote-shared",
	}
	for key, data := range seedData {
		if _, err := seed.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader(data),
		}); err != nil {
			t.Fatalf("seed PutObject %s: %v", key, err)
		}
	}

	// build the gateway: empty local overlay on top of the real backend
	remote, err := newRemoteStore(ctx, endpoint, region, accessKey, secretKey)
	if err != nil {
		t.Fatal(err)
	}
	local, err := newLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := gofakes3.New(newOverlayBackend(
		newOverlayStore(local, remote))).Server()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := newSDKClientForEndpoint(t, ts.URL, "test", "test", region)

	// 1. reads fall back to the remote store while the overlay is empty
	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("remote-only"),
	})
	if err != nil {
		t.Fatalf("GetObject remote-only: %v", err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(data) != "remote-data" {
		t.Fatalf("remote fallback read = %q", data)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("dir/remote"),
	})
	if err != nil {
		t.Fatalf("HeadObject dir/remote: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len("nested-remote")) {
		t.Fatalf("head size = %d", aws.ToInt64(head.ContentLength))
	}

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != len(seedData) {
		t.Fatalf("seeded listing = %d objects, want %d", len(list.Contents), len(seedData))
	}

	// 2. writes land in the local overlay and shadow the remote
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("shared"),
		Body: strings.NewReader("local-shared"),
	}); err != nil {
		t.Fatalf("PutObject shared: %v", err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("new-local"),
		Body: strings.NewReader("local-new"),
	}); err != nil {
		t.Fatalf("PutObject new-local: %v", err)
	}

	got, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("shared"),
	})
	if err != nil {
		t.Fatalf("GetObject shared: %v", err)
	}
	data, _ = io.ReadAll(got.Body)
	got.Body.Close()
	if string(data) != "local-shared" {
		t.Fatalf("local overlay should shadow remote, got %q", data)
	}

	// 3. the remote backend is untouched: writes never propagate
	direct, err := seed.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("shared"),
	})
	if err != nil {
		t.Fatalf("direct GetObject shared: %v", err)
	}
	data, _ = io.ReadAll(direct.Body)
	direct.Body.Close()
	if string(data) != "remote-shared" {
		t.Fatalf("remote backend was modified, got %q", data)
	}

	// 4. listings merge: 4 keys, local shadows shared
	list, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
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
	if etag, ok := seen["shared"]; !ok || etag != `"`+etagOf([]byte("local-shared"))+`"` {
		t.Fatalf("shared should carry the local etag, got %q", etag)
	}
	if _, ok := seen["remote-only"]; !ok {
		t.Fatalf("remote-only missing from merged listing")
	}

	// 5. local state lives on disk under the overlay directory: a read
	// against the same local store still resolves locally
	got, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("new-local"),
	})
	if err != nil {
		t.Fatalf("GetObject new-local after overlay rebuild: %v", err)
	}
	data, _ = io.ReadAll(got.Body)
	got.Body.Close()
	if string(data) != "local-new" {
		t.Fatalf("local overlay state lost, got %q", data)
	}

	// 6. multipart upload completes into the local overlay
	init, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("multi"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	part1 := bytes.Repeat([]byte("a"), 100)
	part2 := bytes.Repeat([]byte("b"), 100)
	p1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String("multi"),
		UploadId: init.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader(part1),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	p2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String("multi"),
		UploadId: init.UploadId, PartNumber: aws.Int32(2),
		Body: bytes.NewReader(part2),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}
	comp, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("multi"),
		UploadId: init.UploadId,
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
	want := append(append([]byte{}, part1...), part2...)
	b1, _ := hex.DecodeString(strings.Trim(aws.ToString(p1.ETag), `"`))
	b2, _ := hex.DecodeString(strings.Trim(aws.ToString(p2.ETag), `"`))
	concat := append(b1, b2...)
	sum := md5sum(concat)
	wantETag := hex.EncodeToString(sum) + "-2"
	if strings.Trim(aws.ToString(comp.ETag), `"`) != wantETag {
		t.Fatalf("multipart etag = %q, want %q", aws.ToString(comp.ETag), wantETag)
	}
	got, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("multi"),
	})
	if err != nil {
		t.Fatalf("GetObject multi: %v", err)
	}
	data, _ = io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(data, want) {
		t.Fatalf("multipart assembled body mismatch: %d bytes", len(data))
	}
}

func TestIntegrationListPagination(t *testing.T) {
	endpoint, accessKey, secretKey, region := integrationEnv(t)
	ctx := context.Background()

	seed := newRemoteClient(t, endpoint, accessKey, secretKey, region)
	bucket := fmt.Sprintf("overlay-pag-%d", time.Now().UnixNano())
	if _, err := seed.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		var token *string
		for {
			list, err := seed.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(bucket), ContinuationToken: token,
			})
			if err != nil {
				return
			}
			for _, o := range list.Contents {
				_, _ = seed.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(bucket), Key: o.Key,
				})
			}
			if !aws.ToBool(list.IsTruncated) {
				break
			}
			token = list.NextContinuationToken
		}
		_, _ = seed.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})

	// seed more than one backend page (1000) so remoteStore.List must walk
	// continuation tokens
	const n = 1001
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%05d", i)
		if _, err := seed.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader(key),
		}); err != nil {
			t.Fatalf("seed PutObject %s: %v", key, err)
		}
	}

	remote, err := newRemoteStore(ctx, endpoint, region, accessKey, secretKey)
	if err != nil {
		t.Fatal(err)
	}
	local, err := newLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := gofakes3.New(newOverlayBackend(
		newOverlayStore(local, remote))).Server()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := newSDKClientForEndpoint(t, ts.URL, "test", "test", region)

	var got []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
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
		if k := fmt.Sprintf("key-%05d", i); want != k {
			t.Fatalf("listing[%d] = %q, want %q", i, want, k)
		}
	}
}
