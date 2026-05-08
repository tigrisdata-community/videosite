package main

import (
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"

	"github.com/facebookgo/flagenv"

	_ "github.com/joho/godotenv/autoload"
)

var (
	bind              = flag.String("bind", ":8080", "HTTP bind address")
	bucketName        = flag.String("bucket-name", "xe-videosite", "Tigris bucket name")
	bucketURLBase     = flag.String("bucket-url-base", "https://xe-videosite.t3.tigrisfiles.io/", "base URL for the bucket with leading slash")
	dbLoc             = flag.String("db-loc", "./var/data.db", "SQLite database location")
	tigrisEndpoint    = flag.String("tigris-storage-endpoint", "https://t3.storage.dev", "Tigris S3 endpoint")
	tigrisAccessKeyID = flag.String("tigris-storage-access-key-id", "", "Tigris access key ID")
	tigrisSecretKey   = flag.String("tigris-storage-secret-access-key", "", "Tigris secret access key")
)

func main() {
	flagenv.Parse()
	flag.Parse()

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

	srv, err := NewServer(ServerConfig{
		BucketName:        *bucketName,
		BucketURLBase:     *bucketURLBase,
		DBLoc:             *dbLoc,
		TigrisEndpoint:    *tigrisEndpoint,
		TigrisAccessKeyID: *tigrisAccessKeyID,
		TigrisSecretKey:   *tigrisSecretKey,
	}, lg)
	if err != nil {
		log.Fatal(err)
	}

	lg.Info("listening", "bind", *bind)
	if err := http.ListenAndServe(*bind, srv.Routes()); err != nil {
		slog.Error("http server stopped", "err", err)
		os.Exit(1)
	}
}
