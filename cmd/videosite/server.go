package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"tangled.org/xeiaso.net/videosite/internal/alpinejs"
	"tangled.org/xeiaso.net/videosite/internal/encoder"
	"tangled.org/xeiaso.net/videosite/internal/htmx"
	"tangled.org/xeiaso.net/videosite/internal/models"
	"tangled.org/xeiaso.net/videosite/internal/upload"
	"tangled.org/xeiaso.net/videosite/internal/xess"
	"tangled.org/xeiaso.net/videosite/web"
)

type ServerConfig struct {
	BucketName        string
	BucketURLBase     string
	DBLoc             string
	TigrisEndpoint    string
	TigrisAccessKeyID string
	TigrisSecretKey   string
	TigrisIAMEndpoint string

	VastAPIKey  string
	VastAPIBase string

	EncoderImage   string
	WebhookBaseURL string
}

type Server struct {
	cfg          ServerConfig
	lg           *slog.Logger
	dao          *models.DAO
	uploader     *upload.Handler
	orchestrator *encoder.Orchestrator
}

func NewServer(ctx context.Context, cfg ServerConfig, lg *slog.Logger) (*Server, error) {
	dao, err := models.New(cfg.DBLoc, lg.With("component", "dao"))
	if err != nil {
		return nil, fmt.Errorf("server: init dao: %w", err)
	}

	uploader, err := upload.New(upload.Config{
		Bucket:      cfg.BucketName,
		Endpoint:    cfg.TigrisEndpoint,
		AccessKeyID: cfg.TigrisAccessKeyID,
		SecretKey:   cfg.TigrisSecretKey,
		URLBase:     cfg.BucketURLBase,
		Expires:     time.Hour,
	}, dao, lg.With("component", "upload"))
	if err != nil {
		return nil, fmt.Errorf("server: init uploader: %w", err)
	}

	s := &Server{
		cfg:      cfg,
		lg:       lg,
		dao:      dao,
		uploader: uploader,
	}

	if cfg.VastAPIKey != "" && cfg.WebhookBaseURL != "" {
		iam, err := encoder.NewTigrisIAM(ctx, encoder.TigrisIAMConfig{
			Endpoint:    cfg.TigrisIAMEndpoint,
			AccessKeyID: cfg.TigrisAccessKeyID,
			SecretKey:   cfg.TigrisSecretKey,
			Bucket:      cfg.BucketName,
		})
		if err != nil {
			return nil, fmt.Errorf("server: init tigris iam: %w", err)
		}
		vast := encoder.NewVastClient(cfg.VastAPIKey, cfg.VastAPIBase, http.DefaultClient)
		o := encoder.NewOrchestrator(encoder.Config{
			DockerImage:     cfg.EncoderImage,
			WebhookBaseURL:  cfg.WebhookBaseURL,
			Bucket:          cfg.BucketName,
			StorageEndpoint: cfg.TigrisEndpoint,
		}, dao, vast, iam, lg.With("component", "encoder"))
		o.Start(ctx)
		s.orchestrator = o
		uploader.OnUploaded = func(ctx context.Context, videoID string) {
			if _, err := dao.CreateEncodingJob(ctx, videoID); err != nil {
				lg.Error("create encoding job", "err", err, "video_id", videoID)
			}
		}
	} else {
		lg.Warn("encoder orchestrator disabled — set VAST_API_KEY and WEBHOOK_BASE_URL to enable")
	}

	return s, nil
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	xess.Mount(mux)
	htmx.Mount(mux)
	alpinejs.Mount(mux)
	s.uploader.Mount(mux)
	if s.orchestrator != nil {
		s.orchestrator.Mount(mux)
	}

	mux.Handle("/static/", http.FileServerFS(web.Static))
	mux.Handle("/{$}", s.indexPage())
	mux.Handle("/upload", s.uploadPage())
	mux.Handle("/", templ.Handler(
		xess.Simple("Not found", web.NotFound()),
		templ.WithStatus(http.StatusNotFound),
	))

	return mux
}

func (s *Server) indexPage() http.Handler {
	return templ.Handler(xess.Base(
		"videosite",
		web.HeadArea(),
		web.Navbar(),
		web.Index(),
		web.Footer(),
	))
}

func (s *Server) uploadPage() http.Handler {
	return templ.Handler(xess.Base(
		"Upload | videosite",
		web.HeadArea(),
		web.Navbar(),
		web.Upload(),
		web.Footer(),
	))
}
