package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

// One API key (PICAPI_API_KEY) authorizes management. The web UI exchanges it for a
// signed session cookie; changing the key ends all sessions.

const (
	sessionCookie   = "picapi_session"
	sessionLifetime = 30 * 24 * time.Hour
	csrfHeader      = "X-Picapi-CSRF"
)

func (s *Server) validKey(key string) bool {
	given := sha256.Sum256([]byte(key))
	want := sha256.Sum256([]byte(s.cfg.APIKey))
	return subtle.ConstantTimeCompare(given[:], want[:]) == 1
}

// requireAuth accepts the API key as a Bearer token, or a session cookie from the web UI.
// Cookie-authenticated writes must carry the X-Picapi-CSRF header, which cross-site
// forms cannot send.
func (s *Server) requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if key, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer "); ok {
			if !s.validKey(strings.TrimSpace(key)) {
				return httpError(http.StatusUnauthorized, "missing or invalid API key")
			}
			c.Set("auth", "key")
			return next(c)
		}
		if !s.validSession(c) {
			return httpError(http.StatusUnauthorized, "missing or invalid API key")
		}
		method := c.Request().Method
		if method != http.MethodGet && method != http.MethodHead && c.Request().Header.Get(csrfHeader) != "1" {
			return httpError(http.StatusForbidden, "missing "+csrfHeader+" header")
		}
		c.Set("auth", "session")
		return next(c)
	}
}

func (s *Server) validSession(c *echo.Context) bool {
	cookie, err := c.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expRaw, mac, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	return err == nil && time.Now().Unix() <= exp && hmac.Equal([]byte(mac), []byte(s.sessionMAC(exp)))
}

// sessionMAC binds the session to the current API key.
func (s *Server) sessionMAC(exp int64) string {
	keyHash := sha256.Sum256([]byte(s.cfg.APIKey))
	mac := hmac.New(sha256.New, []byte(s.cfg.SigningSecret))
	mac.Write([]byte("session|" + base64.RawURLEncoding.EncodeToString(keyHash[:]) + "|" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) setSessionCookie(c *echo.Context, value string, maxAge int) {
	c.SetCookie(&http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicBaseURL, "https://"),
		SameSite: http.SameSiteStrictMode,
	})
}

// login exchanges the API key for a session cookie.
func (s *Server) login(c *echo.Context) error {
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	key, _ := body["key"].(string)
	if key == "" || !s.validKey(key) {
		return httpError(http.StatusUnauthorized, "invalid API key")
	}
	exp := time.Now().Add(sessionLifetime).Unix()
	s.setSessionCookie(c, strconv.FormatInt(exp, 10)+"."+s.sessionMAC(exp), int(sessionLifetime.Seconds()))
	return c.JSON(http.StatusOK, map[string]string{"auth": "session"})
}

func (s *Server) logout(c *echo.Context) error {
	s.setSessionCookie(c, "", -1)
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) sessionInfo(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{"auth": c.Get("auth")})
}
