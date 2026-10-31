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

	"github.com/labstack/echo/v5"
	"golang.org/x/sync/errgroup"
)

const yearSeconds = 31536000

type assetJSON struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	SHA256      string `json:"sha256"`
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
		CreatedAt:   a.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		URL:         s.cfg.PublicBaseURL + "/i/" + a.ID,
	}
}

func httpError(code int, message string) error {
	return echo.NewHTTPError(code, message)
}

// ---- Service layer, shared by the HTTP API and MCP ----

func (s *Server) lookupAsset(ctx context.Context, id string) (*Asset, error) {
	if !isValidID(id) {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	asset, err := s.storage.head(ctx, id)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, httpError(http.StatusNotFound, "asset not found")
	}
	return asset, nil
}

func (s *Server) createAsset(ctx context.Context, buf []byte, filename string) (*Asset, error) {
	if len(buf) == 0 {
		return nil, httpError(http.StatusBadRequest, "file is empty")
	}
	if int64(len(buf)) > s.cfg.MaxUploadBytes {
		return nil, httpError(http.StatusRequestEntityTooLarge, "upload is too large")
	}
	info, err := probe(buf, s.cfg.MaxInputPixels)
	if err != nil {
		return nil, err
	}
	asset := &Asset{
		Filename:    sanitizeFilename(filename),
		ContentType: info.ContentType,
		SizeBytes:   int64(len(buf)),
		Width:       info.Width,
		Height:      info.Height,
		SHA256:      sha256Hex(buf),
	}
	for {
		asset.ID = newID()
		existing, err := s.storage.head(ctx, asset.ID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			break
		}
	}
	if err := s.storage.put(ctx, asset, buf); err != nil {
		return nil, err
	}
	s.events.publish(assetEvent{Type: "created", ID: asset.ID})
	// Read back so created_at is the bucket's LastModified, as in every later response.
	return s.lookupAsset(ctx, asset.ID)
}

// removeAsset deletes the object and purges every cached URL of the asset.
func (s *Server) removeAsset(ctx context.Context, asset *Asset) error {
	if err := s.storage.delete(ctx, asset.ID); err != nil {
		return err
	}
	s.events.publish(assetEvent{Type: "deleted", ID: asset.ID})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.purger.purgeAsset(ctx, asset.ID); err != nil {
			s.logger.Error("purging asset failed", "asset", asset.ID, "error", err)
		}
	}()
	return nil
}

// listAssetPage returns up to limit assets after cursor (an asset ID), newest first.
func (s *Server) listAssetPage(ctx context.Context, limit int, cursor string) ([]assetJSON, string, error) {
	objects, err := s.storage.listIDs(ctx)
	if err != nil {
		return nil, "", err
	}
	start := 0
	if cursor != "" {
		start = -1
		for i, obj := range objects {
			if obj.ID == cursor {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", httpError(http.StatusBadRequest, "invalid cursor")
		}
	}
	end := min(start+limit, len(objects))
	page := objects[start:end]

	items := make([]assetJSON, len(page))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(16)
	for i, obj := range page {
		group.Go(func() error {
			asset, err := s.storage.head(gctx, obj.ID)
			if err != nil {
				return err
			}
			if asset == nil { // deleted meanwhile
				asset = &Asset{ID: obj.ID, Filename: obj.ID, CreatedAt: obj.Modified}
			}
			items[i] = s.assetJSON(asset)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, "", err
	}
	next := ""
	if end < len(objects) && len(page) > 0 {
		next = page[len(page)-1].ID
	}
	return items, next, nil
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

// ---- HTTP handlers ----

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
	asset, err := s.createAsset(c.Request().Context(), buf, filename)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s.assetJSON(asset))
}

var limitPattern = regexp.MustCompile(`^[0-9]+$`)

func (s *Server) listAssets(c *echo.Context) error {
	limitRaw := c.QueryParamOr("limit", "50")
	limit, _ := strconv.Atoi(limitRaw)
	if !limitPattern.MatchString(limitRaw) || limit < 1 || limit > 100 {
		return httpError(http.StatusBadRequest, "limit must be an integer from 1 to 100")
	}
	cursor := c.QueryParam("cursor")
	if _, given := c.QueryParams()["cursor"]; given && !isValidID(cursor) {
		return httpError(http.StatusBadRequest, "invalid cursor")
	}
	items, next, err := s.listAssetPage(c.Request().Context(), limit, cursor)
	if err != nil {
		return err
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func (s *Server) getAsset(c *echo.Context) error {
	asset, err := s.lookupAsset(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.assetJSON(asset))
}

func (s *Server) deleteAsset(c *echo.Context) error {
	asset, err := s.lookupAsset(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	if err := s.removeAsset(c.Request().Context(), asset); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) assetURL(c *echo.Context) error {
	asset, err := s.lookupAsset(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	body, err := readJSON(c)
	if err != nil {
		return err
	}
	transforms, err := optionalString(body, "transforms")
	if err != nil {
		return err
	}
	slug, err := optionalString(body, "slug")
	if err != nil || (slug != "" && !isValidSlug(slug)) {
		return httpError(http.StatusBadRequest, "invalid slug")
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

// ---- Helpers ----

func readJSON(c *echo.Context) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
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

func optionalString(body map[string]any, key string) (string, error) {
	raw, ok := body[key]
	if !ok {
		return "", nil
	}
	str, isString := raw.(string)
	if !isString {
		return "", httpError(http.StatusBadRequest, key+" must be a string")
	}
	return str, nil
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
