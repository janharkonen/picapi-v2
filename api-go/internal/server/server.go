// Package server implements the picapi HTTP server: management API, image delivery,
// MCP endpoint and web UI. The S3 bucket is its only storage.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"golang.org/x/sync/singleflight"
)

type Server struct {
	cfg        Config
	logger     *slog.Logger
	storage    *Storage
	purger     *purger
	bg         *bgRemover
	transforms chan struct{} // semaphore
	inflight   singleflight.Group
	events     eventHub
}

// Run starts the server and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	vips.LoggingSettings(nil, vips.LogLevelWarning)
	if err := vips.Startup(nil); err != nil {
		return err
	}
	defer vips.Shutdown()

	s := &Server{
		cfg:        cfg,
		logger:     logger,
		storage:    newStorage(cfg.S3),
		purger:     newPurger(cfg),
		bg:         newBGRemover(cfg),
		transforms: make(chan struct{}, cfg.TransformConcurrency),
	}

	logger.Info("picapi starting",
		"s3_endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket, "prefix", s.storage.prefix,
		"cloudflare_purge", s.purger.enabled(), "bg_model", cfg.BGModel, "frontend", cfg.FrontendURL)
	sc := echo.StartConfig{Address: ":" + strconv.Itoa(cfg.Port), HideBanner: true}
	if err := sc.Start(ctx, s.routes()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) routes() *echo.Echo {
	e := echo.New()
	e.Logger = s.logger
	e.HTTPErrorHandler = errorHandler
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogMethod: true, LogURI: true, LogLatency: true, HandleError: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			if v.Status >= 500 {
				level = slog.LevelError
			}
			s.logger.Log(c.Request().Context(), level, "request",
				"method", v.Method, "uri", v.URI, "status", v.Status, "latency", v.Latency)
			return nil
		},
	}))
	e.Use(middleware.Recover())

	e.GET("/healthcheck", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	// Session endpoints authenticate with the key in the body.
	e.POST("/api/v1/session", s.login)
	e.DELETE("/api/v1/session", s.logout)

	api := e.Group("/api/v1", s.requireAuth)
	api.GET("/session", s.sessionInfo)
	api.POST("/assets", s.uploadAsset, middleware.BodyLimit(s.cfg.MaxUploadBytes+1024*1024))
	api.GET("/assets", s.listAssets)
	api.GET("/assets/:id", s.getAsset)
	api.DELETE("/assets/:id", s.deleteAsset)
	api.POST("/assets/:id/url", s.assetURL)
	api.GET("/events", s.streamEvents)

	mcpHandler := echo.WrapHandler(s.mcpHandler())
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		e.Add(method, "/mcp", mcpHandler, s.requireAuth)
	}

	for _, path := range []string{"/i/:id", "/i/:id/:a", "/i/:id/:a/:b"} {
		e.GET(path, s.deliver)
		e.HEAD(path, s.deliver)
	}

	s.registerFrontend(e)
	return e
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
		c.Logger().Error("request failed", "method", c.Request().Method, "path", c.Request().URL.Path, "error", err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	if c.Request().Method == http.MethodHead {
		c.NoContent(code)
		return
	}
	c.JSON(code, map[string]string{"error": message})
}
