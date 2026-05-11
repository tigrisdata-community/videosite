// videosite-encoder runs inside the Vast.ai Docker container. It downloads
// the source video from Tigris, runs ffmpeg with NVENC into a DASH bundle,
// uploads the outputs back to Tigris, and posts a signed webhook to the
// videosite server with success or failure.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"tangled.org/xeiaso.net/videosite/internal/encoder"
)

type env struct {
	JobID         string
	VideoID       string
	SourceBucket  string
	SourceKey     string
	DestPrefix    string
	S3Endpoint    string
	AccessKey     string
	SecretKey     string
	WebhookURL    string
	WebhookSecret string
}

func loadEnv() (env, error) {
	var missing []string
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	e := env{
		JobID:         get("JOB_ID"),
		VideoID:       get("VIDEO_ID"),
		SourceBucket:  get("SOURCE_BUCKET"),
		SourceKey:     get("SOURCE_KEY"),
		DestPrefix:    get("DEST_PREFIX"),
		S3Endpoint:    get("AWS_ENDPOINT_URL_S3"),
		AccessKey:     get("AWS_ACCESS_KEY_ID"),
		SecretKey:     get("AWS_SECRET_ACCESS_KEY"),
		WebhookURL:    get("WEBHOOK_URL"),
		WebhookSecret: get("WEBHOOK_SECRET"),
	}
	if len(missing) > 0 {
		return e, fmt.Errorf("missing env vars: %s", strings.Join(missing, ", "))
	}
	return e, nil
}

func main() {
	lg := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(lg)

	e, err := loadEnv()
	if err != nil {
		lg.Error("env", "err", err)
		os.Exit(2)
	}
	lg = lg.With("job_id", e.JobID, "video_id", e.VideoID)

	ctx := context.Background()
	if err := run(ctx, lg, e); err != nil {
		lg.Error("encode failed", "err", err)
		if werr := postWebhook(ctx, e, encoder.WebhookFailed, err.Error()); werr != nil {
			lg.Error("post failure webhook", "err", werr)
		}
		os.Exit(1)
	}

	if err := postWebhook(ctx, e, encoder.WebhookSucceeded, ""); err != nil {
		lg.Error("post success webhook", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, lg *slog.Logger, e env) error {
	work, err := os.MkdirTemp("", "videosite-encoder-*")
	if err != nil {
		return fmt.Errorf("mkdir tmp: %w", err)
	}
	defer os.RemoveAll(work)

	dashDir := filepath.Join(work, "dash")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		return fmt.Errorf("mkdir dash: %w", err)
	}

	s3c, err := newS3Client(ctx, e)
	if err != nil {
		return err
	}

	srcExt := filepath.Ext(e.SourceKey)
	if srcExt == "" {
		srcExt = ".bin"
	}
	srcPath := filepath.Join(work, "source"+srcExt)
	if err := download(ctx, s3c, e.SourceBucket, e.SourceKey, srcPath); err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	lg.Info("downloaded source", "path", srcPath)

	manifestPath := filepath.Join(dashDir, "manifest.mpd")
	args := encoder.FFmpegArgs(srcPath, manifestPath)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	lg.Info("running ffmpeg", "args", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	uploaded, err := uploadDir(ctx, s3c, e.SourceBucket, e.DestPrefix, dashDir)
	if err != nil {
		return fmt.Errorf("upload outputs: %w", err)
	}
	lg.Info("uploaded outputs", "count", uploaded)
	return nil
}

func newS3Client(ctx context.Context, e env) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			e.AccessKey, e.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(e.S3Endpoint)
		o.UsePathStyle = false
	}), nil
}

func download(ctx context.Context, c *s3.Client, bucket, key, dst string) error {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, out.Body); err != nil {
		return err
	}
	return f.Sync()
}

func uploadDir(ctx context.Context, c *s3.Client, bucket, prefix, dir string) (int, error) {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := prefix + filepath.ToSlash(rel)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		ctype := mime.TypeByExtension(filepath.Ext(path))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		_, err = c.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			Body:        f,
			ContentType: aws.String(ctype),
		})
		if err != nil {
			return fmt.Errorf("put %q: %w", key, err)
		}
		count++
		return nil
	})
	return count, err
}

func postWebhook(ctx context.Context, e env, status encoder.WebhookStatus, reason string) error {
	body, err := json.Marshal(encoder.WebhookBody{
		JobID:  e.JobID,
		Status: status,
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	sig := encoder.SignWebhookBody(e.WebhookSecret, body)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("webhook %d: %s", resp.StatusCode, out)
	}
	return nil
}
