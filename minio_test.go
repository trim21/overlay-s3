package main

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func newMinioClient(t *testing.T, ts *httptest.Server, key, secret string) *minio.Client {
	t.Helper()
	client, err := minio.New(strings.TrimPrefix(ts.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4(key, secret, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestMinioClientStreamingUpload reproduces the mc upload path that failed in
// CI: minio-go streams even small PUTs with STREAMING-AWS4-HMAC-SHA256-PAYLOAD
// and emits the Authorization header without spaces after commas. gofakes3
// decodes the aws-chunked body; the gateway must accept the signature.
func TestMinioClientStreamingUpload(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")
	client := newMinioClient(t, ts, "AKID", "SECRET")
	ctx := context.Background()

	if err := client.MakeBucket(ctx, "bucket", minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	payload := strings.Repeat("hello-minio-streaming-", 500) // ~11KiB, single PUT
	if _, err := client.PutObject(ctx, "bucket", "streamed",
		strings.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	obj, err := client.GetObject(ctx, "bucket", "streamed", minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(obj)
	obj.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(data), len(payload))
	}
}
