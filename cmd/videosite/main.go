package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"

	"github.com/a-h/templ"
	"github.com/facebookgo/flagenv"
	"tangled.org/xeiaso.net/videosite/internal/htmx"
	"tangled.org/xeiaso.net/videosite/internal/models"
	"tangled.org/xeiaso.net/videosite/internal/xess"
	"tangled.org/xeiaso.net/videosite/web"
)

var (
	bind          = flag.String("bind", ":8080", "HTTP bind address")
	bucketName    = flag.String("bucket-name", "xe-videosite", "Tigris bucket name")
	bucketURLBase = flag.String("bucket-url-base", "https://xe-videosite.t3.tigrisfiles.io/", "base URL for the bucket with leading slash")
	dbLoc         = flag.String("db-loc", "./var/data.db", "SQLite database location")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		panic("can't read buildinfo")
	}

	lg := slog.With("version", bi.Main.Version)
	lg.Info(
		"starting up",
		"bind", *bind,
		"bucket-name", *bucketName,
		"bucket-url-base", *bucketURLBase,
		"db-loc", *dbLoc,
	)

	dao, err := models.New(*dbLoc, lg.With("component", "logger"))
	if err != nil {
		log.Fatal(err)
	}
	_ = dao

	mux := http.NewServeMux()
	xess.Mount(mux)
	htmx.Mount(mux)

	mux.Handle("/static/", http.FileServerFS(web.Static))

	mux.Handle("/{$}", templ.Handler(
		xess.Base(
			"videosite",
			nil,
			web.Navbar(),
			web.Index(),
			web.Footer(),
		),
	))

	mux.Handle("/", templ.Handler(
		xess.Simple("Not found", web.NotFound()),
		templ.WithStatus(http.StatusNotFound),
	))

	lg.Info("listening", "bind", *bind)
	if err := http.ListenAndServe(*bind, mux); err != nil {
		slog.Error("http server stopped", "err", err)
		os.Exit(1)
	}
}
