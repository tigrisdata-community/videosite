package models

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func newTestDAO(t *testing.T) *DAO {
	t.Helper()
	dbLoc := filepath.Join(t.TempDir(), "test.db")
	dao, err := New(dbLoc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("init dao: %v", err)
	}
	return dao
}

func TestCreateAndGetEncodingJob(t *testing.T) {
	ctx := context.Background()
	dao := newTestDAO(t)

	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	job, err := dao.CreateEncodingJob(ctx, "vid-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.ID == "" || job.WebhookSecret == "" || len(job.WebhookSecret) != 64 {
		t.Errorf("unexpected job: id=%q secret-len=%d", job.ID, len(job.WebhookSecret))
	}
	if job.Status != EncodingJobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}

	got, err := dao.GetEncodingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.VideoID != "vid-1" {
		t.Errorf("video id = %q, want vid-1", got.VideoID)
	}
}

func TestClaimPendingEncodingJob(t *testing.T) {
	ctx := context.Background()
	dao := newTestDAO(t)

	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	t.Run("returns ErrNoPending when empty", func(t *testing.T) {
		_, err := dao.ClaimPendingEncodingJob(ctx)
		if !errors.Is(err, ErrNoPending) {
			t.Errorf("err = %v, want ErrNoPending", err)
		}
	})

	t.Run("transitions pending to launching", func(t *testing.T) {
		j, err := dao.CreateEncodingJob(ctx, "vid-1")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := dao.ClaimPendingEncodingJob(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got.ID != j.ID {
			t.Errorf("id = %q, want %q", got.ID, j.ID)
		}
		if got.Status != EncodingJobLaunching {
			t.Errorf("status = %q, want launching", got.Status)
		}
		// cleanup
		if err := dao.MarkEncodingJobFailed(ctx, j.ID, "test cleanup"); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("second claim of same row returns ErrNoPending", func(t *testing.T) {
		j, err := dao.CreateEncodingJob(ctx, "vid-1")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		first, err := dao.ClaimPendingEncodingJob(ctx)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if first.ID != j.ID {
			t.Errorf("claimed id = %q, want %q", first.ID, j.ID)
		}
		if _, err := dao.ClaimPendingEncodingJob(ctx); !errors.Is(err, ErrNoPending) {
			t.Errorf("second claim err = %v, want ErrNoPending", err)
		}
		// cleanup
		_ = dao.MarkEncodingJobFailed(ctx, j.ID, "test cleanup")
	})
}

func TestEncodingJobStateMachine(t *testing.T) {
	ctx := context.Background()
	dao := newTestDAO(t)
	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T, dao *DAO, jobID string)
		mutate  func(dao *DAO, jobID string) error
		wantErr error
	}{
		{
			name:  "running -> succeeded",
			setup: stepThroughTo(EncodingJobRunning),
			mutate: func(d *DAO, id string) error {
				return d.MarkEncodingJobSucceeded(ctx, id)
			},
		},
		{
			name:  "succeeded cannot transition to failed",
			setup: stepThroughTo(EncodingJobSucceeded),
			mutate: func(d *DAO, id string) error {
				return d.MarkEncodingJobFailed(ctx, id, "boom")
			},
			wantErr: ErrConflict,
		},
		{
			name:  "launching -> failed (mint failure)",
			setup: stepThroughTo(EncodingJobLaunching),
			mutate: func(d *DAO, id string) error {
				return d.MarkEncodingJobFailed(ctx, id, "no offers")
			},
		},
		{
			name:  "running -> failed (encoder reported failure)",
			setup: stepThroughTo(EncodingJobRunning),
			mutate: func(d *DAO, id string) error {
				return d.MarkEncodingJobFailed(ctx, id, "ffmpeg exit 1")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, err := dao.CreateEncodingJob(ctx, "vid-1")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			tt.setup(t, dao, job.ID)

			err = tt.mutate(dao, job.ID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}

// stepThroughTo returns a setup func that drives a freshly created
// EncodingJob through the state machine until it reaches `target`.
func stepThroughTo(target EncodingJobStatus) func(t *testing.T, dao *DAO, jobID string) {
	return func(t *testing.T, dao *DAO, jobID string) {
		t.Helper()
		ctx := context.Background()
		if target == EncodingJobPending {
			return
		}
		if _, err := dao.ClaimPendingEncodingJob(ctx); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if target == EncodingJobLaunching {
			return
		}
		if err := dao.MarkEncodingJobRunning(ctx, jobID, 42, "tid_test", 0.25); err != nil {
			t.Fatalf("running: %v", err)
		}
		if target == EncodingJobRunning {
			return
		}
		if target == EncodingJobSucceeded {
			if err := dao.MarkEncodingJobSucceeded(ctx, jobID); err != nil {
				t.Fatalf("succeeded: %v", err)
			}
			return
		}
		if target == EncodingJobFailed {
			if err := dao.MarkEncodingJobFailed(ctx, jobID, "test"); err != nil {
				t.Fatalf("failed: %v", err)
			}
			return
		}
	}
}
