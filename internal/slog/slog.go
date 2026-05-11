// Package slog is my set of wrappers around package slog.
package slog

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

var (
	slogLevel = flag.String("slog-level", "INFO", "log level")

	// The current slog handler.
	Handler slog.Handler

	leveler *slog.LevelVar
)

func Init() {
	var programLevel slog.Level
	if err := (&programLevel).UnmarshalText([]byte(*slogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level %s: %v, using info\n", *slogLevel, err)
		programLevel = slog.LevelInfo
	}

	leveler = &slog.LevelVar{}
	leveler.Set(programLevel)

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     leveler,
	})
	slog.SetDefault(slog.New(h))

	Handler = h
}
