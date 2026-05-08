package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"tangled.org/xeiaso.net/videosite/internal/alpinejs"
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
}

type Server struct {
	cfg      ServerConfig
	lg       *slog.Logger
	dao      *models.DAO
	uploader *upload.Handler
}

func NewServer(cfg ServerConfig, lg *slog.Logger) (*Server, error) {
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

	return &Server{
		cfg:      cfg,
		lg:       lg,
		dao:      dao,
		uploader: uploader,
	}, nil
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	xess.Mount(mux)
	htmx.Mount(mux)
	alpinejs.Mount(mux)
	s.uploader.Mount(mux)

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
