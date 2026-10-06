package main

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

func main() {
  // Echo instance
  e := echo.New()

  // Middleware
  e.Use(middleware.RequestLogger())
  e.Use(middleware.Recover())

  // Route => handler
  e.GET("/healthcheck", func(c *echo.Context) error {
    return c.String(http.StatusOK, "ok")
  })

  // Start server
  sc := echo.StartConfig{Address: ":1323"}
  if err := sc.Start(context.Background(), e); err != nil {
    e.Logger.Error("failed to start server", "error", err)
  }
}
