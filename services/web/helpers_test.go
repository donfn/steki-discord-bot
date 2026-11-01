package web

import (
	"io"
	"log/slog"
	"strconv"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func snowflakeAt(t time.Time) string {
	const discordEpochMs = 1420070400000
	return strconv.FormatInt((t.UnixMilli()-discordEpochMs)<<22, 10)
}
