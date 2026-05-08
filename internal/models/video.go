package models

import "gorm.io/gorm"

type Video struct {
	gorm.Model
	ID            string `gorm:"uniqueIndex"`
	Filename      string
	Status        VideoStatus
	FailureReason string
}

type VideoStatus string

const (
	VideoStatusUploading VideoStatus = "uploading"
	VideoStatusUploaded  VideoStatus = "uploaded"
	VideoStatusEncoding  VideoStatus = "encoding"
	VideoStatusReady     VideoStatus = "ready"
	VideoStatusFailed    VideoStatus = "failed"
)
