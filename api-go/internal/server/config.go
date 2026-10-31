package server

import (
	"fmt"
	"os"
	"runtime"
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

type Config struct {
	Port                 int
	PublicBaseURL        string
	SigningSecret        string
	APIKey               string
	FrontendURL          string
	S3                   S3Config
	TransformVersion     string
	MaxUploadBytes       int64
	MaxInputPixels       int
	TransformConcurrency int
	CloudflareZoneID     string
	CloudflareAPIToken   string
	BGModel              string
	BGModelDir           string
	BGConcurrency        int
	OnnxRuntimeLib       string
}

func LoadConfig() (Config, error) {
	var missing []string
	for _, name := range []string{
		"S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY",
		"PUBLIC_BASE_URL", "SIGNING_SECRET", "PICAPI_API_KEY",
	} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	cfg := Config{
		PublicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		SigningSecret: os.Getenv("SIGNING_SECRET"),
		APIKey:        os.Getenv("PICAPI_API_KEY"),
		FrontendURL:   strings.TrimRight(os.Getenv("FRONTEND_URL"), "/"),
		S3: S3Config{
			Endpoint:        os.Getenv("S3_ENDPOINT"),
			Region:          os.Getenv("S3_REGION"),
			Bucket:          os.Getenv("S3_BUCKET"),
			EnvPrefix:       os.Getenv("S3_ENV_PREFIX"),
			AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		},
		TransformVersion:   envOr("TRANSFORM_VERSION", "1"),
		CloudflareZoneID:   os.Getenv("CLOUDFLARE_ZONE_ID"),
		CloudflareAPIToken: os.Getenv("CLOUDFLARE_API_TOKEN"),
		BGModel:            envOr("BG_MODEL", "isnet"),
		BGModelDir:         envOr("BG_MODEL_DIR", "/opt/picapi/models"),
		OnnxRuntimeLib:     envOr("ONNXRUNTIME_LIB", "/usr/local/lib/libonnxruntime.so"),
	}
	if len(cfg.SigningSecret) < 32 {
		return Config{}, fmt.Errorf("SIGNING_SECRET must be at least 32 characters")
	}
	if len(cfg.APIKey) < 32 {
		return Config{}, fmt.Errorf("PICAPI_API_KEY must be at least 32 characters")
	}
	if (cfg.CloudflareZoneID == "") != (cfg.CloudflareAPIToken == "") {
		return Config{}, fmt.Errorf("CLOUDFLARE_ZONE_ID and CLOUDFLARE_API_TOKEN must be set together")
	}
	if _, ok := bgModels[cfg.BGModel]; !ok && cfg.BGModel != "off" {
		return Config{}, fmt.Errorf("BG_MODEL must be isnet, u2netp or off")
	}

	var err error
	if cfg.S3.UsePathStyle, err = boolEnv("S3_USE_PATH_STYLE", true); err != nil {
		return Config{}, err
	}
	if cfg.Port, err = intEnv("PORT", 8080); err != nil {
		return Config{}, err
	}
	maxUpload, err := intEnv("MAX_UPLOAD_BYTES", 50*1024*1024)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxUploadBytes = int64(maxUpload)
	if cfg.MaxInputPixels, err = intEnv("MAX_INPUT_PIXELS", 100_000_000); err != nil {
		return Config{}, err
	}
	if cfg.TransformConcurrency, err = intEnv("TRANSFORM_CONCURRENCY", runtime.NumCPU()); err != nil {
		return Config{}, err
	}
	if cfg.BGConcurrency, err = intEnv("BG_CONCURRENCY", 1); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func intEnv(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func boolEnv(name string, fallback bool) (bool, error) {
	switch strings.ToLower(os.Getenv(name)) {
	case "":
		return fallback, nil
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s must be a boolean", name)
}
