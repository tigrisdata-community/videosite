package models

import (
	"context"
	"fmt"
)

func (d *DAO) CreateVideo(ctx context.Context, id, filename string) (*Video, error) {
	v := &Video{ID: id, Filename: filename, Status: VideoStatusUploaded}
	if err := d.db.WithContext(ctx).Create(v).Error; err != nil {
		return nil, fmt.Errorf("models: create video %q: %w", id, err)
	}
	return v, nil
}

func (d *DAO) MarkVideoUploaded(ctx context.Context, id string) error {
	res := d.db.WithContext(ctx).Model(&Video{}).Where("id = ?", id).Update("status", VideoStatusUploaded)
	if res.Error != nil {
		return fmt.Errorf("models: mark video uploaded %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("models: mark video uploaded %q: not found", id)
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
