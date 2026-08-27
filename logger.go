package main

import (
	"fmt"
	"os"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func init() {
	zlog.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
	}).With().Timestamp().Logger()
}

// gofakes3Log adapts gofakes3's Logger interface to zerolog. Only error
// level messages are forwarded; the rest are dropped.
type gofakes3Log struct{}

func newGofakes3Log() gofakes3.Logger { return gofakes3Log{} }

func (gofakes3Log) Print(level gofakes3.LogLevel, v ...interface{}) {
	if level == gofakes3.LogErr {
		zlog.Error().Msg(fmt.Sprint(v...))
	}
}
