package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestBackendPrefix(t *testing.T) {
	mapped := &s3Store{mappedBucket: "overlay-data", mappedPrefix: "gateway"}
	rootless := &s3Store{mappedBucket: "overlay-data"}
	baseline := &s3Store{}

	tests := []struct {
		name   string
		store  *s3Store
		bucket string
		prefix string
		want   string
	}{
		{"overlay bucket root", mapped, "res-to-hs", "", "gateway/res-to-hs/"},
		{"overlay nested prefix", mapped, "res-to-hs", "users/niuziru/sql/", "gateway/res-to-hs/users/niuziru/sql/"},
		{"overlay prefix without trailing slash", mapped, "res-to-hs", "users/niuziru/sql", "gateway/res-to-hs/users/niuziru/sql"},
		{"overlay without mapping prefix", rootless, "res-to-hs", "dir/", "res-to-hs/dir/"},
		{"baseline bucket root", baseline, "bucket", "", ""},
		{"baseline passthrough", baseline, "bucket", "users/niuziru/sql/", "users/niuziru/sql/"},
	}
	for _, tt := range tests {
		got := tt.store.backendPrefix(tt.bucket, tt.prefix)
		if got != tt.want {
			t.Errorf("%s: backendPrefix(%q, %q) = %q, want %q",
				tt.name, tt.bucket, tt.prefix, got, tt.want)
		}
		// silo and MinIO reject a listing prefix containing "//" with
		// XMinioInvalidObjectName, so a prefixed listing could never reach
		// the backend at all
		if strings.Contains(got, "//") {
			t.Errorf("%s: backendPrefix = %q contains %q", tt.name, got, "//")
		}
	}
}

// headerRecorder records one request header seen by a fake backend.
type headerRecorder struct {
	mu    sync.Mutex
	value string
}

func (h *headerRecorder) set(v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.value = v
}

func (h *headerRecorder) get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value
}

func mustRemoteStore(t *testing.T, endpoint string) *s3Store {
	t.Helper()
	s, err := newRemoteStore(context.Background(), endpoint, "us-east-1", "ak", "sk", true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestGetPassesRangeToBackend checks a resolved range is sent to the backend
// as a Range header and that the 206 slice comes back untouched.
func TestGetPassesRangeToBackend(t *testing.T) {
	var seen headerRecorder
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.set(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Range", "bytes 5-8/20")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "5678")
	}))
	t.Cleanup(ts.Close)

	rc, meta, err := mustRemoteStore(t, ts.URL).
		Get(context.Background(), "bucket", "key", &ByteRange{Start: 5, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := "bytes=5-8"; seen.get() != want {
		t.Errorf("backend received Range %q, want %q", seen.get(), want)
	}
	if got := readAll(t, rc); got != "5678" {
		t.Errorf("body = %q, want %q", got, "5678")
	}
	if meta.Size != 4 {
		t.Errorf("meta.Size = %d, want 4", meta.Size)
	}
}

// TestGetSlicesWhenBackendIgnoresRange covers a backend that answers a ranged
// GET with the whole object and no Content-Range: passing that body through
// would serve the caller bytes it never asked for.
func TestGetSlicesWhenBackendIgnoresRange(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "0123456789ABCDEFGHIJ")
	}))
	t.Cleanup(ts.Close)

	rc, meta, err := mustRemoteStore(t, ts.URL).
		Get(context.Background(), "bucket", "key", &ByteRange{Start: 5, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "5678" {
		t.Errorf("body = %q, want %q", got, "5678")
	}
	if meta.Size != 4 {
		t.Errorf("meta.Size = %d, want 4", meta.Size)
	}
}
