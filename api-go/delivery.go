package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
)

type rendered struct {
	data        []byte
	contentType string
	durationMs  float64
}

var expPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

// deliver serves originals and variants at /i/{id}[/{transforms}][/{slug}.{ext}].
func (s *server) deliver(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var asset *Asset
	if isValidID(id) {
		var err error
		if asset, err = s.db.getAsset(ctx, id); err != nil {
			return err
		}
	}
	if asset == nil {
		return httpError(http.StatusNotFound, "not found")
	}

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
	token, err := singleQuery(query, "token")
	if err != nil {
		return err
	}

	// One URL per variant: redirect anything else to the canonical form.
	canonical := t.canonical()
	canonicalQuery := url.Values{}
	for key, value := range map[string]*string{"exp": exp, "s": sig, "token": token} {
		if value != nil {
			canonicalQuery.Set(key, *value)
		}
	}
	canonicalURL := deliveryPath(asset.ID, canonical, slug)
	if len(canonicalQuery) > 0 {
		canonicalURL += "?" + canonicalQuery.Encode() // Encode sorts keys: exp, s, token
	}
	requestURL := c.Request().URL.EscapedPath()
	if c.Request().URL.RawQuery != "" {
		requestURL += "?" + c.Request().URL.RawQuery
	}
	if requestURL != canonicalURL {
		if asset.Visibility == "public" {
			c.Response().Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", yearSeconds))
		} else {
			c.Response().Header().Set("Cache-Control", "no-store")
		}
		return c.Redirect(http.StatusMovedPermanently, canonicalURL)
	}

	header := c.Response().Header()
	cacheControl := fmt.Sprintf("public, max-age=%d, immutable", yearSeconds)
	if asset.Visibility == "private" {
		tokenExp, err := s.verifyViewToken(token, asset.ID)
		if err != nil {
			return err
		}
		cacheControl = fmt.Sprintf("private, max-age=%d", max(0, tokenExp-time.Now().Unix()))
		header.Set("Referrer-Policy", "no-referrer")
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
	if resolved != nil && resolved.BG {
		return httpError(http.StatusNotImplemented, "background removal is not implemented yet")
	}

	header.Set("Cache-Control", cacheControl)
	header.Set("X-Content-Type-Options", "nosniff")

	if resolved == nil {
		etag := `"` + asset.SHA256 + `"`
		header.Set("ETag", etag)
		if etagMatches(c.Request().Header.Get("If-None-Match"), etag) {
			return c.NoContent(http.StatusNotModified)
		}
		obj, err := s.storage.get(ctx, s.storage.originalKey(asset.ID))
		if err != nil {
			return err
		}
		if obj == nil {
			return httpError(http.StatusNotFound, "not found")
		}
		defer obj.Body.Close()
		if obj.Size >= 0 {
			header.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		}
		return c.Stream(http.StatusOK, asset.ContentType, obj.Body)
	}

	hash := variantHash(s.cfg.TransformVersion, asset.ID, *resolved)
	etag := `"` + hash + `"`
	header.Set("ETag", etag)
	header.Set("X-Picapi-Variant", hash)
	if etagMatches(c.Request().Header.Get("If-None-Match"), etag) {
		return c.NoContent(http.StatusNotModified)
	}

	if s.cfg.VariantStore {
		stored, err := s.db.getVariant(ctx, asset.ID, hash)
		if err != nil {
			return err
		}
		if stored != nil {
			obj, err := s.storage.get(ctx, stored.StoredKey)
			if err != nil {
				return err
			}
			if obj != nil {
				defer obj.Body.Close()
				if err := s.db.touchVariant(ctx, asset.ID, hash); err != nil {
					return err
				}
				header.Set("X-Picapi-Cache", "hit")
				if obj.Size >= 0 {
					header.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
				}
				return c.Stream(http.StatusOK, stored.ContentType, obj.Body)
			}
			if err := s.db.deleteVariant(ctx, asset.ID, hash); err != nil {
				return err
			}
		}
	}

	// Concurrent requests for one variant share a render. The render ignores the request's
	// cancellation, because other requests may be waiting for it.
	result, err, _ := s.inflight.Do(hash, func() (any, error) {
		s.transforms <- struct{}{}
		defer func() { <-s.transforms }()
		started := time.Now()
		original, err := s.storage.getBytes(context.Background(), s.storage.originalKey(asset.ID))
		if err != nil {
			return nil, err
		}
		data, err := render(original, *resolved, s.cfg.MaxInputPixels)
		if err != nil {
			return nil, err
		}
		return &rendered{
			data:        data,
			contentType: outputContentTypes[resolved.F],
			durationMs:  float64(time.Since(started).Microseconds()) / 1000,
		}, nil
	})
	if err != nil {
		return err
	}
	out := result.(*rendered)
	if s.cfg.VariantStore {
		s.storeVariant(c, asset.ID, hash, resolved.F, out)
	}

	header.Set("X-Picapi-Cache", "miss")
	header.Set("Server-Timing", fmt.Sprintf("transform;dur=%.1f", out.durationMs))
	header.Set("Content-Length", strconv.Itoa(len(out.data)))
	return c.Blob(http.StatusOK, out.contentType, out.data)
}

// storeVariant saves a render in the background. Concurrent requests share a render, but only one stores it.
func (s *server) storeVariant(c *echo.Context, assetID, hash, format string, out *rendered) {
	if _, busy := s.storing.LoadOrStore(hash, true); busy {
		return
	}
	logger := c.Echo().Logger
	go func() {
		defer s.storing.Delete(hash)
		ctx := context.Background()
		key := s.storage.variantKey(assetID, hash, format)
		if err := s.storage.put(ctx, key, out.data, out.contentType, nil); err != nil {
			logger.Error("storing variant failed", "key", key, "error", err)
			return
		}
		// Skip if the asset was deleted meanwhile; its cleanup won't see this row.
		asset, err := s.db.getAsset(ctx, assetID)
		if err == nil && asset != nil {
			err = s.db.upsertVariant(ctx, assetID, hash, Variant{
				StoredKey:   key,
				ContentType: out.contentType,
				SizeBytes:   int64(len(out.data)),
			})
		}
		if err != nil {
			logger.Error("recording variant failed", "key", key, "error", err)
		}
	}()
}

func (s *server) verifyViewToken(token *string, assetID string) (int64, error) {
	if token == nil {
		return 0, httpError(http.StatusForbidden, "this asset is private")
	}
	invalid := httpError(http.StatusForbidden, "invalid or expired token")
	parsed, err := jwt.Parse(*token, func(*jwt.Token) (any, error) {
		return []byte(s.cfg.SigningSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return 0, invalid
	}
	sub, err := parsed.Claims.GetSubject()
	if err != nil || sub != assetID {
		return 0, invalid
	}
	exp, err := parsed.Claims.GetExpirationTime()
	if err != nil || exp == nil {
		return 0, invalid
	}
	return exp.Unix(), nil
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
