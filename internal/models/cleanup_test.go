package models

import (
	"context"
	"testing"
	"time"
)

// TestListStaleEncodingJobKeys covers the hourly cleanup-sweep DAO:
// only rows with both a non-empty access key id and a CreatedAt older
// than the threshold should come back.
func TestListStaleEncodingJobKeys(t *testing.T) {
	ctx := context.Background()
	dao := newTestDAO(t)
	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	now := time.Now()
	type seed struct {
		id        string
		accessKey string
		createdAt time.Time
	}
	seeds := []seed{
		{id: "fresh-with-key", accessKey: "tid_keep", createdAt: now.Add(-1 * time.Hour)},
		{id: "stale-with-key", accessKey: "tid_kill", createdAt: now.Add(-49 * time.Hour)},
		{id: "stale-no-key", accessKey: "", createdAt: now.Add(-72 * time.Hour)},
		{id: "very-stale-with-key", accessKey: "tid_kill_2", createdAt: now.Add(-7 * 24 * time.Hour)},
	}
	for _, s := range seeds {
		j := &EncodingJob{
			ID:                s.id,
			VideoID:           "vid-1",
			Status:            EncodingJobRunning,
			TigrisAccessKeyID: s.accessKey,
			WebhookSecret:     "x",
		}
		if err := dao.db.WithContext(ctx).Create(j).Error; err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
		// gorm's CreatedAt autopopulation overrides what we'd hand in.
		// Override it explicitly so the threshold actually triggers.
		if err := dao.db.WithContext(ctx).Model(&EncodingJob{}).
			Where("id = ?", s.id).
			Update("created_at", s.createdAt).Error; err != nil {
			t.Fatalf("backdate %s: %v", s.id, err)
		}
	}

	got, err := dao.ListStaleEncodingJobKeys(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}

	wantIDs := map[string]string{
		"stale-with-key":      "tid_kill",
		"very-stale-with-key": "tid_kill_2",
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d stale rows, want %d: %+v", len(got), len(wantIDs), got)
	}
	for _, row := range got {
		wantKey, ok := wantIDs[row.ID]
		if !ok {
			t.Errorf("unexpected row id %q in result", row.ID)
			continue
		}
		if row.AccessKeyID != wantKey {
			t.Errorf("row %q: access key = %q, want %q", row.ID, row.AccessKeyID, wantKey)
		}
	}
}

func TestClearEncodingJobAccessKey(t *testing.T) {
	ctx := context.Background()
	dao := newTestDAO(t)
	if _, err := dao.CreateVideo(ctx, "vid-1", "f.mp4"); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	job, err := dao.CreateEncodingJob(ctx, "vid-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := dao.db.WithContext(ctx).Model(&EncodingJob{}).
		Where("id = ?", job.ID).
		Update("tigris_access_key_id", "tid_target").Error; err != nil {
		t.Fatalf("set key: %v", err)
	}

	if err := dao.ClearEncodingJobAccessKey(ctx, job.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got, err := dao.GetEncodingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TigrisAccessKeyID != "" {
		t.Errorf("access key = %q, want empty", got.TigrisAccessKeyID)
	}
	if got.Status != EncodingJobPending {
		t.Errorf("status = %q, want pending (clear must not touch status)", got.Status)
	}
}
