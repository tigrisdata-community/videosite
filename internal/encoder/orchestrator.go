package encoder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tangled.org/xeiaso.net/videosite/internal/models"
)

// Config tunes the orchestrator. All fields have sensible zero-value
// fallbacks except the credentials.
type Config struct {
	DockerImage     string
	DiskGB          int
	PollInterval    time.Duration
	JanitorInterval time.Duration
	MaxJobDuration  time.Duration
	WebhookBaseURL  string // e.g. https://videosite.example.com — no trailing slash
	GpuPrefs        []string
	MinReliability  float64

	Bucket          string
	StorageEndpoint string
}

func (c *Config) defaults() {
	if c.DiskGB == 0 {
		c.DiskGB = 32
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.JanitorInterval == 0 {
		c.JanitorInterval = 30 * time.Second
	}
	if c.MaxJobDuration == 0 {
		c.MaxJobDuration = 2 * time.Hour
	}
	if len(c.GpuPrefs) == 0 {
		c.GpuPrefs = []string{"RTX_3090", "RTX_4090"}
	}
	if c.MinReliability == 0 {
		c.MinReliability = 0.95
	}
}

// Orchestrator owns the encoding-job lifecycle. It runs two goroutines: a
// pending-claimer that picks up new jobs and launches Vast.ai instances, and
// a janitor that reconciles instances whose encoders never reported back.
type Orchestrator struct {
	cfg  Config
	dao  *models.DAO
	vast *VastClient
	iam  *TigrisIAM
	log  *slog.Logger
}

func NewOrchestrator(cfg Config, dao *models.DAO, vast *VastClient, iam *TigrisIAM, lg *slog.Logger) *Orchestrator {
	cfg.defaults()
	return &Orchestrator{cfg: cfg, dao: dao, vast: vast, iam: iam, log: lg}
}

// Mount registers the webhook route. Call this from the server's Routes().
func (o *Orchestrator) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/encode-callback", o.handleWebhook)
}

// Start kicks off the background goroutines. They exit when ctx is cancelled.
func (o *Orchestrator) Start(ctx context.Context) {
	go o.pendingLoop(ctx)
	go o.janitorLoop(ctx)
}

func (o *Orchestrator) pendingLoop(ctx context.Context) {
	t := time.NewTicker(o.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := o.claimAndLaunchOne(ctx); err != nil && !errors.Is(err, models.ErrNoPending) {
				o.log.Error("pending loop", "err", err)
			}
		}
	}
}

func (o *Orchestrator) claimAndLaunchOne(ctx context.Context) error {
	job, err := o.dao.ClaimPendingEncodingJob(ctx)
	if err != nil {
		return err
	}
	o.log.Info("claimed pending job", "id", job.ID, "video_id", job.VideoID)

	video, err := o.dao.GetVideo(ctx, job.VideoID)
	if err != nil {
		o.failJob(ctx, job, fmt.Sprintf("get video: %v", err))
		return nil
	}

	sourceKey := fmt.Sprintf("raw/%s/%s", video.ID, video.Filename)
	destPrefix := fmt.Sprintf("v/%s/", video.ID)

	scoped, err := o.iam.CreateScopedKey(ctx, sourceKey, destPrefix)
	if err != nil {
		o.failJob(ctx, job, fmt.Sprintf("scoped key: %v", err))
		return nil
	}

	offers, err := o.vast.SearchOffers(ctx, PreferredOfferQuery(o.cfg.GpuPrefs, o.cfg.MinReliability))
	if err != nil {
		_ = o.iam.DeleteKey(ctx, scoped.AccessKeyID)
		o.failJob(ctx, job, fmt.Sprintf("search offers: %v", err))
		return nil
	}
	offer, err := PickOffer(offers, o.cfg.GpuPrefs)
	if err != nil {
		_ = o.iam.DeleteKey(ctx, scoped.AccessKeyID)
		o.failJob(ctx, job, fmt.Sprintf("pick offer: %v", err))
		return nil
	}
	o.log.Info("picked offer",
		"ask_contract_id", offer.AskContractID,
		"gpu", offer.GpuName, "dph", offer.DphTotal)

	env := map[string]string{
		"JOB_ID":                job.ID,
		"VIDEO_ID":              video.ID,
		"SOURCE_BUCKET":         o.cfg.Bucket,
		"SOURCE_KEY":            sourceKey,
		"DEST_PREFIX":           destPrefix,
		"AWS_ENDPOINT_URL_S3":   o.cfg.StorageEndpoint,
		"AWS_ACCESS_KEY_ID":     scoped.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": scoped.SecretKey,
		"AWS_REGION":            "auto",
		"WEBHOOK_URL":           strings.TrimRight(o.cfg.WebhookBaseURL, "/") + "/api/encode-callback",
		"WEBHOOK_SECRET":        job.WebhookSecret,
	}

	instanceID, err := o.vast.Mint(ctx, offer.AskContractID, LaunchConfig{
		Image:   o.cfg.DockerImage,
		Env:     env,
		Disk:    o.cfg.DiskGB,
		Onstart: "/usr/local/bin/videosite-encoder",
		Label:   "videosite-encoder/" + job.ID,
	})
	if err != nil {
		_ = o.iam.DeleteKey(ctx, scoped.AccessKeyID)
		o.failJob(ctx, job, fmt.Sprintf("mint: %v", err))
		return nil
	}

	if err := o.dao.MarkEncodingJobRunning(ctx, job.ID, instanceID, scoped.AccessKeyID, offer.DphTotal); err != nil {
		// We minted an instance but couldn't record it — slay it so it
		// doesn't run unattended.
		_ = o.vast.Destroy(ctx, instanceID)
		_ = o.iam.DeleteKey(ctx, scoped.AccessKeyID)
		return fmt.Errorf("mark running: %w", err)
	}

	if err := o.dao.MarkVideoEncoding(ctx, video.ID); err != nil && !errors.Is(err, models.ErrConflict) {
		o.log.Error("mark video encoding", "err", err, "video_id", video.ID)
	}

	o.log.Info("minted instance", "instance_id", instanceID, "job_id", job.ID)
	return nil
}

func (o *Orchestrator) failJob(ctx context.Context, job *models.EncodingJob, reason string) {
	o.log.Error("job failed", "id", job.ID, "reason", reason)
	if err := o.dao.MarkEncodingJobFailed(ctx, job.ID, reason); err != nil && !errors.Is(err, models.ErrConflict) {
		o.log.Error("mark job failed", "err", err, "id", job.ID)
	}
	if err := o.dao.MarkVideoFailed(ctx, job.VideoID, reason); err != nil && !errors.Is(err, models.ErrConflict) {
		o.log.Error("mark video failed", "err", err, "video_id", job.VideoID)
	}
}

func (o *Orchestrator) janitorLoop(ctx context.Context) {
	t := time.NewTicker(o.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.runJanitor(ctx)
		}
	}
}

func (o *Orchestrator) runJanitor(ctx context.Context) {
	jobs, err := o.dao.ListEncodingJobsForJanitor(ctx)
	if err != nil {
		o.log.Error("janitor: list jobs", "err", err)
		return
	}
	for _, job := range jobs {
		o.reconcileJob(ctx, job)
	}
}

func (o *Orchestrator) reconcileJob(ctx context.Context, job *models.EncodingJob) {
	// Time-out: any running job past the deadline is force-failed.
	if job.StartedAt != nil && time.Since(*job.StartedAt) > o.cfg.MaxJobDuration {
		_ = o.completeJob(ctx, job, false, "exceeded max job duration")
		return
	}

	// Stuck launching: orchestrator probably crashed mid-mint. Fail the job
	// and clean up whatever might already exist.
	if job.Status == models.EncodingJobLaunching && time.Since(job.UpdatedAt) > 5*time.Minute {
		_ = o.completeJob(ctx, job, false, "stuck in launching")
		return
	}

	if job.VastInstanceID == 0 {
		// Nothing to poll yet; pendingLoop will catch it.
		return
	}

	inst, err := o.vast.GetInstance(ctx, job.VastInstanceID)
	if errors.Is(err, ErrInstanceGone) {
		_ = o.completeJob(ctx, job, false, "vast instance disappeared")
		return
	}
	if err != nil {
		o.log.Warn("janitor: get instance", "err", err, "instance_id", job.VastInstanceID)
		return
	}

	if inst.ActualStatus == "exited" || inst.ActualStatus == "stopped" {
		_ = o.completeJob(ctx, job, false,
			fmt.Sprintf("instance ended (status=%s msg=%s) without webhook", inst.ActualStatus, inst.StatusMsg))
	}
}

// cleanup slays the Vast.ai instance and deletes the IAM key. Idempotent.
func (o *Orchestrator) cleanup(job *models.EncodingJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if job.VastInstanceID != 0 {
		if err := o.vast.Destroy(ctx, job.VastInstanceID); err != nil {
			o.log.Warn("cleanup: destroy instance", "err", err, "instance_id", job.VastInstanceID)
		}
	}
	if job.TigrisAccessKeyID != "" {
		if err := o.iam.DeleteKey(ctx, job.TigrisAccessKeyID); err != nil {
			o.log.Warn("cleanup: delete iam key", "err", err, "access_key_id", job.TigrisAccessKeyID)
		}
	}
}
