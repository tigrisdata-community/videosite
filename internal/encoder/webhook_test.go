package encoder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tigrisdata-community/videosite/internal/models"
)

func TestSignAndVerifyWebhook(t *testing.T) {
	body := []byte(`{"job_id":"abc","status":"succeeded"}`)
	sig := SignWebhookBody("topsecret", body)

	if !verifySignature("topsecret", body, sig) {
		t.Error("good signature did not verify")
	}
	if verifySignature("wrong", body, sig) {
		t.Error("bad secret verified")
	}
	if verifySignature("topsecret", []byte(`{"tampered":true}`), sig) {
		t.Error("tampered body verified")
	}
}

func TestWebhookHandler(t *testing.T) {
	ctx := context.Background()
	dao, err := models.New(filepath.Join(t.TempDir(), "test.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("init dao: %v", err)
	}
	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := dao.MarkVideoUploaded(ctx, "vid-1"); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	job, err := dao.CreateEncodingJob(ctx, "vid-1")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := dao.ClaimPendingEncodingJob(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := dao.MarkEncodingJobRunning(ctx, job.ID, 99, "tid_test", 0.20); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := dao.MarkVideoEncoding(ctx, "vid-1"); err != nil {
		t.Fatalf("mark video encoding: %v", err)
	}

	// Stub server for both vast.ai (DELETE /instances/99/) and Tigris IAM
	// (POST /, AWS query API). 200/204 for everything we care about so the
	// synchronous cleanup path in completeJob doesn't error.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v0/instances/"):
			w.WriteHeader(http.StatusNoContent)
		default: // IAM AWS query API responses are XML; empty 200 is fine.
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<Response/>`)
		}
	}))
	defer stub.Close()

	iam, err := NewTigrisIAM(ctx, TigrisIAMConfig{
		Endpoint: stub.URL, AccessKeyID: "k", SecretKey: "s", Bucket: "b",
	})
	if err != nil {
		t.Fatalf("iam: %v", err)
	}

	o := &Orchestrator{
		cfg:  Config{},
		dao:  dao,
		vast: NewVastClient("k", stub.URL, stub.Client()),
		iam:  iam,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		name     string
		body     WebhookBody
		secret   string // "" = use job.WebhookSecret
		wantCode int
	}{
		{
			name:     "missing job_id",
			body:     WebhookBody{Status: WebhookSucceeded},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown job",
			body:     WebhookBody{JobID: "ghost", Status: WebhookSucceeded},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "bad signature",
			body:     WebhookBody{JobID: job.ID, Status: WebhookSucceeded},
			secret:   "wrong",
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "unknown status",
			body:     WebhookBody{JobID: job.ID, Status: "exploded"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "succeeded",
			body:     WebhookBody{JobID: job.ID, Status: WebhookSucceeded},
			wantCode: http.StatusNoContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			secret := tt.secret
			if secret == "" {
				secret = job.WebhookSecret
			}
			req, err := http.NewRequest(http.MethodPost,
				srv.URL+"/api/encode-callback", bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("req: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(webhookSigHdr, SignWebhookBody(secret, raw))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantCode {
				out, _ := io.ReadAll(resp.Body)
				t.Errorf("code = %d, want %d body=%s", resp.StatusCode, tt.wantCode, out)
			}
		})
	}

	// After the successful webhook, EncodingJob → succeeded and Video → ready.
	got, err := dao.GetEncodingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != models.EncodingJobSucceeded {
		t.Errorf("job status = %q, want succeeded", got.Status)
	}
	v, err := dao.GetVideo(ctx, "vid-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.Status != models.VideoStatusReady {
		t.Errorf("video status = %q, want ready", v.Status)
	}
}

func TestWebhookHandler_FailedWithLogs(t *testing.T) {
	ctx := context.Background()
	dao, err := models.New(filepath.Join(t.TempDir(), "test.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("init dao: %v", err)
	}
	if _, err := dao.CreateVideo(ctx, "vid-fail", "bad.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := dao.MarkVideoUploaded(ctx, "vid-fail"); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	job, err := dao.CreateEncodingJob(ctx, "vid-fail")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := dao.ClaimPendingEncodingJob(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := dao.MarkEncodingJobRunning(ctx, job.ID, 42, "tid_fail", 0.10); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := dao.MarkVideoEncoding(ctx, "vid-fail"); err != nil {
		t.Fatalf("mark video encoding: %v", err)
	}

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v0/instances/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<Response/>`)
		}
	}))
	defer stub.Close()

	iam, err := NewTigrisIAM(ctx, TigrisIAMConfig{
		Endpoint: stub.URL, AccessKeyID: "k", SecretKey: "s", Bucket: "b",
	})
	if err != nil {
		t.Fatalf("iam: %v", err)
	}

	o := &Orchestrator{
		cfg:  Config{},
		dao:  dao,
		vast: NewVastClient("k", stub.URL, stub.Client()),
		iam:  iam,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ffmpegOutput := "frame= 1000 fps=30 q=28.0 size=  123456kB time=00:01:23.45 bitrate=12345.6kbits/s speed=1.0x\n[h264_nvenc @ 0x55a] encoder failed: invalid param"
	raw, err := json.Marshal(WebhookBody{
		JobID:  job.ID,
		Status: WebhookFailed,
		Reason: "ffmpeg: exit status 1",
		Logs:   ffmpegOutput,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/encode-callback", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhookSigHdr, SignWebhookBody(job.WebhookSecret, raw))

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("code = %d, want 204 body=%s", resp.StatusCode, out)
	}

	got, err := dao.GetEncodingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != models.EncodingJobFailed {
		t.Errorf("job status = %q, want failed", got.Status)
	}
	v, err := dao.GetVideo(ctx, "vid-fail")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.Status != models.VideoStatusFailed {
		t.Errorf("video status = %q, want failed", v.Status)
	}
	if v.FailureReason != "ffmpeg: exit status 1" {
		t.Errorf("failure reason = %q, want ffmpeg error", v.FailureReason)
	}
	if v.EncodeLogs != ffmpegOutput {
		t.Errorf("encode logs = %q, want ffmpeg output", v.EncodeLogs)
	}
}
