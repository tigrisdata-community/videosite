package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

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

	tigrisIAMEndpoint = flag.String("tigris-iam-endpoint", "https://iam.storage.dev", "Tigris IAM endpoint")

	vastAPIKey  = flag.String("vast-api-key", "", "Vast.ai API key")
	vastAPIBase = flag.String("vast-api-base", "https://console.vast.ai", "Vast.ai API base URL")

	encoderImage           = flag.String("encoder-docker-image", "reg.xeiaso.net/xeserv/videosite-encoder:latest", "Docker image for the encoder container")
	encoderDiskGB          = flag.Int("encoder-disk-gb", 32, "Disk size in GB for the encoder container")
	encoderPollInterval    = flag.Duration("encoder-poll-interval", 5*time.Second, "How often to claim a pending encoding job")
	encoderJanitorInterval = flag.Duration("encoder-janitor-interval", 30*time.Second, "How often to reconcile running encoding jobs")
	encoderMaxDuration     = flag.Duration("encoder-max-duration", 2*time.Hour, "Force-fail running encoding jobs older than this")
	encoderMinReliability  = flag.Float64("encoder-min-reliability", 0.95, "Minimum vast.ai host reliability score")

	webhookBaseURL = flag.String("webhook-base-url", "", "Public base URL the encoder posts callbacks to (e.g. https://videosite.example)")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv, err := NewServer(ctx, ServerConfig{
		BucketName:             *bucketName,
		BucketURLBase:          *bucketURLBase,
		DBLoc:                  *dbLoc,
		TigrisEndpoint:         *tigrisEndpoint,
		TigrisAccessKeyID:      *tigrisAccessKeyID,
		TigrisSecretKey:        *tigrisSecretKey,
		TigrisIAMEndpoint:      *tigrisIAMEndpoint,
		VastAPIKey:             *vastAPIKey,
		VastAPIBase:            *vastAPIBase,
		EncoderImage:           *encoderImage,
		EncoderDiskGB:          *encoderDiskGB,
		EncoderPollInterval:    *encoderPollInterval,
		EncoderJanitorInterval: *encoderJanitorInterval,
		EncoderMaxDuration:     *encoderMaxDuration,
		EncoderMinReliability:  *encoderMinReliability,
		WebhookBaseURL:         *webhookBaseURL,
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
