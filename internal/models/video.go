package models

import "gorm.io/gorm"

type Video struct {
	gorm.Model
	ID     string `gorm:"uniqueIndex"`
	Status VideoStatus
}

type VideoStatus string

const (
	VideoStatusUploaded = "uploaded"
	VideoStatusEncoding = "encoding"
	VideoStatusReady    = "ready"
)
