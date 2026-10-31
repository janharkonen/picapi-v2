package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
)

const (
	yearSeconds     = 31536000
	maxShareSeconds = 30 * 24 * 3600
)

type assetJSON struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	SHA256      string `json:"sha256"`
	Visibility  string `json:"visibility"`
	CreatedAt   string `json:"created_at"`
	URL         string `json:"url"`
}

func (s *server) assetJSON(a *Asset) assetJSON {
	return assetJSON{
		ID:          a.ID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		Width:       a.Width,
		Height:      a.Height,
		SHA256:      a.SHA256,
		Visibility:  a.Visibility,
		CreatedAt:   formatMillis(a.CreatedAt),
		URL:         s.cfg.PublicBaseURL + "/i/" + a.ID,
	}
}

func formatMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func httpError(code int, message string) error {
	return echo.NewHTTPError(code, message)
}

func (s *server) requireAPIKey(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		key, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return httpError(http.StatusUnauthorized, "missing or invalid API key")
		}
		valid, err := s.db.isValidAPIKey(c.Request().Context(), key)
		if err != nil {
			return err
		}
		if !valid {
			return httpError(http.StatusUnauthorized, "missing or invalid API key")
		}
		return next(c)
	}
}

func (s *server) mustGetAsset(c *echo.Context) (*Asset, error) {
	id := c.Param("id")
	if !isValidID(id) {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	asset, err := s.db.getAsset(c.Request().Context(), id)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	return asset, nil
}

func (s *server) uploadAsset(c *echo.Context) error {
	ctx := c.Request().Context()
	form, err := c.MultipartForm()
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return httpError(http.StatusRequestEntityTooLarge, "upload is too large")
		}
		return httpError(http.StatusBadRequest, `multipart field "file" is required`)
	}
	files := form.File["file"]
	if len(files) == 0 {
		return httpError(http.StatusBadRequest, `multipart field "file" is required`)
	}
	header := files[0]
	if header.Size == 0 {
		return httpError(http.StatusBadRequest, "file is empty")
	}
	if header.Size > s.cfg.MaxUploadBytes {
		return httpError(http.StatusRequestEntityTooLarge, "upload is too large")
	}

	visibility := "public"
	if values := form.Value["visibility"]; len(values) > 0 {
		visibility = values[0]
	}
	if err := validateVisibility(visibility); err != nil {
		return err
	}
	filename := header.Filename
	if values := form.Value["filename"]; len(values) > 0 {
		filename = values[0]
	}
	filename = sanitizeFilename(filename)

	file, err := header.Open()
	if err != nil {
		return err
	}
	defer file.Close()
	buf, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	info, err := probe(buf, s.cfg.MaxInputPixels)
	if err != nil {
		return err
	}

	id := newID()
	for {
		exists, err := s.db.assetExists(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			break
		}
		id = newID()
	}

	err = s.storage.put(ctx, s.storage.originalKey(id), buf, info.ContentType, map[string]string{
		"picapi-id":       id,
		"picapi-filename": url.PathEscape(filename),
	})
	if err != nil {
		return err
	}

	now := nowMillis()
	asset := Asset{
		ID:          id,
		Filename:    filename,
		ContentType: info.ContentType,
		SizeBytes:   int64(len(buf)),
		Width:       info.Width,
		Height:      info.Height,
		SHA256:      sha256Hex(buf),
		Visibility:  visibility,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.insertAsset(ctx, asset); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s.assetJSON(&asset))
}

var limitPattern = regexp.MustCompile(`^[0-9]+$`)

func (s *server) listAssets(c *echo.Context) error {
	ctx := c.Request().Context()
	limitRaw := c.QueryParamOr("limit", "50")
	limit, _ := strconv.Atoi(limitRaw)
	if !limitPattern.MatchString(limitRaw) || limit < 1 || limit > 100 {
		return httpError(http.StatusBadRequest, "limit must be an integer from 1 to 100")
	}
	cursor := c.QueryParam("cursor")
	if _, given := c.QueryParams()["cursor"]; given {
		exists := false
		if isValidID(cursor) {
			var err error
			if exists, err = s.db.assetExists(ctx, cursor); err != nil {
				return err
			}
		}
		if !exists {
			return httpError(http.StatusBadRequest, "invalid cursor")
		}
	}

	rows, err := s.db.listAssets(ctx, limit+1, cursor)
	if err != nil {
		return err
	}
	items := make([]assetJSON, 0, limit)
	for i := 0; i < len(rows) && i < limit; i++ {
		items = append(items, s.assetJSON(&rows[i]))
	}
	var next *string
	if len(rows) > limit {
		next = &items[len(items)-1].ID
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *server) getAsset(c *echo.Context) error {
	asset, err := s.mustGetAsset(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.assetJSON(asset))
}

func (s *server) patchAsset(c *echo.Context) error {
	asset, err := s.mustGetAsset(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	var filename, visibility *string
	if raw, ok := body["filename"]; ok {
		name, isString := raw.(string)
		if !isString {
			return httpError(http.StatusBadRequest, "filename must be a string")
		}
		name = sanitizeFilename(name)
		filename = &name
	}
	if raw, ok := body["visibility"]; ok {
		v, _ := raw.(string)
		if err := validateVisibility(v); err != nil {
			return err
		}
		visibility = &v
	}
	if filename == nil && visibility == nil {
		return httpError(http.StatusBadRequest, "nothing to update; send filename and/or visibility")
	}
	ctx := c.Request().Context()
	if err := s.db.updateAsset(ctx, asset.ID, filename, visibility); err != nil {
		return err
	}
	updated, err := s.db.getAsset(ctx, asset.ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.assetJSON(updated))
}

func (s *server) deleteAsset(c *echo.Context) error {
	asset, err := s.mustGetAsset(c)
	if err != nil {
		return err
	}
	if err := s.db.softDeleteAsset(c.Request().Context(), asset.ID); err != nil {
		return err
	}
	go func() {
		ctx := context.Background()
		err := s.storage.delete(ctx, s.storage.originalKey(asset.ID))
		if err == nil {
			err = s.storage.deletePrefix(ctx, s.storage.variantPrefix(asset.ID))
		}
		if err == nil {
			err = s.db.deleteVariants(ctx, asset.ID)
		}
		if err != nil {
			c.Echo().Logger.Error("background delete failed", "asset", asset.ID, "error", err)
		}
	}()
	return c.NoContent(http.StatusNoContent)
}

func (s *server) assetURL(c *echo.Context) error {
	asset, err := s.mustGetAsset(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	var exp int64
	if raw, ok := body["expires_in"]; ok {
		seconds, err := parseSeconds(raw, yearSeconds)
		if err != nil {
			return err
		}
		exp = time.Now().Unix() + seconds
	}
	built, err := s.buildURL(asset, body, exp)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"url": built})
}

func (s *server) shareAsset(c *echo.Context) error {
	asset, err := s.mustGetAsset(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	seconds := int64(3600)
	if raw, ok := body["expires_in"]; ok {
		if seconds, err = parseSeconds(raw, maxShareSeconds); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	exp := now + seconds
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": asset.ID,
		"iat": now,
		"exp": exp,
	}).SignedString([]byte(s.cfg.SigningSecret))
	if err != nil {
		return err
	}
	built, err := s.buildURL(asset, body, 0)
	if err != nil {
		return err
	}
	if strings.Contains(built, "?") {
		built += "&token=" + url.QueryEscape(token)
	} else {
		built += "?token=" + url.QueryEscape(token)
	}
	return c.JSON(http.StatusOK, map[string]string{
		"url":        built,
		"token":      token,
		"expires_at": formatMillis(exp * 1000),
	})
}

// buildURL builds an absolute, canonical delivery URL, signed when the transforms need it or it expires.
func (s *server) buildURL(asset *Asset, body map[string]any, exp int64) (string, error) {
	t := Transform{}
	if raw, ok := body["transforms"]; ok {
		str, isString := raw.(string)
		if !isString {
			return "", httpError(http.StatusBadRequest, "transforms must be a string")
		}
		if str != "" {
			var err error
			if t, err = parseTransformString(str); err != nil {
				return "", err
			}
			if _, err := t.resolve(asset.ContentType); err != nil {
				return "", err
			}
		}
	}
	slug := ""
	if raw, ok := body["slug"]; ok {
		str, isString := raw.(string)
		if !isString || !isValidSlug(str) {
			return "", httpError(http.StatusBadRequest, "invalid slug")
		}
		slug = str
	}
	canonical := t.canonical()
	query := url.Values{}
	if exp != 0 || t.needsSignature() {
		if exp != 0 {
			query.Set("exp", strconv.FormatInt(exp, 10))
		}
		query.Set("s", signature(s.cfg.SigningSecret, signingPath(asset.ID, canonical), exp))
	}
	built := s.cfg.PublicBaseURL + deliveryPath(asset.ID, canonical, slug)
	if len(query) > 0 {
		built += "?" + query.Encode()
	}
	return built, nil
}

func readJSON(c *echo.Context) (map[string]any, error) {
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, httpError(http.StatusBadRequest, "invalid JSON body")
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, httpError(http.StatusBadRequest, "JSON body must be an object")
	}
	return obj, nil
}

func validateVisibility(v string) error {
	if v != "public" && v != "private" {
		return httpError(http.StatusBadRequest, "visibility must be public or private")
	}
	return nil
}

func parseSeconds(raw any, maxSeconds int64) (int64, error) {
	n, ok := raw.(float64)
	if !ok || n != math.Trunc(n) || n < 1 || n > float64(maxSeconds) {
		return 0, httpError(http.StatusBadRequest, "expires_in must be an integer from 1 to "+strconv.FormatInt(maxSeconds, 10))
	}
	return int64(n), nil
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(controlChars.ReplaceAllString(name, ""))
	if runes := []rune(name); len(runes) > 255 {
		name = string(runes[:255])
	}
	if name == "" {
		return "upload"
	}
	return name
}
