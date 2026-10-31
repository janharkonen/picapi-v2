package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

// Delivery: /i/{id}[/{transforms}][/{slug}.{ext}]
//
// Every request reads the original from the bucket. Without transforms its bytes are
// streamed as they are; with transforms the result is rendered in memory and streamed.
// Nothing is stored: Cloudflare caches the responses, which are immutable.

var expPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

func (s *Server) deliver(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	if !isValidID(id) {
		return httpError(http.StatusNotFound, "not found")
	}
	asset, body, err := s.storage.get(ctx, id)
	if err != nil {
		return err
	}
	if asset == nil {
		return httpError(http.StatusNotFound, "not found")
	}
	defer body.Close()

	// Split the optional segments into transforms and slug.
	a, b := c.Param("a"), c.Param("b")
	pathTransforms, slug := "", ""
	switch {
	case b != "":
		if strings.Contains(a, ".") {
			return httpError(http.StatusBadRequest, "invalid transforms segment")
		}
		pathTransforms, slug = a, b
	case a != "" && strings.Contains(a, "."):
		slug = a
	default:
		pathTransforms = a
	}
	if slug != "" && !isValidSlug(slug) {
		return httpError(http.StatusBadRequest, "invalid slug")
	}

	query := c.Request().URL.Query()
	queryTransforms, err := parseQueryTransforms(query)
	if err != nil {
		return err
	}
	if pathTransforms != "" && queryTransforms != nil {
		return httpError(http.StatusBadRequest, "transforms given in both path and query")
	}
	t := queryTransforms
	if pathTransforms != "" {
		if t, err = parseTransformString(pathTransforms); err != nil {
			return err
		}
	}
	var resolved *Resolved
	if len(t) > 0 {
		r, err := t.resolve(asset.ContentType)
		if err != nil {
			return err
		}
		resolved = &r
	}

	exp, err := singleQuery(query, "exp")
	if err != nil {
		return err
	}
	sig, err := singleQuery(query, "s")
	if err != nil {
		return err
	}

	// One URL per variant, so Cloudflare holds one copy: redirect anything else to the
	// canonical form. Unknown query parameters (cache busters) are dropped.
	canonical := t.canonical()
	canonicalQuery := url.Values{}
	if exp != nil {
		canonicalQuery.Set("exp", *exp)
	}
	if sig != nil {
		canonicalQuery.Set("s", *sig)
	}
	canonicalURL := deliveryPath(asset.ID, canonical, slug)
	if len(canonicalQuery) > 0 {
		canonicalURL += "?" + canonicalQuery.Encode() // Encode sorts keys: exp, s
	}
	requestURL := c.Request().URL.EscapedPath()
	if c.Request().URL.RawQuery != "" {
		requestURL += "?" + c.Request().URL.RawQuery
	}
	if requestURL != canonicalURL {
		c.Response().Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", yearSeconds))
		return c.Redirect(http.StatusMovedPermanently, canonicalURL)
	}

	var expUnix int64
	if exp != nil {
		parsed, _ := strconv.ParseInt(*exp, 10, 64)
		if !expPattern.MatchString(*exp) || parsed < time.Now().Unix() {
			return httpError(http.StatusForbidden, "URL has expired")
		}
		expUnix = parsed
	}
	if sig != nil || exp != nil {
		if sig == nil || !verifySignature(s.cfg.SigningSecret, signingPath(asset.ID, canonical), expUnix, *sig) {
			return httpError(http.StatusForbidden, "invalid signature")
		}
	} else if t.needsSignature() {
		return httpError(http.StatusForbidden, "these transforms require a signed URL")
	}
	if resolved != nil && resolved.BG && s.bg == nil {
		return httpError(http.StatusNotImplemented, errBGDisabled.Error())
	}

	header := c.Response().Header()
	cacheControl := fmt.Sprintf("public, max-age=%d, immutable", yearSeconds)
	if exp != nil {
		// Signed URLs that expire must not outlive their expiry in caches.
		cacheControl = fmt.Sprintf("public, max-age=%d", max(0, expUnix-time.Now().Unix()))
	}
	header.Set("Cache-Control", cacheControl)
	header.Set("X-Content-Type-Options", "nosniff")

	if resolved == nil {
		etag := `"` + asset.SHA256 + `"`
		header.Set("ETag", etag)
		if etagMatches(c.Request().Header.Get("If-None-Match"), etag) {
			return c.NoContent(http.StatusNotModified)
		}
		header.Set("Content-Length", strconv.FormatInt(asset.SizeBytes, 10))
		return c.Stream(http.StatusOK, asset.ContentType, body)
	}

	hash := variantHash(s.cfg.TransformVersion, asset.ID, *resolved)
	etag := `"` + hash + `"`
	header.Set("ETag", etag)
	header.Set("X-Picapi-Variant", hash)
	if etagMatches(c.Request().Header.Get("If-None-Match"), etag) {
		return c.NoContent(http.StatusNotModified)
	}

	original, err := io.ReadAll(io.LimitReader(body, s.cfg.MaxUploadBytes+1))
	if err != nil {
		return err
	}
	// Concurrent requests for one variant share a render.
	started := time.Now()
	result, err, _ := s.inflight.Do(hash, func() (any, error) {
		return s.renderVariant(original, *resolved)
	})
	if err != nil {
		return err
	}
	data := result.([]byte)
	header.Set("Server-Timing", fmt.Sprintf("transform;dur=%.1f", float64(time.Since(started).Microseconds())/1000))
	header.Set("Content-Length", strconv.Itoa(len(data)))
	return c.Blob(http.StatusOK, outputContentTypes[resolved.F], data)
}

func (s *Server) renderVariant(original []byte, r Resolved) ([]byte, error) {
	if r.BG {
		mask, err := s.bg.mask(original)
		if err != nil {
			if errors.Is(err, errBGDisabled) {
				return nil, httpError(http.StatusNotImplemented, err.Error())
			}
			return nil, fmt.Errorf("background removal: %w", err)
		}
		if original, err = cutout(original, mask, r.BGC); err != nil {
			return nil, err
		}
	}
	s.transforms <- struct{}{}
	defer func() { <-s.transforms }()
	return render(original, r, s.cfg.MaxInputPixels)
}

func singleQuery(query url.Values, key string) (*string, error) {
	values := query[key]
	if len(values) > 1 {
		return nil, httpError(http.StatusBadRequest, fmt.Sprintf("duplicate query parameter %q", key))
	}
	if len(values) == 0 {
		return nil, nil
	}
	return &values[0], nil
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		value := strings.TrimSpace(part)
		if value == "*" || value == etag || value == "W/"+etag {
			return true
		}
	}
	return false
}
