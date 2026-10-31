package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	sessionCookie   = "picapi_session"
	sessionLifetime = 30 * 24 * time.Hour
	csrfHeader      = "X-Picapi-CSRF"
)

// requireAuth accepts an API key as a Bearer token, or a session cookie from the web UI.
// Cookie-authenticated writes must carry the X-Picapi-CSRF header, which cross-site
// forms cannot send.
func (s *Server) requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		if key, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer "); ok {
			id, err := s.db.apiKeyID(ctx, strings.TrimSpace(key))
			if err != nil {
				return err
			}
			if id == "" {
				return httpError(http.StatusUnauthorized, "missing or invalid API key")
			}
			if err := s.db.touchKey(ctx, id); err != nil {
				return err
			}
			c.Set("keyID", id)
			c.Set("auth", "key")
			return next(c)
		}

		keyID, err := s.sessionKeyID(c)
		if err != nil {
			return err
		}
		if keyID == "" {
			return httpError(http.StatusUnauthorized, "missing or invalid API key")
		}
		method := c.Request().Method
		if method != http.MethodGet && method != http.MethodHead && c.Request().Header.Get(csrfHeader) != "1" {
			return httpError(http.StatusForbidden, "missing "+csrfHeader+" header")
		}
		c.Set("keyID", keyID)
		c.Set("auth", "session")
		return next(c)
	}
}

// sessionKeyID returns the API key ID of a valid session cookie, or "".
func (s *Server) sessionKeyID(c *echo.Context) (string, error) {
	cookie, err := c.Cookie(sessionCookie)
	if err != nil {
		return "", nil
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return "", nil
	}
	keyID, expRaw, mac := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || time.Now().Unix() > exp || !hmac.Equal([]byte(mac), []byte(s.sessionMAC(keyID, exp))) {
		return "", nil
	}
	// Revoking the key ends its sessions.
	active, err := s.db.isActiveKeyID(c.Request().Context(), keyID)
	if err != nil || !active {
		return "", err
	}
	return keyID, nil
}

func (s *Server) sessionMAC(keyID string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.SigningSecret))
	mac.Write([]byte("session|" + keyID + "|" + strconv.FormatInt(exp, 10)))
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

// login exchanges an API key for a session cookie.
func (s *Server) login(c *echo.Context) error {
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	key, _ := body["key"].(string)
	keyID := ""
	if key != "" {
		if keyID, err = s.db.apiKeyID(c.Request().Context(), key); err != nil {
			return err
		}
	}
	if keyID == "" {
		return httpError(http.StatusUnauthorized, "invalid API key")
	}
	exp := time.Now().Add(sessionLifetime).Unix()
	s.setSessionCookie(c, keyID+"."+strconv.FormatInt(exp, 10)+"."+s.sessionMAC(keyID, exp), int(sessionLifetime.Seconds()))
	return c.JSON(http.StatusOK, map[string]string{"key_id": keyID, "auth": "session"})
}

func (s *Server) logout(c *echo.Context) error {
	s.setSessionCookie(c, "", -1)
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) sessionInfo(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{"key_id": c.Get("keyID"), "auth": c.Get("auth")})
}
