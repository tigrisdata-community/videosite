package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"gorm.io/gorm"

	"github.com/tigrisdata-community/videosite/internal/alpinejs"
	"github.com/tigrisdata-community/videosite/internal/encoder"
	"github.com/tigrisdata-community/videosite/internal/htmx"
	"github.com/tigrisdata-community/videosite/internal/models"
	"github.com/tigrisdata-community/videosite/internal/upload"
	"github.com/tigrisdata-community/videosite/internal/xess"
	"github.com/tigrisdata-community/videosite/web"
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
		lg.Info("started vast.ai encoder orchestrator")
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
	mux.Handle("GET /v/{id}", s.videoPage())
	mux.Handle("GET /v/{id}/status", s.videoStatus())
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

func (s *Server) manifestURL(id string) string {
	return strings.TrimRight(s.cfg.BucketURLBase, "/") + "/v/" + id + "/manifest.mpd"
}

func (s *Server) videoPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		v, err := s.dao.GetVideo(r.Context(), id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				w.WriteHeader(http.StatusNotFound)
				_ = xess.Simple("Not found", web.NotFound()).Render(r.Context(), w)
				return
			}
			s.lg.Error("get video", "err", err, "id", id)
			http.Error(w, "get video", http.StatusInternalServerError)
			return
		}
		title := v.Filename
		if title == "" {
			title = v.ID
		}
		page := xess.Base(
			title,
			web.HeadArea(),
			web.Navbar(),
			web.Video(v, s.manifestURL(v.ID)),
			web.Footer(),
		)
		if err := page.Render(r.Context(), w); err != nil {
			s.lg.Error("render video page", "err", err, "id", id)
		}
	})
}

func (s *Server) videoStatus() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !htmx.Is(r) {
			http.Redirect(w, r, "/v/"+id, http.StatusSeeOther)
			return
		}
		v, err := s.dao.GetVideo(r.Context(), id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			s.lg.Error("get video status", "err", err, "id", id)
			http.Error(w, "get video", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if v.Status == models.VideoStatusReady || v.Status == models.VideoStatusFailed {
			w.WriteHeader(htmx.StatusStopPolling)
		}
		if err := web.VideoStatusPanel(v, s.manifestURL(v.ID)).Render(r.Context(), w); err != nil {
			s.lg.Error("render video status", "err", err, "id", id)
		}
	})
}
