package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

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

	// Middleware
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())

	// Route => handler
	e.GET("/healthcheck", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	e.GET("/image", func(c *echo.Context) error {
		fmt.Println("s3cfg", s3cfg.Endpoint, s3cfg.Bucket, s3cfg.Region, s3cfg.AccessKeyID, s3cfg.SecretAccessKey)
		return c.String(http.StatusOK, "image")
	})

	// Start server
	sc := echo.StartConfig{Address: ":1323"}
	if err := sc.Start(context.Background(), e); err != nil {
		e.Logger.Error("failed to start server", "error", err)
	}
}
