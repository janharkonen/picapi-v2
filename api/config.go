package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// S3Config holds settings for an S3-compatible object store.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	EnvPrefix       string
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool
}

func loadS3Config() (S3Config, error) {
	cfg := S3Config{
		Endpoint:        os.Getenv("S3_ENDPOINT"),
		Region:          os.Getenv("S3_REGION"),
		Bucket:          os.Getenv("S3_BUCKET"),
		EnvPrefix:       os.Getenv("S3_ENV_PREFIX"),
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
	}

	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "S3_ENDPOINT")
	}
	if cfg.Region == "" {
		missing = append(missing, "S3_REGION")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "S3_BUCKET")
	}
	if cfg.AccessKeyID == "" {
		missing = append(missing, "S3_ACCESS_KEY_ID")
	}
	if cfg.SecretAccessKey == "" {
		missing = append(missing, "S3_SECRET_ACCESS_KEY")
	}
	if len(missing) > 0 {
		return S3Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	usePathStyle := os.Getenv("S3_USE_PATH_STYLE")
	if usePathStyle == "" {
		cfg.UsePathStyle = true
		return cfg, nil
	}

	parsed, err := strconv.ParseBool(usePathStyle)
	if err != nil {
		return S3Config{}, fmt.Errorf("S3_USE_PATH_STYLE must be a boolean")
	}
	cfg.UsePathStyle = parsed
	return cfg, nil
}
