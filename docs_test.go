package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsEndpointServesReadme(t *testing.T) {
	router, _ := newTestRouter(t)
	ts := httptest.NewServer(withDocs(newS3Server(router)))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + docPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(string(body), "[buckets.demo.overlay]") {
		t.Fatalf("docs do not carry the configuration reference:\n%s", body)
	}

	// the same server still answers the S3 service call at /
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<Name>alpha</Name>") {
		t.Fatalf("GET / is no longer ListBuckets:\n%s", body)
	}
}

func TestDocsEndpointNeedsNoCredentials(t *testing.T) {
	router, _ := newTestRouter(t)
	handler := withDocs(sigv4Middleware(newS3Server(router), "AKID", "SECRET"))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + docPath)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unsigned docs request = %d, want 200", resp.StatusCode)
	}
}
