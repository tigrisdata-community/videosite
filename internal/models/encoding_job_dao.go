package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrNoPending = errors.New("no pending encoding jobs")

func (d *DAO) transitionEncodingJob(ctx context.Context, id string, from, to EncodingJobStatus, extra map[string]any) error {
	updates := map[string]any{"status": string(to)}
	maps.Copy(updates, extra)
	res := d.db.WithContext(ctx).Model(&EncodingJob{}).
		Where("id = ? AND status = ?", id, string(from)).
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("models: transition encoding job %q %s->%s: %w", id, from, to, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: transition encoding job %q %s->%s: %w", id, from, to, ErrConflict)
	}
	return nil
}

func (d *DAO) CreateEncodingJob(ctx context.Context, videoID string) (*EncodingJob, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("models: create encoding job: read random: %w", err)
	}
	j := &EncodingJob{
		ID:            uuid.NewString(),
		VideoID:       videoID,
		Status:        EncodingJobPending,
		WebhookSecret: hex.EncodeToString(secret),
	}
	if err := d.db.WithContext(ctx).Create(j).Error; err != nil {
		return nil, fmt.Errorf("models: create encoding job for video %q: %w", videoID, err)
	}
	return j, nil
}

// ClaimPendingEncodingJob atomically picks one pending job and transitions it
// to launching. Returns ErrNoPending when no pending job is available so the
// caller can sleep without treating it as a hard error.
func (d *DAO) ClaimPendingEncodingJob(ctx context.Context) (*EncodingJob, error) {
	var job *EncodingJob
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var j EncodingJob
		if err := tx.Where("status = ?", string(EncodingJobPending)).
			Order("created_at ASC").
			First(&j).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNoPending
			}
			return err
		}
		res := tx.Model(&EncodingJob{}).
			Where("id = ? AND status = ?", j.ID, string(EncodingJobPending)).
			Update("status", string(EncodingJobLaunching))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNoPending
		}
		j.Status = EncodingJobLaunching
		job = &j
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoPending) {
			return nil, ErrNoPending
		}
		return nil, fmt.Errorf("models: claim pending encoding job: %w", err)
	}
	return job, nil
}

func (d *DAO) MarkEncodingJobRunning(ctx context.Context, id string, vastInstanceID int, accessKeyID string, dph float64) error {
	now := time.Now()
	return d.transitionEncodingJob(ctx, id, EncodingJobLaunching, EncodingJobRunning, map[string]any{
		"vast_instance_id":     vastInstanceID,
		"tigris_access_key_id": accessKeyID,
		"dph_total":            dph,
		"started_at":           &now,
	})
}

func (d *DAO) MarkEncodingJobSucceeded(ctx context.Context, id string) error {
	now := time.Now()
	return d.transitionEncodingJob(ctx, id, EncodingJobRunning, EncodingJobSucceeded, map[string]any{
		"completed_at": &now,
	})
}

// MarkEncodingJobFailed transitions a non-succeeded job to failed. Mirrors
// MarkVideoFailed's guard: a job that's already succeeded can't be regressed.
func (d *DAO) MarkEncodingJobFailed(ctx context.Context, id, reason string) error {
	now := time.Now()
	res := d.db.WithContext(ctx).Model(&EncodingJob{}).
		Where("id = ? AND status <> ?", id, string(EncodingJobSucceeded)).
		Updates(map[string]any{
			"status":         string(EncodingJobFailed),
			"failure_reason": reason,
			"completed_at":   &now,
		})
	if res.Error != nil {
		return fmt.Errorf("models: mark encoding job failed %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: mark encoding job failed %q: %w", id, ErrConflict)
	}
	return nil
}

func (d *DAO) GetEncodingJob(ctx context.Context, id string) (*EncodingJob, error) {
	var j EncodingJob
	if err := d.db.WithContext(ctx).Where("id = ?", id).First(&j).Error; err != nil {
		return nil, fmt.Errorf("models: get encoding job %q: %w", id, err)
	}
	return &j, nil
}

// ListEncodingJobsForJanitor returns jobs that the janitor needs to reconcile:
// launching (orchestrator died after claim, before mint completed) and
// running (encoder may have crashed without firing the webhook).
func (d *DAO) ListEncodingJobsForJanitor(ctx context.Context) ([]*EncodingJob, error) {
	var jobs []*EncodingJob
	err := d.db.WithContext(ctx).
		Where("status IN ?", []string{string(EncodingJobLaunching), string(EncodingJobRunning)}).
		Find(&jobs).Error
	if err != nil {
		return nil, fmt.Errorf("models: list jobs for janitor: %w", err)
	}
	return jobs, nil
}
