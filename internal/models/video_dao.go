package models

import (
	"context"
	"errors"
	"fmt"
)

var ErrConflict = errors.New("video status conflict or not found")

func (d *DAO) transition(ctx context.Context, id string, from, to VideoStatus) error {
	res := d.db.WithContext(ctx).Model(&Video{}).
		Where("id = ? AND status = ?", id, string(from)).
		Update("status", string(to))
	if res.Error != nil {
		return fmt.Errorf("models: transition video %q %s->%s: %w", id, from, to, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: transition video %q %s->%s: %w", id, from, to, ErrConflict)
	}
	return nil
}

func (d *DAO) CreateVideo(ctx context.Context, id, filename string) (*Video, error) {
	v := &Video{ID: id, Filename: filename, Status: VideoStatusUploading}
	if err := d.db.WithContext(ctx).Create(v).Error; err != nil {
		return nil, fmt.Errorf("models: create video %q: %w", id, err)
	}
	return v, nil
}

func (d *DAO) MarkVideoUploaded(ctx context.Context, id string) error {
	return d.transition(ctx, id, VideoStatusUploading, VideoStatusUploaded)
}

func (d *DAO) MarkVideoEncoding(ctx context.Context, id string) error {
	return d.transition(ctx, id, VideoStatusUploaded, VideoStatusEncoding)
}

func (d *DAO) MarkVideoReady(ctx context.Context, id string) error {
	return d.transition(ctx, id, VideoStatusEncoding, VideoStatusReady)
}

func (d *DAO) MarkVideoFailed(ctx context.Context, id, reason string) error {
	res := d.db.WithContext(ctx).Model(&Video{}).
		Where("id = ? AND status <> ?", id, string(VideoStatusReady)).
		Updates(map[string]any{"status": string(VideoStatusFailed), "failure_reason": reason})
	if res.Error != nil {
		return fmt.Errorf("models: mark video failed %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: mark video failed %q: %w", id, ErrConflict)
	}
	return nil
}

func (d *DAO) MarkVideoFailedWithLogs(ctx context.Context, id, reason, logs string) error {
	res := d.db.WithContext(ctx).Model(&Video{}).
		Where("id = ? AND status <> ?", id, string(VideoStatusReady)).
		Updates(map[string]any{
			"status":         string(VideoStatusFailed),
			"failure_reason": reason,
			"encode_logs":    logs,
		})
	if res.Error != nil {
		return fmt.Errorf("models: mark video failed %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: mark video failed %q: %w", id, ErrConflict)
	}
	return nil
}

func (d *DAO) GetVideo(ctx context.Context, id string) (*Video, error) {
	var v Video
	if err := d.db.WithContext(ctx).Where("id = ?", id).First(&v).Error; err != nil {
		return nil, fmt.Errorf("models: get video %q: %w", id, err)
	}
	return &v, nil
}
