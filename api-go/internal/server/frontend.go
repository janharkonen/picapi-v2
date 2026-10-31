package server

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/labstack/echo/v5"
)

// The SvelteKit frontend runs as a separate, unexposed service. "/" redirects to "/ui",
// and everything under /ui is reverse-proxied to FRONTEND_URL, so the frontend shares the
// public origin (and its session cookie) without a public domain of its own.

const frontendBase = "/ui"

func (s *Server) registerFrontend(e *echo.Echo) {
	e.GET("/", func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.Redirect(http.StatusFound, frontendBase)
	})

	var handler echo.HandlerFunc
	if s.cfg.FrontendURL == "" {
		handler = func(*echo.Context) error {
			return httpError(http.StatusServiceUnavailable, "frontend not configured (FRONTEND_URL)")
		}
	} else {
		target, _ := url.Parse(s.cfg.FrontendURL)
		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				r.SetXForwarded()
				// Keep the public Host, so the frontend builds URLs and checks origins for it.
				r.Out.Host = r.In.Host
				r.Out.Header.Del("Authorization")
			},
			// Flush immediately: live queries stream their updates.
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				s.logger.Error("frontend proxy failed", "path", r.URL.Path, "error", err)
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "frontend unavailable", http.StatusBadGateway)
			},
		}
		handler = echo.WrapHandler(proxy)
	}
	e.Any(frontendBase, handler)
	e.Any(frontendBase+"/*", handler)
}
