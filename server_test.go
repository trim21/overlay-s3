package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"
)

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func signedRequest(t *testing.T, method, url, body, keyID, secret string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("x-amz-content-sha256", sha256hex(body))
	} else {
		req.Header.Set("x-amz-content-sha256", emptyPayloadHash)
	}
	signer := v4.NewSigner()
	creds := aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}
	if err := signer.SignHTTP(context.Background(), creds, req,
		req.Header.Get("x-amz-content-sha256"), "s3", "us-east-1",
		time.Now()); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSigV4ValidSignature(t *testing.T) {
	req := signedRequest(t, http.MethodPut,
		"http://127.0.0.1:8080/bucket/key?list-type=2", "hello",
		"AKID", "SECRET")
	if err := verifySigV4(req, "AKID", "SECRET"); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestSigV4WrongSecret(t *testing.T) {
	req := signedRequest(t, http.MethodPut,
		"http://127.0.0.1:8080/bucket/key", "hello", "AKID", "SECRET")
	if err := verifySigV4(req, "AKID", "OTHER-SECRET"); err == nil {
		t.Fatal("wrong secret accepted")
	}
}

func TestSigV4WrongAccessKey(t *testing.T) {
	req := signedRequest(t, http.MethodGet,
		"http://127.0.0.1:8080/bucket/key", "", "AKID", "SECRET")
	if err := verifySigV4(req, "OTHER-KEY", "SECRET"); err == nil {
		t.Fatal("wrong access key accepted")
	}
}

func TestSigV4TamperedBody(t *testing.T) {
	req := signedRequest(t, http.MethodPut,
		"http://127.0.0.1:8080/bucket/key", "hello", "AKID", "SECRET")
	// sign the request for one payload hash, then change the header
	req.Header.Set("x-amz-content-sha256", sha256hex("tampered"))
	if err := verifySigV4(req, "AKID", "SECRET"); err == nil {
		t.Fatal("tampered payload hash accepted")
	}
}

func TestSigV4StreamingSignature(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut,
		"http://127.0.0.1:8080/bucket/key", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-amz-content-sha256", streamingPayload)
	signer := v4.NewSigner()
	creds := aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	if err := signer.SignHTTP(context.Background(), creds, req,
		streamingPayload, "s3", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := verifySigV4(req, "AKID", "SECRET"); err != nil {
		t.Fatalf("streaming signature rejected: %v", err)
	}
}

// TestSigV4AuthorizationWithoutSpaces guards the parsing fix for minio-go's
// streaming signer, which emits "Credential=...,SignedHeaders=...,Signature=..."
// without spaces after the commas.
func TestSigV4AuthorizationWithoutSpaces(t *testing.T) {
	req := signedRequest(t, http.MethodPut,
		"http://127.0.0.1:8080/bucket/key", "hello", "AKID", "SECRET")
	req.Header.Set("Authorization", strings.ReplaceAll(
		req.Header.Get("Authorization"), ", ", ","))
	if err := verifySigV4(req, "AKID", "SECRET"); err != nil {
		t.Fatalf("space-less authorization rejected: %v", err)
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithAuth(t, "", "")
}

// newTestServerWithAuth builds a gateway over memstore overlay+baseline.
// baselineBuckets are pre-provisioned on the baseline store, mirroring the
// production assumption that baseline buckets always exist.
func newTestServerWithAuth(t *testing.T, key, secret string, baselineBuckets ...string) *httptest.Server {
	t.Helper()
	baseline := newMemStore()
	for _, b := range baselineBuckets {
		if err := baseline.CreateBucket(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	handler := newS3Server(newOverlayStore(newMemStore(), baseline))
	if key != "" {
		handler = sigv4Middleware(handler, key, secret)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func putObject(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func createBucket(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket status %d", resp.StatusCode)
	}
}

func TestServerPutGetHeadList(t *testing.T) {
	ts := newTestServer(t)
	createBucket(t, ts.URL+"/bucket")
	var err error

	resp := putObject(t, ts.URL+"/bucket/key", "hello")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if etag != `"`+etagOf([]byte("hello"))+`"` {
		t.Fatalf("unexpected etag %q", etag)
	}

	resp, err = http.Get(ts.URL + "/bucket/key")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(data) != "hello" {
		t.Fatalf("get: status %d body %q", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("content-type %q", ct)
	}

	resp, err = http.Head(ts.URL + "/bucket/key")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("head status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Length") != "5" {
		t.Fatalf("head content-length %q", resp.Header.Get("Content-Length"))
	}

	resp, err = http.Get(ts.URL + "/bucket/missing")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing key status %d", resp.StatusCode)
	}
}

func TestServerHeadObject(t *testing.T) {
	ts := newTestServer(t)
	createBucket(t, ts.URL+"/bucket")
	resp := putObject(t, ts.URL+"/bucket/key", "hello")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodHead, ts.URL+"/bucket/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("head status %d", hresp.StatusCode)
	}
	wantETag := `"` + etagOf([]byte("hello")) + `"`
	if hresp.Header.Get("ETag") != wantETag {
		t.Fatalf("head etag %q, want %q", hresp.Header.Get("ETag"), wantETag)
	}
	if hresp.Header.Get("Content-Length") != "5" {
		t.Fatalf("head content-length %q", hresp.Header.Get("Content-Length"))
	}
	if ct := hresp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("head content-type %q", ct)
	}
	if hresp.Header.Get("Last-Modified") == "" {
		t.Fatal("head missing Last-Modified")
	}

	req, err = http.NewRequest(http.MethodHead, ts.URL+"/bucket/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	mresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusNotFound {
		t.Fatalf("head missing status %d", mresp.StatusCode)
	}
}

func TestServerListObjectsV2(t *testing.T) {
	ts := newTestServerWithAuth(t, "", "", "bucket")
	createBucket(t, ts.URL+"/bucket")
	for _, k := range []string{"a", "dir/b", "dir/c"} {
		resp := putObject(t, ts.URL+"/bucket/"+k, k)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// no delimiter: all keys
	resp, err := http.Get(ts.URL + "/bucket?list-type=2")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(data)
	if !strings.Contains(body, "<Key>a</Key>") ||
		!strings.Contains(body, "<Key>dir/b</Key>") ||
		!strings.Contains(body, "<Key>dir/c</Key>") {
		t.Fatalf("list missing keys:\n%s", body)
	}

	// delimiter: common prefixes
	resp, err = http.Get(ts.URL + "/bucket?list-type=2&delimiter=/")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	body = string(data)
	if !strings.Contains(body, "<Key>a</Key>") ||
		!strings.Contains(body, "<Prefix>dir/</Prefix>") {
		t.Fatalf("delimiter grouping wrong:\n%s", body)
	}
}

// failingListStore stands in for a backend store that rejects a listing
// argument and surfaces the S3 API error it returned.
type failingListStore struct {
	Store
	err error
}

func (f failingListStore) ListPage(context.Context, string, ListParams) (ListPage, error) {
	return ListPage{}, f.err
}

// TestServerBackendAPIErrorBecomesClientError checks that a backend 4xx is
// reported to the client as itself: previously every backend API error except
// AccessDenied was logged and answered as a misleading 500 InternalError.
func TestServerBackendAPIErrorBecomesClientError(t *testing.T) {
	tests := []struct {
		backendCode string
		wantStatus  int
		wantCode    string
	}{
		{"XMinioInvalidObjectName", http.StatusBadRequest, "InvalidObjectName"},
		{"AccessDenied", http.StatusForbidden, "AccessDenied"},
		// an unmapped backend failure must stay visible, not be renamed
		{"SomethingElse", http.StatusInternalServerError, "InternalError"},
	}
	for _, tt := range tests {
		baseline := failingListStore{
			Store: newMemStore(),
			err:   &smithy.GenericAPIError{Code: tt.backendCode, Message: "backend refused"},
		}
		ts := httptest.NewServer(newS3Server(newOverlayStore(newMemStore(), baseline)))
		t.Cleanup(ts.Close)

		resp, err := http.Get(ts.URL + "/bucket?list-type=2&prefix=dir/")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("%s: status = %d, want %d:\n%s",
				tt.backendCode, resp.StatusCode, tt.wantStatus, data)
		}
		if want := "<Code>" + tt.wantCode + "</Code>"; !strings.Contains(string(data), want) {
			t.Errorf("%s: body = %s, want it to contain %s", tt.backendCode, data, want)
		}
	}
}

func TestServerListBuckets(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "<Name>mybucket</Name>") {
		t.Fatalf("bucket not listed:\n%s", data)
	}
}

func TestServerDeleteNotImplemented(t *testing.T) {
	ts := newTestServer(t)
	createBucket(t, ts.URL+"/bucket")
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/bucket/key", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
}

func TestServerSignedRequest(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")

	req := signedRequest(t, http.MethodPut, ts.URL+"/bucket", "", "AKID", "SECRET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed create bucket status %d", resp.StatusCode)
	}

	req = signedRequest(t, http.MethodPut, ts.URL+"/bucket/key", "hello",
		"AKID", "SECRET")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed put status %d", resp.StatusCode)
	}

	bad := signedRequest(t, http.MethodPut, ts.URL+"/bucket/key", "hello",
		"AKID", "WRONG")
	resp, err = http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned put status %d", resp.StatusCode)
	}
}

func TestServerPayloadHashMismatch(t *testing.T) {
	ts := newTestServerWithAuth(t, "AKID", "SECRET")

	req := signedRequest(t, http.MethodPut, ts.URL+"/bucket", "", "AKID", "SECRET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// signed for body "hello", then send a different body of the same
	// length: the signature still validates, the payload hash does not.
	req = signedRequest(t, http.MethodPut, ts.URL+"/bucket/key", "hello",
		"AKID", "SECRET")
	req.Body = io.NopCloser(strings.NewReader("HELLO"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("tampered payload accepted")
	}

	// the object must not exist afterwards
	req = signedRequest(t, http.MethodGet, ts.URL+"/bucket/key", "", "AKID", "SECRET")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("tampered payload was written, status %d", resp.StatusCode)
	}
}
