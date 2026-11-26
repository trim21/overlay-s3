package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	zlog "github.com/rs/zerolog/log"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to the TOML configuration file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	stores, err := buildBucketStores(ctx, cfg)
	if err != nil {
		zlog.Fatal().Err(err).Msg("create bucket stores")
	}

	handler := newS3Server(newBucketRouter(stores))
	if cfg.AuthKey != "" {
		handler = sigv4Middleware(handler, cfg.AuthKey, cfg.AuthSecret)
	}
	handler = withDocs(handler)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		zlog.Info().Strs("buckets", sortedKeys(stores)).
			Msgf("overlay-s3 listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			zlog.Fatal().Err(err).Msg("server")
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// buildBucketStores connects the overlay and baseline backend of every
// configured bucket. Each bucket is independent: it may live on different S3
// endpoints, and buckets sharing one physical overlay bucket stay separated by
// their own key prefix.
func buildBucketStores(ctx context.Context, cfg *config) (map[string]*overlayStore, error) {
	stores := make(map[string]*overlayStore, len(cfg.Buckets))
	for _, name := range sortedKeys(cfg.Buckets) {
		b := cfg.Buckets[name]
		overlay, err := newMappedStore(ctx,
			b.Overlay.Endpoint, regionOr(b.Overlay.Region),
			b.Overlay.AccessKey, b.Overlay.SecretKey,
			b.Overlay.Bucket, b.Overlay.Prefix, pathStyle(b.Overlay.PathStyle))
		if err != nil {
			return nil, fmt.Errorf("bucket %q overlay: %w", name, err)
		}
		if err := overlay.EnsureBucket(ctx); err != nil {
			return nil, fmt.Errorf("bucket %q: ensure overlay bucket %q: %w",
				name, b.Overlay.Bucket, err)
		}
		baseline, err := newRemoteStore(ctx,
			b.Baseline.Endpoint, regionOr(b.Baseline.Region),
			b.Baseline.AccessKey, b.Baseline.SecretKey, pathStyle(b.Baseline.PathStyle))
		if err != nil {
			return nil, fmt.Errorf("bucket %q baseline: %w", name, err)
		}
		stores[name] = newOverlayStore(overlay, baseline)
	}
	return stores, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
