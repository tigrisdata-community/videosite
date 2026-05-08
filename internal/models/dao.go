package models

import (
	"fmt"
	"log/slog"

	"github.com/ncruces/go-sqlite3/gormlite"
	slogGorm "github.com/orandin/slog-gorm"
	"gorm.io/gorm"
	gormPrometheus "gorm.io/plugin/prometheus"
)

type DAO struct {
	db *gorm.DB
}

func New(dbLoc string, lg *slog.Logger) (*DAO, error) {
	db, err := gorm.Open(gormlite.Open(dbLoc), &gorm.Config{
		Logger: slogGorm.New(
			slogGorm.WithHandler(lg.Handler()),
			slogGorm.WithErrorField("err"),
			slogGorm.WithRecordNotFoundError(),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := db.AutoMigrate(&Video{}, &EncodingJob{}); err != nil {
		return nil, fmt.Errorf("failed to migrate schema: %w", err)
	}

	db.Use(gormPrometheus.New(gormPrometheus.Config{
		DBName: "videosite",
	}))

	return &DAO{db: db}, nil
}
