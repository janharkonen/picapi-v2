package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"golang.org/x/sync/singleflight"
)

type server struct {
	cfg        Config
	db         *DB
	storage    *Storage
	transforms chan struct{} // semaphore
	inflight   singleflight.Group
	storing    sync.Map
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Error("failed to load .env", "error", err)
		os.Exit(1)
	}
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	vips.LoggingSettings(nil, vips.LogLevelWarning)
	if err := vips.Startup(nil); err != nil {
		logger.Error("failed to start libvips", "error", err)
		os.Exit(1)
	}
	defer vips.Shutdown()

	db, err := openDB(cfg.DatabasePath)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	if cfg.BootstrapKey != "" {
		created, err := db.ensureBootstrapKey(cfg.BootstrapKey)
		if err != nil {
			logger.Error("failed to create bootstrap API key", "error", err)
			os.Exit(1)
		}
		if created {
			logger.Info("created bootstrap API key")
		}
	}

	s := &server{
		cfg:        cfg,
		db:         db,
		storage:    newStorage(cfg.S3),
		transforms: make(chan struct{}, cfg.TransformConcurrency),
	}

	e := echo.New()
	e.Logger = logger
	e.HTTPErrorHandler = errorHandler
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())

	e.GET("/healthcheck", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	api := e.Group("/api/v1", s.requireAPIKey)
	api.POST("/assets", s.uploadAsset, middleware.BodyLimit(cfg.MaxUploadBytes+1024*1024))
	api.GET("/assets", s.listAssets)
	api.GET("/assets/:id", s.getAsset)
	api.PATCH("/assets/:id", s.patchAsset)
	api.DELETE("/assets/:id", s.deleteAsset)
	api.POST("/assets/:id/url", s.assetURL)
	api.POST("/assets/:id/share", s.shareAsset)

	for _, path := range []string{"/i/:id", "/i/:id/:a", "/i/:id/:a/:b"} {
		e.GET(path, s.deliver)
		e.HEAD(path, s.deliver)
	}

	logger.Info("s3 storage configured", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket, "prefix", cfg.S3.EnvPrefix)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sc := echo.StartConfig{Address: ":" + strconv.Itoa(cfg.Port), HideBanner: true}
	if err := sc.Start(ctx, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
	}
}

// errorHandler writes every error as {"error": "..."} and never lets it be cached.
func errorHandler(c *echo.Context, err error) {
	if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
		return
	}
	code := http.StatusInternalServerError
	message := "internal server error"
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		code = httpErr.Code
		message = httpErr.Message
	} else if sc := echo.StatusCode(err); sc != 0 {
		code = sc
		message = strings.ToLower(http.StatusText(sc))
	} else {
		c.Logger().Error("request failed", "error", err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	if c.Request().Method == http.MethodHead {
		c.NoContent(code)
		return
	}
	c.JSON(code, map[string]string{"error": message})
}
