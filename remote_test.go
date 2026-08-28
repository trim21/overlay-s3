package main

import (
	"strings"
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
