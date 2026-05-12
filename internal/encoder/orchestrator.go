package encoder

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tigrisdata-community/videosite/internal/models"
)

// Orchestrator owns the encoding-job lifecycle. A single ticker drives both
// halves of the work: claim one pending job, reconcile any non-terminal
// jobs whose Vast.ai instance has exited without firing the webhook.
type Orchestrator struct {
	cfg  Config
	dao  *models.DAO
	vast *VastClient
	iam  *TigrisIAM
	log  *slog.Logger
}

// Config is what the server passes in. Tunables not in here are constants
// (tickInterval, maxJobDuration, DefaultGPUPrefs, DefaultMinReliability) —
// promote them to fields the day someone actually wants to override one.
type Config struct {
	DockerImage     string
	WebhookBaseURL  string // public base URL the encoder POSTs callbacks to
	Bucket          string
	StorageEndpoint string
}

const (
	claimInterval     = 10 * time.Second
	reconcileInterval = 5 * time.Minute
	maxJobDuration    = 2 * time.Hour
	encoderDiskGB     = 32
	webhookSigHdr     = "X-Webhook-Signature"
	cleanupInterval   = 1 * time.Hour
	staleKeyAge       = 48 * time.Hour

	// DefaultMinReliability is the host reliability floor the orchestrator
	// passes to PreferredOfferQuery. Exported so the vast-search CLI can
	// share the same default.
	DefaultMinReliability = 0.95
)

// DefaultGPUPrefs is the GPU preference list the orchestrator uses, ordered
// highest priority first. Exported so the vast-search CLI matches production.
var DefaultGPUPrefs = []string{"RTX 3090", "RTX 4090", "RTX 5090"}

// DefaultGeolocations is the country allowlist passed to PreferredOfferQuery:
// North America plus Western/Northern Europe. Restricts hosts to jurisdictions
// we're comfortable shipping source uploads to. Exported so the vast-search
// CLI matches production.
var DefaultGeolocations = []string{
	"US", "CA", "MX",
	"GB", "IE", "FR", "DE", "NL", "BE", "CH", "AT",
	"IT", "ES", "SE", "FI", "NO", "DK", "PL",
}

func NewOrchestrator(cfg Config, dao *models.DAO, vast *VastClient, iam *TigrisIAM, lg *slog.Logger) *Orchestrator {
	return &Orchestrator{cfg: cfg, dao: dao, vast: vast, iam: iam, log: lg}
}

func (o *Orchestrator) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/encode-callback", o.handleWebhook)
}

// Start kicks off the background loops. They all exit when ctx is cancelled.
// Claim runs fast so a newly-uploaded video starts encoding promptly; it only
// hits vast.ai when a pending row exists. Reconcile polls vast.ai for every
// running job, so it runs on a much slower cadence to stay under the API rate
// limit. Cleanup sweeps stale IAM keys hourly so a crash between mint and the
// completion path doesn't leak credentials forever.
func (o *Orchestrator) Start(ctx context.Context) {
	go o.claimLoop(ctx)
	go o.reconcileLoop(ctx)
	go o.cleanupLoop(ctx)
}

func (o *Orchestrator) claimLoop(ctx context.Context) {
	t := time.NewTicker(claimInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := o.claimAndLaunchOne(ctx); err != nil && !errors.Is(err, models.ErrNoPending) {
				o.log.Error("claim/launch", "err", err)
			}
		}
	}
}

func (o *Orchestrator) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.reconcile(ctx)
		}
	}
}

// claimAndLaunchOne is the saga: claim → IAM key → search → mint → record.
// On any step's failure we roll back the work we've done so far and mark the
// job failed. Returns ErrNoPending when nothing's waiting.
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

	scoped, err := o.iam.CreateScopedKey(ctx, job.ID)
	if err != nil {
		o.failJob(ctx, job, fmt.Sprintf("scoped key: %v", err))
		return nil
	}

	offer, err := o.findOffer(ctx)
	if err != nil {
		_ = o.iam.DeleteScopedKey(ctx, scoped.AccessKeyID)
		o.failJob(ctx, job, err.Error())
		return nil
	}
	o.log.Info("picked offer", "ask_contract_id", offer.AskContractID, "gpu", offer.GpuName, "dph", offer.DphTotal)

	instanceID, err := o.vast.Mint(ctx, offer.AskContractID, LaunchConfig{
		Image:   o.cfg.DockerImage,
		Env:     o.buildEnv(job, video, sourceKey, destPrefix, scoped),
		Disk:    encoderDiskGB,
		Onstart: "/usr/local/bin/videosite-encoder",
		Label:   "videosite-encoder/" + job.ID,
	})
	if err != nil {
		_ = o.iam.DeleteScopedKey(ctx, scoped.AccessKeyID)
		o.failJob(ctx, job, fmt.Sprintf("mint: %v", err))
		return nil
	}

	if err := o.dao.MarkEncodingJobRunning(ctx, job.ID, instanceID, scoped.AccessKeyID, offer.DphTotal); err != nil {
		// Minted but couldn't record — slay before anyone gets billed.
		_ = o.vast.Destroy(ctx, instanceID)
		_ = o.iam.DeleteScopedKey(ctx, scoped.AccessKeyID)
		return fmt.Errorf("mark running: %w", err)
	}
	if err := o.dao.MarkVideoEncoding(ctx, video.ID); err != nil && !errors.Is(err, models.ErrConflict) {
		o.log.Error("mark video encoding", "err", err, "video_id", video.ID)
	}

	o.log.Info("minted instance", "instance_id", instanceID, "job_id", job.ID)
	return nil
}

func (o *Orchestrator) findOffer(ctx context.Context) (Offer, error) {
	offers, err := o.vast.SearchOffers(ctx, PreferredOfferQuery(DefaultGPUPrefs, DefaultGeolocations, DefaultMinReliability))
	if err != nil {
		return Offer{}, fmt.Errorf("search offers: %w", err)
	}
	offer, err := PickOffer(offers, DefaultGPUPrefs)
	if err != nil {
		return Offer{}, fmt.Errorf("pick offer: %w", err)
	}
	return offer, nil
}

func (o *Orchestrator) buildEnv(job *models.EncodingJob, video *models.Video, sourceKey, destPrefix string, scoped *ScopedKey) map[string]string {
	return map[string]string{
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

// reconcile walks non-terminal jobs: timing out the long-running ones,
// failing jobs whose Vast.ai instance has exited, and tearing down any
// resources left behind by a terminal job whose webhook ran but whose
// cleanup didn't get a chance (or where the janitor itself is the path
// that ends the job).
func (o *Orchestrator) reconcile(ctx context.Context) {
	jobs, err := o.dao.ListEncodingJobsForJanitor(ctx)
	if err != nil {
		o.log.Error("list jobs for janitor", "err", err)
		return
	}
	for _, job := range jobs {
		o.reconcileJob(ctx, job)
	}
}

func (o *Orchestrator) reconcileJob(ctx context.Context, job *models.EncodingJob) {
	if job.StartedAt != nil && time.Since(*job.StartedAt) > maxJobDuration {
		o.completeJob(ctx, job, false, "exceeded max job duration", "")
		return
	}
	if job.VastInstanceID == 0 {
		return
	}
	inst, err := o.vast.GetInstance(ctx, job.VastInstanceID)
	if errors.Is(err, ErrInstanceGone) {
		o.completeJob(ctx, job, false, "vast instance disappeared", "")
		return
	}
	if err != nil {
		o.log.Warn("janitor: get instance", "err", err, "instance_id", job.VastInstanceID)
		return
	}
	if inst.ActualStatus == "exited" || inst.ActualStatus == "stopped" {
		o.completeJob(ctx, job, false,
			fmt.Sprintf("instance ended (status=%s msg=%s) without webhook", inst.ActualStatus, inst.StatusMsg), "")
	}
}

// completeJob transitions the EncodingJob + Video together and tears down
// the instance and IAM key. All steps are idempotent and ignore "already
// done" errors, so it's safe to call from both the webhook and the janitor.
func (o *Orchestrator) completeJob(ctx context.Context, job *models.EncodingJob, ok bool, reason, logs string) {
	var jobErr error
	if ok {
		jobErr = o.dao.MarkEncodingJobSucceeded(ctx, job.ID)
		if jobErr == nil {
			if err := o.dao.MarkVideoReady(ctx, job.VideoID); err != nil && !errors.Is(err, models.ErrConflict) {
				o.log.Error("mark video ready", "err", err, "video_id", job.VideoID)
			}
		}
	} else {
		jobErr = o.dao.MarkEncodingJobFailed(ctx, job.ID, reason)
		if jobErr == nil {
			var err error
			if logs != "" {
				err = o.dao.MarkVideoFailedWithLogs(ctx, job.VideoID, reason, logs)
			} else {
				err = o.dao.MarkVideoFailed(ctx, job.VideoID, reason)
			}
			if err != nil && !errors.Is(err, models.ErrConflict) {
				o.log.Error("mark video failed", "err", err, "video_id", job.VideoID)
			}
		}
	}
	// ErrConflict means another path beat us to a terminal state. Either
	// way, the resources still need cleaning up.
	if jobErr != nil && !errors.Is(jobErr, models.ErrConflict) {
		o.log.Error("complete job", "err", jobErr, "id", job.ID)
		return
	}

	if job.VastInstanceID != 0 {
		if err := o.vast.Destroy(ctx, job.VastInstanceID); err != nil {
			o.log.Warn("destroy instance", "err", err, "instance_id", job.VastInstanceID)
		}
	}
	if job.TigrisAccessKeyID != "" {
		if err := o.iam.DeleteScopedKey(ctx, job.TigrisAccessKeyID); err != nil {
			o.log.Warn("delete iam key", "err", err, "access_key_id", job.TigrisAccessKeyID)
		}
	}
}

// cleanupLoop sweeps stale IAM keys every cleanupInterval. Anything
// whose EncodingJob row is older than staleKeyAge and still has an
// access key recorded gets deleted unconditionally — this is the
// safety net for crashes between mint and a successful completion.
func (o *Orchestrator) cleanupLoop(ctx context.Context) {
	// Run once on startup so a restart shortly after a crash picks up
	// the orphan immediately instead of waiting an hour.
	o.sweepStaleKeys(ctx)
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.sweepStaleKeys(ctx)
		}
	}
}

func (o *Orchestrator) sweepStaleKeys(ctx context.Context) {
	stale, err := o.dao.ListStaleEncodingJobKeys(ctx, staleKeyAge)
	if err != nil {
		o.log.Error("sweep: list stale keys", "err", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	var deleted int
	for _, row := range stale {
		if err := o.iam.DeleteScopedKey(ctx, row.AccessKeyID); err != nil {
			o.log.Warn("sweep: delete key", "err", err, "job_id", row.ID, "access_key_id", row.AccessKeyID)
			continue
		}
		if err := o.dao.ClearEncodingJobAccessKey(ctx, row.ID); err != nil {
			o.log.Warn("sweep: clear column", "err", err, "job_id", row.ID)
			continue
		}
		deleted++
	}
	o.log.Info("swept stale iam keys", "found", len(stale), "deleted", deleted)
}

// --- Webhook handler -------------------------------------------------------

// WebhookStatus is the value of the "status" field on the encoder's callback.
type WebhookStatus string

const (
	WebhookSucceeded WebhookStatus = "succeeded"
	WebhookFailed    WebhookStatus = "failed"
)

type WebhookBody struct {
	JobID  string        `json:"job_id"`
	Status WebhookStatus `json:"status"`
	Reason string        `json:"reason,omitempty"`
	Logs   string        `json:"logs,omitempty"`
}

// SignWebhookBody returns the X-Webhook-Signature value for body+secret.
func SignWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func verifySignature(secret string, body []byte, header string) bool {
	expected := SignWebhookBody(secret, body)
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(header)))
}

func (o *Orchestrator) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 128<<10))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var msg WebhookBody
	if err := json.Unmarshal(body, &msg); err != nil || msg.JobID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	job, err := o.dao.GetEncodingJob(r.Context(), msg.JobID)
	if err != nil {
		o.log.Warn("webhook: unknown job", "id", msg.JobID, "err", err)
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if !verifySignature(job.WebhookSecret, body, r.Header.Get(webhookSigHdr)) {
		o.log.Warn("webhook: bad signature", "id", msg.JobID)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	switch msg.Status {
	case WebhookSucceeded:
		o.completeJob(r.Context(), job, true, "", "")
	case WebhookFailed:
		reason := msg.Reason
		if reason == "" {
			reason = "encoder reported failure"
		}
		o.completeJob(r.Context(), job, false, reason, msg.Logs)
	default:
		http.Error(w, fmt.Sprintf("unknown status %q", msg.Status), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
