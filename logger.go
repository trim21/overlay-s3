package main

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func init() {
	zlog.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
	}).With().Timestamp().Logger()
}
