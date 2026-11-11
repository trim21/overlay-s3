package main

import (
	_ "embed"
	"io"
	"net/http"
)

// readmeMarkdown is the repository README, which carries the configuration
// reference. Embedding it keeps the served documentation identical to the
// documentation in the repository.
//
//go:embed README.md
var readmeMarkdown string

// docPath is outside the S3 namespace on purpose: bucket names cannot contain
// underscores, so it can never collide with a bucket or object request.
const docPath = "/_overlay-s3"

// withDocs serves the embedded README as markdown at docPath and passes every
// other request through. It wraps the outermost handler, so documentation is
// reachable without client credentials even when signature checks are on.
func withDocs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == docPath && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = io.WriteString(w, readmeMarkdown)
			return
		}
		next.ServeHTTP(w, r)
	})
}
