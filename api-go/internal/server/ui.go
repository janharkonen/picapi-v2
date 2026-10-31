package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v5"
)

//go:embed ui
var uiFiles embed.FS

// registerUI serves the embedded web UI at /. It authenticates with an API key and then
// uses the session cookie for the management API.
func (s *Server) registerUI(e *echo.Echo) {
	sub, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServer(http.FS(sub))
	handler := func(c *echo.Context) error {
		h := c.Response().Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		files.ServeHTTP(c.Response(), c.Request())
		return nil
	}
	e.GET("/", handler)
	e.GET("/app.js", handler)
	e.GET("/style.css", handler)
}
