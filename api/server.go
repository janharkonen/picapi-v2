package main

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

func main() {
	// Echo instance
	e := echo.New()

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		e.Logger.Error("failed to load .env", "error", err)
		os.Exit(1)
	}

	s3cfg, err := loadS3Config()
	if err != nil {
		e.Logger.Error("invalid s3 configuration", "error", err)
		os.Exit(1)
	}
	e.Logger.Info("s3 storage configured", "endpoint", s3cfg.Endpoint, "bucket", s3cfg.Bucket, "region", s3cfg.Region)
	s3client := newS3Client(s3cfg)

	// Middleware
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())

	// Route => handler
	e.GET("/healthcheck", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	e.GET("/image_fetch", func(c *echo.Context) error {
		objectKey := "dev/00000040052441_A1N1.png"
		obj, err := fetchObject(c.Request().Context(), s3client, s3cfg.Bucket, objectKey)
		if err != nil {
			c.Logger().Error("failed to fetch image", "error", err, "key", objectKey)
			return echo.NewHTTPError(http.StatusBadGateway, "failed to fetch image")
		}
		defer obj.Body.Close()

		if obj.Size > 0 {
			c.Response().Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		}
		c.Response().Header().Set("Content-Disposition", "inline")
		return c.Stream(http.StatusOK, obj.ContentType, obj.Body)
	})

	e.GET("/image_redirect", func(c *echo.Context) error {
		objectKey := "dev/00000040052441_A1N1.png"
		url, err := presignGetObjectURL(c.Request().Context(), s3client, s3cfg.Bucket, objectKey)
		if err != nil {
			c.Logger().Error("failed to presign image", "error", err, "key", objectKey)
			return echo.NewHTTPError(http.StatusBadGateway, "failed to presign image")
		}

		// Do not cache the redirect. The presigned URL expires.
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.Redirect(http.StatusFound, url)
	})

	// Start server
	sc := echo.StartConfig{Address: ":1323"}
	if err := sc.Start(context.Background(), e); err != nil {
		e.Logger.Error("failed to start server", "error", err)
	}
}
