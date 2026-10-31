// Transform parameters: parsing, presets, canonical form, resolution, signing.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// Transform holds the explicit parameters exactly as requested, keyed by name.
// Values are validated on insert, so they are already in canonical spelling.
type Transform map[string]string

// Canonical key order.
var transformKeys = []string{"p", "w", "h", "fit", "g", "f", "q", "dpr", "up", "bg", "bgc"}

// Resolved holds fully resolved parameters; the variant hash and the image pipeline use these.
// Zero W/H and Q mean "not set".
type Resolved struct {
	W, H int
	Fit  string
	G    string
	F    string
	Q    int
	Up   bool
	BG   bool
	BGC  string
}

var presets = map[string]Transform{
	"thumb":  {"w": "128", "h": "128", "fit": "cover", "g": "attention", "f": "webp", "q": "70"},
	"small":  {"w": "320", "f": "webp", "q": "75"},
	"medium": {"w": "640", "f": "webp", "q": "80"},
	"large":  {"w": "1280", "f": "webp", "q": "80"},
	"og":     {"w": "1200", "h": "630", "fit": "cover", "g": "attention", "f": "jpeg", "q": "80"},
}

var (
	gridSizes     = map[string]bool{"64": true, "128": true, "256": true, "320": true, "480": true, "640": true, "960": true, "1280": true, "1920": true}
	gridQualities = map[string]bool{"50": true, "60": true, "70": true, "80": true, "90": true}
)

const (
	maxDimension   = 4096
	defaultQuality = 80
)

var (
	dimensionPattern = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
	qualityPattern   = regexp.MustCompile(`^(100|[1-9][0-9]?)$`)
	colorPattern     = regexp.MustCompile(`^[0-9a-f]{6}$`)
	slugPattern      = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*\.[A-Za-z0-9]+$`)
)

func badRequest(format string, args ...any) error {
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(format, args...))
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

func (t Transform) set(key, value string) error {
	if !oneOf(key, transformKeys...) {
		return badRequest("unknown transform %q", key)
	}
	if _, dup := t[key]; dup {
		return badRequest("duplicate transform %q", key)
	}
	switch key {
	case "p":
		if _, ok := presets[value]; !ok {
			return badRequest("unknown preset %q", value)
		}
	case "w", "h":
		if n, _ := strconv.Atoi(value); !dimensionPattern.MatchString(value) || n > maxDimension {
			return badRequest("%s must be an integer from 1 to %d", key, maxDimension)
		}
	case "fit":
		if !oneOf(value, "inside", "cover", "fill") {
			return badRequest("fit must be inside, cover or fill")
		}
	case "g":
		if !oneOf(value, "center", "attention", "n", "s", "e", "w") {
			return badRequest("g must be center, attention, n, s, e or w")
		}
	case "f":
		if !oneOf(value, "jpeg", "png", "webp", "avif") {
			return badRequest("f must be jpeg, png, webp or avif")
		}
	case "q":
		if !qualityPattern.MatchString(value) {
			return badRequest("q must be an integer from 1 to 100")
		}
	case "dpr":
		if !oneOf(value, "1", "2", "3") {
			return badRequest("dpr must be 1, 2 or 3")
		}
	case "up":
		if !oneOf(value, "0", "1") {
			return badRequest("up must be 0 or 1")
		}
	case "bg":
		if value != "remove" {
			return badRequest("bg must be remove")
		}
	case "bgc":
		if !colorPattern.MatchString(value) {
			return badRequest("bgc must be a 6-digit lowercase hex color")
		}
	}
	t[key] = value
	return nil
}

// parseTransformString parses the path form: "w_320,f_webp".
func parseTransformString(s string) (Transform, error) {
	t := Transform{}
	for _, part := range strings.Split(s, ",") {
		sep := strings.Index(part, "_")
		if sep <= 0 || sep == len(part)-1 {
			return nil, badRequest("invalid transform %q", part)
		}
		if err := t.set(part[:sep], part[sep+1:]); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// parseQueryTransforms parses the query form: "?w=320&f=webp". Returns nil when no transform keys are present.
func parseQueryTransforms(query url.Values) (Transform, error) {
	t := Transform{}
	for _, key := range transformKeys {
		values := query[key]
		if len(values) == 0 {
			continue
		}
		if len(values) > 1 {
			return nil, badRequest("duplicate transform %q", key)
		}
		if err := t.set(key, values[0]); err != nil {
			return nil, err
		}
	}
	if len(t) == 0 {
		return nil, nil
	}
	return t, nil
}

func (t Transform) canonical() string {
	parts := make([]string, 0, len(t))
	for _, key := range transformKeys {
		if value, ok := t[key]; ok {
			parts = append(parts, key+"_"+value)
		}
	}
	return strings.Join(parts, ",")
}

func defaultOutputFormat(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/avif":
		return "avif"
	default:
		return "png"
	}
}

func (t Transform) resolve(sourceContentType string) (Resolved, error) {
	merged := Transform{"fit": "inside", "g": "center", "dpr": "1", "up": "0"}
	if name, ok := t["p"]; ok {
		for key, value := range presets[name] {
			merged[key] = value
		}
	}
	for key, value := range t {
		if key != "p" {
			merged[key] = value
		}
	}

	fit := merged["fit"]
	_, hasW := merged["w"]
	_, hasH := merged["h"]
	if fit != "inside" && (!hasW || !hasH) {
		return Resolved{}, badRequest("fit_%s requires both w and h", fit)
	}
	if _, ok := t["g"]; ok && fit != "cover" {
		return Resolved{}, badRequest("g requires fit_cover")
	}

	f, ok := merged["f"]
	if !ok {
		f = defaultOutputFormat(sourceContentType)
	}
	q := 0
	if f == "png" {
		if _, ok := t["q"]; ok {
			return Resolved{}, badRequest("q has no effect on png output")
		}
	} else {
		q = defaultQuality
		if raw, ok := merged["q"]; ok {
			q, _ = strconv.Atoi(raw)
		}
	}

	_, hasBG := merged["bg"]
	bgc, hasBGC := merged["bgc"]
	if hasBGC && !hasBG {
		return Resolved{}, badRequest("bgc requires bg_remove")
	}
	if hasBG && f == "jpeg" && !hasBGC {
		return Resolved{}, badRequest("bg_remove needs png, webp or avif output, or a bgc color")
	}

	dpr, _ := strconv.Atoi(merged["dpr"])
	w, _ := strconv.Atoi(merged["w"])
	h, _ := strconv.Atoi(merged["h"])
	w, h = w*dpr, h*dpr
	if w > maxDimension || h > maxDimension {
		return Resolved{}, badRequest("size multiplied by dpr must be at most %d", maxDimension)
	}

	g := "center"
	if fit == "cover" {
		g = merged["g"]
	}
	return Resolved{W: w, H: h, Fit: fit, G: g, F: f, Q: q, Up: merged["up"] == "1", BG: hasBG, BGC: bgc}, nil
}

// needsSignature reports whether the request leaves the grid or asks for background removal.
func (t Transform) needsSignature() bool {
	if _, ok := t["bg"]; ok {
		return true
	}
	for _, key := range []string{"w", "h"} {
		if v, ok := t[key]; ok && !gridSizes[v] {
			return true
		}
	}
	if v, ok := t["q"]; ok && !gridQualities[v] {
		return true
	}
	return false
}

func variantHash(transformVersion, id string, r Resolved) string {
	optInt := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	boolInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	input := fmt.Sprintf("%s|%s|w=%s;h=%s;fit=%s;g=%s;f=%s;q=%s;up=%d;bg=%d;bgc=%s",
		transformVersion, id, optInt(r.W), optInt(r.H), r.Fit, r.G, r.F, optInt(r.Q), boolInt(r.Up), boolInt(r.BG), r.BGC)
	return sha256Hex([]byte(input))[:32]
}

func isValidSlug(slug string) bool {
	return len(slug) <= 100 && slugPattern.MatchString(slug)
}

// signingPath is the path without slug; this is what gets signed.
func signingPath(id, canonicalTransforms string) string {
	if canonicalTransforms == "" {
		return "/i/" + id
	}
	return "/i/" + id + "/" + canonicalTransforms
}

func deliveryPath(id, canonicalTransforms, slug string) string {
	path := signingPath(id, canonicalTransforms)
	if slug != "" {
		return path + "/" + slug
	}
	return path
}

// signature signs path, plus "?exp=<exp>" when exp is non-zero.
func signature(secret, path string, exp int64) string {
	message := path
	if exp != 0 {
		message += "?exp=" + strconv.FormatInt(exp, 10)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySignature(secret, path string, exp int64, given string) bool {
	return hmac.Equal([]byte(signature(secret, path, exp)), []byte(given))
}
