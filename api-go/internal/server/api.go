package server

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

func (s *Server) assetJSON(a *Asset) assetJSON {
	return assetJSON{
		ID:          a.ID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		Width:       a.Width,
		Height:      a.Height,
		SHA256:      a.SHA256,
		Visibility:  a.Visibility,
		CreatedAt:   formatTime(a.CreatedAt),
		URL:         s.cfg.PublicBaseURL + "/i/" + a.ID,
	}
}

func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

func httpError(code int, message string) error {
	return echo.NewHTTPError(code, message)
}

// ---- Service layer, shared by the HTTP API and MCP ----

func (s *Server) lookupAsset(ctx context.Context, id string) (*Asset, error) {
	if !isValidID(id) {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	asset, err := s.db.getAsset(ctx, id)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	return asset, nil
}

func (s *Server) createAsset(ctx context.Context, buf []byte, filename, visibility string) (*Asset, error) {
	if len(buf) == 0 {
		return nil, httpError(http.StatusBadRequest, "file is empty")
	}
	if int64(len(buf)) > s.cfg.MaxUploadBytes {
		return nil, httpError(http.StatusRequestEntityTooLarge, "upload is too large")
	}
	if visibility == "" {
		visibility = "public"
	}
	if err := validateVisibility(visibility); err != nil {
		return nil, err
	}
	info, err := probe(buf, s.cfg.MaxInputPixels)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	asset := Asset{
		Filename:    sanitizeFilename(filename),
		ContentType: info.ContentType,
		SizeBytes:   int64(len(buf)),
		Width:       info.Width,
		Height:      info.Height,
		SHA256:      sha256Hex(buf),
		Visibility:  visibility,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// The object is written first; a crash leaves an orphan object, never a row without bytes.
	for {
		asset.ID = newID()
		exists, err := s.db.assetExists(ctx, asset.ID)
		if err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		err = s.storage.put(ctx, s.storage.originalKey(asset.ID), buf, info.ContentType, map[string]string{
			"picapi-id":       asset.ID,
			"picapi-filename": url.PathEscape(asset.Filename),
		})
		if err != nil {
			return nil, err
		}
		inserted, err := s.db.insertAsset(ctx, asset)
		if err != nil {
			return nil, err
		}
		if inserted {
			return &asset, nil
		}
	}
}

// updateAsset changes filename and/or visibility. Going private purges the CDN.
func (s *Server) updateAsset(ctx context.Context, asset *Asset, filename, visibility *string) (*Asset, error) {
	if filename == nil && visibility == nil {
		return nil, httpError(http.StatusBadRequest, "nothing to update; send filename and/or visibility")
	}
	if filename != nil {
		name := sanitizeFilename(*filename)
		filename = &name
	}
	if visibility != nil {
		if err := validateVisibility(*visibility); err != nil {
			return nil, err
		}
	}
	if err := s.db.updateAsset(ctx, asset.ID, filename, visibility); err != nil {
		return nil, err
	}
	if visibility != nil && *visibility == "private" && asset.Visibility == "public" {
		go func() {
			if err := s.purgeAsset(context.Background(), asset.ID); err != nil {
				s.logger.Error("purging asset failed", "asset", asset.ID, "error", err)
			}
		}()
	}
	return s.lookupAsset(ctx, asset.ID)
}

func (s *Server) removeAsset(ctx context.Context, asset *Asset) error {
	if err := s.db.softDeleteAsset(ctx, asset.ID); err != nil {
		return err
	}
	go s.cleanupDeletedAsset(asset.ID)
	return nil
}

// buildURL builds an absolute, canonical delivery URL, signed when the transforms need it
// or when it expires (exp != 0).
func (s *Server) buildURL(asset *Asset, transforms, slug string, exp int64) (string, error) {
	t := Transform{}
	if transforms != "" {
		var err error
		if t, err = parseTransformString(transforms); err != nil {
			return "", err
		}
		if _, err := t.resolve(asset.ContentType); err != nil {
			return "", err
		}
	}
	if slug != "" && !isValidSlug(slug) {
		return "", httpError(http.StatusBadRequest, "invalid slug")
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

type shareResult struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Server) share(asset *Asset, transforms, slug string, seconds int64) (shareResult, error) {
	now := time.Now().Unix()
	exp := now + seconds
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": asset.ID,
		"iat": now,
		"exp": exp,
	}).SignedString([]byte(s.cfg.SigningSecret))
	if err != nil {
		return shareResult{}, err
	}
	built, err := s.buildURL(asset, transforms, slug, 0)
	if err != nil {
		return shareResult{}, err
	}
	sep := "?"
	if strings.Contains(built, "?") {
		sep = "&"
	}
	return shareResult{
		URL:       built + sep + "token=" + url.QueryEscape(token),
		Token:     token,
		ExpiresAt: formatTime(time.Unix(exp, 0)),
	}, nil
}

// ---- HTTP handlers ----

func (s *Server) assetFromPath(c *echo.Context) (*Asset, error) {
	return s.lookupAsset(c.Request().Context(), c.Param("id"))
}

func (s *Server) uploadAsset(c *echo.Context) error {
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

	file, err := header.Open()
	if err != nil {
		return err
	}
	defer file.Close()
	buf, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	asset, err := s.createAsset(c.Request().Context(), buf, filename, visibility)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s.assetJSON(asset))
}

var limitPattern = regexp.MustCompile(`^[0-9]+$`)

func (s *Server) listAssets(c *echo.Context) error {
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

func (s *Server) getAsset(c *echo.Context) error {
	asset, err := s.assetFromPath(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.assetJSON(asset))
}

func (s *Server) patchAsset(c *echo.Context) error {
	asset, err := s.assetFromPath(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	filename, err := optionalString(body, "filename")
	if err != nil {
		return err
	}
	var visibility *string
	if raw, ok := body["visibility"]; ok {
		v, _ := raw.(string)
		visibility = &v
	}
	updated, err := s.updateAsset(c.Request().Context(), asset, filename, visibility)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.assetJSON(updated))
}

func (s *Server) deleteAsset(c *echo.Context) error {
	asset, err := s.assetFromPath(c)
	if err != nil {
		return err
	}
	if err := s.removeAsset(c.Request().Context(), asset); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) assetURL(c *echo.Context) error {
	asset, err := s.assetFromPath(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	transforms, slug, err := urlOptions(body)
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
	built, err := s.buildURL(asset, transforms, slug, exp)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"url": built})
}

func (s *Server) shareAsset(c *echo.Context) error {
	asset, err := s.assetFromPath(c)
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	transforms, slug, err := urlOptions(body)
	if err != nil {
		return err
	}
	seconds := int64(3600)
	if raw, ok := body["expires_in"]; ok {
		if seconds, err = parseSeconds(raw, maxShareSeconds); err != nil {
			return err
		}
	}
	result, err := s.share(asset, transforms, slug, seconds)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, result)
}

type keyJSON struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	LastUsed  *string `json:"last_used"`
	RevokedAt *string `json:"revoked_at"`
	Key       string  `json:"key,omitempty"`
}

func toKeyJSON(k APIKey) keyJSON {
	return keyJSON{ID: k.ID, Name: k.Name, CreatedAt: formatTime(k.CreatedAt), LastUsed: formatTimePtr(k.LastUsed), RevokedAt: formatTimePtr(k.RevokedAt)}
}

func (s *Server) listKeys(c *echo.Context) error {
	keys, err := s.db.listKeys(c.Request().Context())
	if err != nil {
		return err
	}
	items := make([]keyJSON, len(keys))
	for i, k := range keys {
		items[i] = toKeyJSON(k)
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

// createKey returns the new key once; only its hash is stored.
func (s *Server) createKey(c *echo.Context) error {
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	name, _ := body["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return httpError(http.StatusBadRequest, "name must be 1 to 100 characters")
	}
	key := "pk_" + randomString(40)
	created, err := s.db.createKey(c.Request().Context(), name, key)
	if err != nil {
		return err
	}
	out := toKeyJSON(created)
	out.Key = key
	return c.JSON(http.StatusCreated, out)
}

func (s *Server) revokeKey(c *echo.Context) error {
	ctx := c.Request().Context()
	count, err := s.db.activeKeyCount(ctx)
	if err != nil {
		return err
	}
	if count <= 1 {
		active, err := s.db.isActiveKeyID(ctx, c.Param("id"))
		if err != nil {
			return err
		}
		if active {
			return httpError(http.StatusConflict, "cannot revoke the last active key")
		}
	}
	revoked, err := s.db.revokeKey(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if !revoked {
		return httpError(http.StatusNotFound, "key not found")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) reconcileHandler(c *echo.Context) error {
	fix := c.QueryParam("fix") == "true"
	report, err := s.reconcile(c.Request().Context(), fix)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, report)
}

// ---- Helpers ----

func readJSON(c *echo.Context) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseJSONObject(raw)
}

func parseJSONObject(raw []byte) (map[string]any, error) {
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

func optionalString(body map[string]any, key string) (*string, error) {
	raw, ok := body[key]
	if !ok {
		return nil, nil
	}
	str, isString := raw.(string)
	if !isString {
		return nil, httpError(http.StatusBadRequest, key+" must be a string")
	}
	return &str, nil
}

func urlOptions(body map[string]any) (transforms, slug string, err error) {
	t, err := optionalString(body, "transforms")
	if err != nil {
		return "", "", err
	}
	sl, err := optionalString(body, "slug")
	if err != nil || (sl != nil && !isValidSlug(*sl)) {
		return "", "", httpError(http.StatusBadRequest, "invalid slug")
	}
	if t != nil {
		transforms = *t
	}
	if sl != nil {
		slug = *sl
	}
	return transforms, slug, nil
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
