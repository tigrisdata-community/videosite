// Package upload implements the Tigris client-upload broker protocol.
//
// The browser POSTs JSON to /api/upload with one of four actions
// (singlepart-init, multipart-init, multipart-get-parts, multipart-complete)
// and receives presigned S3 URLs that it uses to PUT bytes directly to
// Tigris, never proxying through this server.
//
// After a successful multipart upload the browser POSTs to
// /api/upload/finalize, which marks the Video as uploaded and returns an
// HTML fragment for HTMX to swap into the page.
package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"tangled.org/xeiaso.net/videosite/internal/models"
	"tangled.org/xeiaso.net/videosite/web"
)

type Action string

const (
	SinglepartInit    Action = "singlepart-init"
	MultipartInit     Action = "multipart-init"
	MultipartGetParts Action = "multipart-get-parts"
	MultipartComplete Action = "multipart-complete"
)

type Config struct {
	Bucket      string
	Endpoint    string
	AccessKeyID string
	SecretKey   string
	URLBase     string
	Expires     time.Duration
}

type Handler struct {
	cfg     Config
	s3      *s3.Client
	presign *s3.PresignClient
	dao     *models.DAO
	log     *slog.Logger
}

func New(cfg Config, dao *models.DAO, lg *slog.Logger) (*Handler, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("upload: Bucket is required")
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("upload: Endpoint is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretKey == "" {
		return nil, errors.New("upload: AccessKeyID and SecretKey are required")
	}
	if cfg.Expires == 0 {
		cfg.Expires = time.Hour
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("upload: load aws config: %w", err)
	}

	s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = false
	})

	return &Handler{
		cfg:     cfg,
		s3:      s3c,
		presign: s3.NewPresignClient(s3c),
		dao:     dao,
		log:     lg,
	}, nil
}

func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/upload", h.broker)
	mux.HandleFunc("POST /api/upload/finalize", h.finalize)
}

type request struct {
	Action   Action              `json:"action"`
	Name     string              `json:"name"`
	UploadID string              `json:"uploadId,omitempty"`
	Parts    []int32             `json:"parts,omitempty"`
	PartIDs  []map[string]string `json:"partIds,omitempty"`
}

type response struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func (h *Handler) broker(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var req request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}

	ctx := r.Context()
	switch req.Action {
	case SinglepartInit:
		h.singlepartInit(ctx, w, req)
	case MultipartInit:
		h.multipartInit(ctx, w, req)
	case MultipartGetParts:
		h.multipartGetParts(ctx, w, req)
	case MultipartComplete:
		h.multipartComplete(ctx, w, req)
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid action %q", req.Action))
	}
}

// validateName ensures the client-supplied filename can't escape the raw/<id>/ prefix.
func validateName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > 512 {
		return errors.New("name too long")
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return errors.New("name contains invalid characters")
	}
	clean := path.Clean("/" + name)
	if clean != "/"+name && clean != "/"+strings.TrimPrefix(name, "/") {
		return errors.New("name must be a clean relative path")
	}
	if strings.Contains(name, "..") {
		return errors.New("name must not contain ..")
	}
	return nil
}

// validateKey makes sure the key the client echoes back is one we issued
// (under the raw/ prefix). Stops a client from presigning arbitrary keys.
func validateKey(key string) error {
	if !strings.HasPrefix(key, "raw/") {
		return errors.New("key must be under raw/")
	}
	if strings.Contains(key, "..") {
		return errors.New("key must not contain ..")
	}
	return nil
}

func (h *Handler) singlepartInit(ctx context.Context, w http.ResponseWriter, req request) {
	if err := validateName(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := uuid.NewString()
	key := "raw/" + id + "/" + req.Name

	if _, err := h.dao.CreateVideo(ctx, id); err != nil {
		h.log.Error("create video", "err", err, "id", id)
		writeErr(w, http.StatusInternalServerError, errors.New("create video"))
		return
	}

	signed, err := h.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(h.cfg.Bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(h.cfg.Expires))
	if err != nil {
		h.log.Error("presign put", "err", err, "key", key)
		writeErr(w, http.StatusInternalServerError, errors.New("presign"))
		return
	}

	writeJSON(w, http.StatusOK, response{Data: map[string]any{
		"url": signed.URL,
		"key": key,
		"id":  id,
	}})
}

func (h *Handler) multipartInit(ctx context.Context, w http.ResponseWriter, req request) {
	if err := validateName(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := uuid.NewString()
	key := "raw/" + id + "/" + req.Name

	if _, err := h.dao.CreateVideo(ctx, id); err != nil {
		h.log.Error("create video", "err", err, "id", id)
		writeErr(w, http.StatusInternalServerError, errors.New("create video"))
		return
	}

	out, err := h.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(h.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		h.log.Error("create multipart", "err", err, "key", key)
		writeErr(w, http.StatusInternalServerError, errors.New("create multipart"))
		return
	}

	writeJSON(w, http.StatusOK, response{Data: map[string]any{
		"uploadId": aws.ToString(out.UploadId),
		"key":      key,
		"id":       id,
	}})
}

func (h *Handler) multipartGetParts(ctx context.Context, w http.ResponseWriter, req request) {
	if err := validateKey(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.UploadID == "" || len(req.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("uploadId and parts are required"))
		return
	}

	urls := make([]map[string]any, 0, len(req.Parts))
	for _, p := range req.Parts {
		signed, err := h.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(h.cfg.Bucket),
			Key:        aws.String(req.Name),
			UploadId:   aws.String(req.UploadID),
			PartNumber: aws.Int32(p),
		}, s3.WithPresignExpires(h.cfg.Expires))
		if err != nil {
			h.log.Error("presign part", "err", err, "key", req.Name, "part", p)
			writeErr(w, http.StatusInternalServerError, errors.New("presign part"))
			return
		}
		urls = append(urls, map[string]any{"part": p, "url": signed.URL})
	}

	writeJSON(w, http.StatusOK, response{Data: urls})
}

func (h *Handler) multipartComplete(ctx context.Context, w http.ResponseWriter, req request) {
	if err := validateKey(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.UploadID == "" || len(req.PartIDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("uploadId and partIds are required"))
		return
	}

	parts := make([]s3types.CompletedPart, 0, len(req.PartIDs))
	for _, m := range req.PartIDs {
		for k, etag := range m {
			n, err := strconv.Atoi(k)
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("bad part number %q", k))
				return
			}
			etag = strings.Trim(etag, `"`)
			if etag == "" {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("missing etag for part %d", n))
				return
			}
			parts = append(parts, s3types.CompletedPart{
				ETag:       aws.String(`"` + etag + `"`),
				PartNumber: aws.Int32(int32(n)),
			})
		}
	}
	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})

	if _, err := h.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(h.cfg.Bucket),
		Key:             aws.String(req.Name),
		UploadId:        aws.String(req.UploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		h.log.Error("complete multipart", "err", err, "key", req.Name, "parts", parts)
		writeErr(w, http.StatusInternalServerError, errors.New("complete multipart"))
		return
	}

	writeJSON(w, http.StatusOK, response{Data: map[string]any{
		"path": req.Name,
		"url":  strings.TrimRight(h.cfg.URLBase, "/") + "/" + req.Name,
	}})
}

func (h *Handler) finalize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := h.dao.MarkVideoUploaded(ctx, id); err != nil {
		h.log.Error("mark uploaded", "err", err, "id", id)
		http.Error(w, "mark uploaded", http.StatusInternalServerError)
		return
	}
	v, err := h.dao.GetVideo(ctx, id)
	if err != nil {
		h.log.Error("get video", "err", err, "id", id)
		http.Error(w, "get video", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := web.UploadedRow(v).Render(ctx, w); err != nil {
		h.log.Error("render row", "err", err, "id", id)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, response{Error: err.Error()})
}
