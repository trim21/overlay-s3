package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	zlog "github.com/rs/zerolog/log"
)

func main() {
	var (
		listen = flag.String("listen", ":8080", "HTTP listen address")

		overlayEndpoint  = flag.String("overlay-endpoint", "", "overlay S3 endpoint receiving all writes (required)")
		overlayRegion    = flag.String("overlay-region", "us-east-1", "overlay S3 region")
		overlayAccessKey = flag.String("overlay-access-key", "", "overlay S3 access key")
		overlaySecretKey = flag.String("overlay-secret-key", "", "overlay S3 secret key")
		overlayBucket    = flag.String("overlay-bucket", "", "physical bucket in the overlay S3 holding all data")
		overlayPrefix    = flag.String("overlay-prefix", "", "key prefix inside the overlay bucket; client bucket b and key k map to prefix/b/k")
		overlayPathStyle = flag.Bool("overlay-path-style", true, "use path-style addressing for the overlay S3 endpoint")

		baselineEndpoint  = flag.String("baseline-endpoint", "", "baseline S3 endpoint for read fallback (empty uses AWS)")
		baselineRegion    = flag.String("baseline-region", "us-east-1", "baseline S3 region")
		baselineAccessKey = flag.String("baseline-access-key", "", "baseline S3 access key")
		baselineSecretKey = flag.String("baseline-secret-key", "", "baseline S3 secret key")
		baselinePathStyle = flag.Bool("baseline-path-style", true, "use path-style addressing for the baseline S3 endpoint")

		authKey    = flag.String("auth-key", "", "access key clients must sign with (empty disables signature checks)")
		authSecret = flag.String("auth-secret", "", "secret key clients must sign with")
	)
	flag.Parse()

	if *overlayEndpoint == "" || *overlayBucket == "" ||
		*overlayAccessKey == "" || *overlaySecretKey == "" {
		fmt.Fprintln(os.Stderr,
			"-overlay-endpoint, -overlay-bucket, -overlay-access-key and -overlay-secret-key are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	overlay, err := newMappedStore(ctx, *overlayEndpoint, *overlayRegion,
		*overlayAccessKey, *overlaySecretKey, *overlayBucket, *overlayPrefix, *overlayPathStyle)
	if err != nil {
		zlog.Fatal().Err(err).Msg("create overlay store")
	}
	if err := overlay.EnsureBucket(ctx); err != nil {
		zlog.Fatal().Err(err).Str("bucket", *overlayBucket).Msg("ensure overlay bucket")
	}
	baseline, err := newRemoteStore(ctx, *baselineEndpoint, *baselineRegion,
		*baselineAccessKey, *baselineSecretKey, *baselinePathStyle)
	if err != nil {
		zlog.Fatal().Err(err).Msg("create baseline store")
	}

	backend := newOverlayStore(overlay, baseline)
	handler := newS3Server(backend)
	if *authKey != "" {
		handler = sigv4Middleware(handler, *authKey, *authSecret)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		baselineLabel := *baselineEndpoint
		if baselineLabel == "" {
			baselineLabel = "aws"
		}
		zlog.Info().Msgf("overlay-s3 listening on %s (overlay=%s/%s%s baseline=%s)",
			*listen, *overlayEndpoint, *overlayBucket, *overlayPrefix, baselineLabel)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			zlog.Fatal().Err(err).Msg("server")
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
