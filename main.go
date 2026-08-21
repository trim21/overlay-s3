package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"
)

func main() {
	var (
		listen          = flag.String("listen", ":8080", "HTTP listen address")
		localDir        = flag.String("local-dir", "./data", "local overlay storage directory")
		remoteEndpoint  = flag.String("remote-endpoint", "", "remote S3 endpoint (empty uses AWS)")
		remoteRegion    = flag.String("remote-region", "us-east-1", "remote S3 region")
		remoteAccessKey = flag.String("remote-access-key", "", "remote S3 access key")
		remoteSecretKey = flag.String("remote-secret-key", "", "remote S3 secret key")
		authKey         = flag.String("auth-key", "", "access key clients must sign with (empty disables signature checks)")
		authSecret      = flag.String("auth-secret", "", "secret key clients must sign with")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	local, err := newLocalStore(*localDir)
	if err != nil {
		log.Fatalf("local store: %v", err)
	}
	remote, err := newRemoteStore(ctx, *remoteEndpoint, *remoteRegion,
		*remoteAccessKey, *remoteSecretKey)
	if err != nil {
		log.Fatalf("remote store: %v", err)
	}

	backend := newOverlayBackend(newOverlayStore(local, remote))
	handler := gofakes3.New(backend).Server()
	if *authKey != "" {
		handler = sigv4Middleware(handler, *authKey, *authSecret)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		remoteLabel := *remoteEndpoint
		if remoteLabel == "" {
			remoteLabel = "aws"
		}
		log.Printf("overlay-s3 listening on %s (local=%s remote=%s)",
			*listen, *localDir, remoteLabel)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
