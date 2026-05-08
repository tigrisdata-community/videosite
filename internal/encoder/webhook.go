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
	"net/http"
	"strings"

	"tangled.org/xeiaso.net/videosite/internal/models"
)

const webhookSigHeader = "X-Webhook-Signature"

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
}

// SignWebhookBody returns the X-Webhook-Signature value for body+secret.
func SignWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature is constant-time and tolerates a missing "sha256=" prefix.
func verifySignature(secret string, body []byte, header string) bool {
	expected := SignWebhookBody(secret, body)
	got := strings.TrimSpace(header)
	return hmac.Equal([]byte(expected), []byte(got))
}

// handleWebhook is mounted at POST /api/encode-callback. The orchestrator
// calls back into this when a webhook lands so it can short-circuit the
// janitor's polling and clean up the instance + IAM key sooner.
func (o *Orchestrator) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var msg WebhookBody
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "decode body", http.StatusBadRequest)
		return
	}
	if msg.JobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	job, err := o.dao.GetEncodingJob(r.Context(), msg.JobID)
	if err != nil {
		o.log.Warn("webhook: unknown job", "id", msg.JobID, "err", err)
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}

	if !verifySignature(job.WebhookSecret, body, r.Header.Get(webhookSigHeader)) {
		o.log.Warn("webhook: bad signature", "id", msg.JobID)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	switch msg.Status {
	case WebhookSucceeded:
		if err := o.completeJob(r.Context(), job, true, ""); err != nil {
			o.log.Error("webhook: complete succeeded", "err", err, "id", job.ID)
			http.Error(w, "complete", http.StatusInternalServerError)
			return
		}
	case WebhookFailed:
		reason := msg.Reason
		if reason == "" {
			reason = "encoder reported failure"
		}
		if err := o.completeJob(r.Context(), job, false, reason); err != nil {
			o.log.Error("webhook: complete failed", "err", err, "id", job.ID)
			http.Error(w, "complete", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("unknown status %q", msg.Status), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// errAlreadyTerminal is returned by completeJob when the job is no longer in
// a state that can be completed (already succeeded/failed). Treated as a
// no-op by callers.
var errAlreadyTerminal = errors.New("encoder: job already terminal")

// completeJob is shared between the webhook and the janitor: it transitions
// the EncodingJob and the underlying Video together, then async-cleans up.
func (o *Orchestrator) completeJob(ctx context.Context, job *models.EncodingJob, ok bool, reason string) error {
	if ok {
		if err := o.dao.MarkEncodingJobSucceeded(ctx, job.ID); err != nil {
			if errors.Is(err, models.ErrConflict) {
				return errAlreadyTerminal
			}
			return err
		}
		if err := o.dao.MarkVideoReady(ctx, job.VideoID); err != nil && !errors.Is(err, models.ErrConflict) {
			o.log.Error("mark video ready", "err", err, "video_id", job.VideoID)
		}
	} else {
		if err := o.dao.MarkEncodingJobFailed(ctx, job.ID, reason); err != nil {
			if errors.Is(err, models.ErrConflict) {
				return errAlreadyTerminal
			}
			return err
		}
		if err := o.dao.MarkVideoFailed(ctx, job.VideoID, reason); err != nil && !errors.Is(err, models.ErrConflict) {
			o.log.Error("mark video failed", "err", err, "video_id", job.VideoID)
		}
	}

	go o.cleanup(job)
	return nil
}
