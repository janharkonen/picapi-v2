package server

import (
	"bytes"
	"fmt"
	"math"
	"net/http"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/labstack/echo/v5"
)

var outputContentTypes = map[string]string{
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
	"avif": "image/avif",
}

// sniffContentType detects the upload type from magic bytes; the extension and client-sent type are not trusted.
func sniffContentType(buf []byte) string {
	switch {
	case len(buf) >= 3 && buf[0] == 0xff && buf[1] == 0xd8 && buf[2] == 0xff:
		return "image/jpeg"
	case bytes.HasPrefix(buf, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(buf, []byte("GIF87a")) || bytes.HasPrefix(buf, []byte("GIF89a")):
		return "image/gif"
	case len(buf) >= 12 && string(buf[0:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		return "image/webp"
	case len(buf) >= 12 && string(buf[4:8]) == "ftyp" && (string(buf[8:12]) == "avif" || string(buf[8:12]) == "avis"):
		return "image/avif"
	}
	return ""
}

type imageInfo struct {
	ContentType string
	Width       int
	Height      int
}

// displaySize returns the dimensions after applying the EXIF orientation.
func displaySize(img *vips.ImageRef) (int, int) {
	if img.Orientation() >= 5 {
		return img.Height(), img.Width()
	}
	return img.Width(), img.Height()
}

// probe validates an upload and returns its type and display dimensions.
func probe(buf []byte, maxInputPixels int) (imageInfo, error) {
	contentType := sniffContentType(buf)
	if contentType == "" {
		return imageInfo{}, echo.NewHTTPError(http.StatusUnsupportedMediaType, "unsupported file type; allowed: jpeg, png, gif, webp, avif")
	}
	img, err := vips.NewImageFromBuffer(buf)
	if err != nil {
		return imageInfo{}, echo.NewHTTPError(http.StatusBadRequest, "file is not a readable image")
	}
	defer img.Close()
	if img.Width()*img.Height() > maxInputPixels {
		return imageInfo{}, echo.NewHTTPError(http.StatusBadRequest, "image has too many pixels")
	}
	w, h := displaySize(img)
	return imageInfo{ContentType: contentType, Width: w, Height: h}, nil
}

func roundDim(v float64) int {
	return max(1, int(math.Round(v)))
}

// render applies the resolved transform.
func render(buf []byte, r Resolved, maxInputPixels int) ([]byte, error) {
	header, err := vips.NewImageFromBuffer(buf)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	srcW, srcH := displaySize(header)
	header.Close()
	if srcW*srcH > maxInputPixels {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "image has too many pixels")
	}

	img, err := resize(buf, r, srcW, srcH)
	if err != nil {
		return nil, err
	}
	defer img.Close()

	if img.Interpretation() != vips.InterpretationSRGB {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return nil, fmt.Errorf("convert to sRGB: %w", err)
		}
	}

	var out []byte
	switch r.F {
	case "jpeg":
		if img.HasAlpha() {
			if err := img.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
				return nil, fmt.Errorf("flatten: %w", err)
			}
		}
		out, _, err = img.ExportJpeg(&vips.JpegExportParams{Quality: r.Q, StripMetadata: true, OptimizeCoding: true})
	case "png":
		out, _, err = img.ExportPng(&vips.PngExportParams{Compression: 6, StripMetadata: true})
	case "webp":
		out, _, err = img.ExportWebp(&vips.WebpExportParams{Quality: r.Q, ReductionEffort: 4, StripMetadata: true})
	case "avif":
		out, _, err = img.ExportAvif(&vips.AvifExportParams{Quality: r.Q, Effort: 4, Bitdepth: 8, StripMetadata: true})
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", r.F, err)
	}
	return out, nil
}

// resize loads the image with shrink-on-load and EXIF auto-rotation, at the target size.
func resize(buf []byte, r Resolved, srcW, srcH int) (*vips.ImageRef, error) {
	params := vips.NewImportParams()
	if r.W == 0 && r.H == 0 {
		img, err := vips.LoadImageFromBuffer(buf, params)
		if err != nil {
			return nil, fmt.Errorf("load image: %w", err)
		}
		if err := img.AutoRotate(); err != nil {
			img.Close()
			return nil, fmt.Errorf("auto-rotate: %w", err)
		}
		return img, nil
	}

	w, h := float64(r.W), float64(r.H)
	sw, sh := float64(srcW), float64(srcH)
	switch r.Fit {
	case "inside":
		scale := math.Inf(1)
		if r.W != 0 {
			scale = w / sw
		}
		if r.H != 0 {
			scale = math.Min(scale, h/sh)
		}
		if !r.Up {
			scale = math.Min(scale, 1)
		}
		return vips.LoadThumbnailFromBuffer(buf, roundDim(sw*scale), roundDim(sh*scale), vips.InterestingNone, vips.SizeForce, params)

	case "cover":
		scale := math.Max(w/sw, h/sh)
		if !r.Up {
			scale = math.Min(scale, 1)
		}
		cropW := min(r.W, roundDim(sw*scale))
		cropH := min(r.H, roundDim(sh*scale))
		switch r.G {
		case "center":
			return vips.LoadThumbnailFromBuffer(buf, cropW, cropH, vips.InterestingCentre, vips.SizeBoth, params)
		case "attention":
			return vips.LoadThumbnailFromBuffer(buf, cropW, cropH, vips.InterestingAttention, vips.SizeBoth, params)
		}
		// Compass gravity: scale to cover, then cut from the requested edge.
		scaledW := max(cropW, roundDim(sw*scale))
		scaledH := max(cropH, roundDim(sh*scale))
		img, err := vips.LoadThumbnailFromBuffer(buf, scaledW, scaledH, vips.InterestingNone, vips.SizeForce, params)
		if err != nil {
			return nil, err
		}
		left, top := (scaledW-cropW)/2, (scaledH-cropH)/2
		switch r.G {
		case "n":
			top = 0
		case "s":
			top = scaledH - cropH
		case "e":
			left = scaledW - cropW
		case "w":
			left = 0
		}
		if err := img.ExtractArea(left, top, cropW, cropH); err != nil {
			img.Close()
			return nil, err
		}
		return img, nil

	default: // fill
		outW, outH := r.W, r.H
		if !r.Up {
			outW, outH = min(r.W, srcW), min(r.H, srcH)
		}
		return vips.LoadThumbnailFromBuffer(buf, outW, outH, vips.InterestingNone, vips.SizeForce, params)
	}
}
