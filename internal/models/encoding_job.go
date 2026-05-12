package models

import (
	"time"

	"gorm.io/gorm"
)

type EncodingJob struct {
	gorm.Model
	ID                string `gorm:"uniqueIndex"`
	VideoID           string `gorm:"index"`
	Status            EncodingJobStatus
	VastInstanceID    int
	TigrisAccessKeyID string
	WebhookSecret     string
	StartedAt         *time.Time
	CompletedAt       *time.Time
	FailureReason     string
	DphTotal          float64
}

// StaleEncodingJobKey is the per-row payload returned by
// ListStaleEncodingJobKeys — just enough to delete the scoped key and
// clear its column.
type StaleEncodingJobKey struct {
	ID          string
	AccessKeyID string
}

type EncodingJobStatus string

const (
	EncodingJobPending   EncodingJobStatus = "pending"
	EncodingJobLaunching EncodingJobStatus = "launching"
	EncodingJobRunning   EncodingJobStatus = "running"
	EncodingJobSucceeded EncodingJobStatus = "succeeded"
	EncodingJobFailed    EncodingJobStatus = "failed"
)
