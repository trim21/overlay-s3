package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestRouter builds a router over two independent memstore-backed buckets.
func newTestRouter(t *testing.T) (*bucketRouter, map[string]*memStore) {
	t.Helper()
	ctx := context.Background()
	baselines := map[string]*memStore{}
	stores := map[string]*overlayStore{}
	for _, name := range []string{"alpha", "beta"} {
		baseline := newMemStore()
		if err := baseline.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
		overlay := newMemStore()
		if err := overlay.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
		baselines[name] = baseline
		stores[name] = newOverlayStore(overlay, baseline)
	}
	return newBucketRouter(stores), baselines
}

func TestRouterServesEachBucketFromItsOwnStore(t *testing.T) {
	ctx := context.Background()
	router, baselines := newTestRouter(t)

	if _, err := baselines["alpha"].Put(ctx, "alpha", "from-alpha", strings.NewReader("a"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := router.Get(ctx, "alpha", "from-alpha", nil); err != nil {
		t.Fatalf("read through alpha: %v", err)
	}
	// beta has its own stores: alpha's baseline object must not leak into it
	if _, _, err := router.Get(ctx, "beta", "from-alpha", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("beta read = %v, want ErrNotFound", err)
	}

	if _, err := router.Put(ctx, "beta", "local", strings.NewReader("b"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := baselines["alpha"].Head(ctx, "alpha", "local"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("beta write reached alpha's backend: %v", err)
	}
}

func TestRouterUnknownBucket(t *testing.T) {
	ctx := context.Background()
	router, _ := newTestRouter(t)

	if _, err := router.Put(ctx, "unknown", "key", strings.NewReader("x"), "text/plain"); !errors.Is(err, errBucketNotConfigured) {
		t.Fatalf("Put error = %v, want errBucketNotConfigured", err)
	}
	if _, err := router.ListPage(ctx, "unknown", ListParams{}); !errors.Is(err, errBucketNotConfigured) {
		t.Fatalf("ListPage error = %v, want errBucketNotConfigured", err)
	}
	if err := router.CreateBucket(ctx, "unknown"); !errors.Is(err, errBucketNotConfigured) {
		t.Fatalf("CreateBucket error = %v, want errBucketNotConfigured", err)
	}
	if _, err := router.InitiateMultipart(ctx, "unknown", "key", "text/plain"); !errors.Is(err, errBucketNotConfigured) {
		t.Fatalf("InitiateMultipart error = %v, want errBucketNotConfigured", err)
	}
}

func TestRouterListsConfiguredBuckets(t *testing.T) {
	ctx := context.Background()
	router, _ := newTestRouter(t)

	names, err := router.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "alpha,beta" {
		t.Fatalf("buckets = %q, want alpha,beta", got)
	}
	for _, name := range names {
		ok, err := router.BucketExists(ctx, name)
		if err != nil || !ok {
			t.Fatalf("BucketExists(%q) = %v, %v", name, ok, err)
		}
	}
	if ok, err := router.BucketExists(ctx, "unknown"); err != nil || ok {
		t.Fatalf("BucketExists(unknown) = %v, %v", ok, err)
	}
}

func TestRouterListMergesOverlayAndBaseline(t *testing.T) {
	ctx := context.Background()
	router, baselines := newTestRouter(t)

	if _, err := baselines["alpha"].Put(ctx, "alpha", "baseline-key", strings.NewReader("b"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Put(ctx, "alpha", "overlay-key", strings.NewReader("o"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	page, err := router.ListPage(ctx, "alpha", ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range page.Objects {
		keys = append(keys, o.Key)
	}
	if got := strings.Join(keys, ","); got != "baseline-key,overlay-key" {
		t.Fatalf("keys = %q", got)
	}

	body, meta, err := router.Get(ctx, "alpha", "baseline-key", nil)
	if err != nil {
		t.Fatalf("baseline fallback read: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "b" || meta.Size != 1 {
		t.Fatalf("read = %q size %d", data, meta.Size)
	}
}

func TestRouterUnknownBucketOverHTTP(t *testing.T) {
	router, _ := newTestRouter(t)
	ts := httptest.NewServer(newS3Server(router))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/unknown?list-type=2")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "<Code>NoSuchBucket</Code>") {
		t.Fatalf("listing unknown bucket = %d:\n%s", resp.StatusCode, body)
	}

	for bucket, want := range map[string]int{"alpha": http.StatusOK, "unknown": http.StatusNotFound} {
		req, err := http.NewRequest(http.MethodHead, ts.URL+"/"+bucket, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("HEAD %s = %d, want %d", bucket, resp.StatusCode, want)
		}
	}
}
