package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP endpoint (/mcp, streamable HTTP, API key required) exposes the same
// operations as the management API.

type mcpAssetID struct {
	ID string `json:"id" jsonschema:"the asset ID"`
}

type mcpListInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, 1 to 100 (default 50)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type mcpListOutput struct {
	Items      []assetJSON `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

type mcpUploadURLInput struct {
	URL        string `json:"url" jsonschema:"http(s) URL of the image to import"`
	Filename   string `json:"filename,omitempty" jsonschema:"filename to store; defaults to the URL's last path segment"`
	Visibility string `json:"visibility,omitempty" jsonschema:"public (default) or private"`
}

type mcpUploadDataInput struct {
	Data       string `json:"data" jsonschema:"base64-encoded image bytes"`
	Filename   string `json:"filename" jsonschema:"filename to store"`
	Visibility string `json:"visibility,omitempty" jsonschema:"public (default) or private"`
}

type mcpUpdateInput struct {
	ID         string  `json:"id" jsonschema:"the asset ID"`
	Filename   *string `json:"filename,omitempty" jsonschema:"new filename"`
	Visibility *string `json:"visibility,omitempty" jsonschema:"public or private"`
}

type mcpURLInput struct {
	ID         string `json:"id" jsonschema:"the asset ID"`
	Transforms string `json:"transforms,omitempty" jsonschema:"comma-separated transforms, e.g. w_320,f_webp or p_thumb. Keys: p (preset: thumb small medium large og), w, h, fit (inside cover fill), g (center attention n s e w), f (jpeg png webp avif), q (1-100), dpr (1-3), up (0/1), bg (remove), bgc (hex color)"`
	Slug       string `json:"slug,omitempty" jsonschema:"optional readable file name such as team-photo.webp"`
	ExpiresIn  int64  `json:"expires_in,omitempty" jsonschema:"make the URL expire after this many seconds"`
}

type mcpURLOutput struct {
	URL string `json:"url"`
}

type mcpShareInput struct {
	ID         string `json:"id" jsonschema:"the asset ID"`
	Transforms string `json:"transforms,omitempty" jsonschema:"optional transforms, as for get_url"`
	ExpiresIn  int64  `json:"expires_in,omitempty" jsonschema:"seconds until the link expires (default 3600, max 2592000)"`
}

type mcpRemoveBGInput struct {
	ID     string `json:"id" jsonschema:"the asset ID"`
	Format string `json:"format,omitempty" jsonschema:"png (default), webp or avif"`
	Width  int    `json:"width,omitempty" jsonschema:"optional output width in pixels"`
}

type mcpOK struct {
	OK bool `json:"ok"`
}

func (s *Server) mcpHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "picapi", Version: "2.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "list_assets", Description: "List uploaded assets, newest first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListInput) (*mcp.CallToolResult, mcpListOutput, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 50
			}
			if limit < 1 || limit > 100 {
				return nil, mcpListOutput{}, errors.New("limit must be from 1 to 100")
			}
			rows, err := s.db.listAssets(ctx, limit+1, in.Cursor)
			if err != nil {
				return nil, mcpListOutput{}, err
			}
			out := mcpListOutput{Items: []assetJSON{}}
			for i := 0; i < len(rows) && i < limit; i++ {
				out.Items = append(out.Items, s.assetJSON(&rows[i]))
			}
			if len(rows) > limit {
				out.NextCursor = out.Items[len(out.Items)-1].ID
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "get_asset", Description: "Get an asset's metadata."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAssetID) (*mcp.CallToolResult, assetJSON, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err != nil {
				return nil, assetJSON{}, mcpError(err)
			}
			return nil, s.assetJSON(asset), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "upload_from_url", Description: "Download an image from a public URL and store it as a new asset."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpUploadURLInput) (*mcp.CallToolResult, assetJSON, error) {
			buf, name, err := s.fetchRemote(ctx, in.URL)
			if err != nil {
				return nil, assetJSON{}, err
			}
			if in.Filename != "" {
				name = in.Filename
			}
			asset, err := s.createAsset(ctx, buf, name, in.Visibility)
			if err != nil {
				return nil, assetJSON{}, mcpError(err)
			}
			return nil, s.assetJSON(asset), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "upload_data", Description: "Store base64-encoded image bytes as a new asset."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpUploadDataInput) (*mcp.CallToolResult, assetJSON, error) {
			buf, err := base64.StdEncoding.DecodeString(in.Data)
			if err != nil {
				return nil, assetJSON{}, errors.New("data must be standard base64")
			}
			asset, err := s.createAsset(ctx, buf, in.Filename, in.Visibility)
			if err != nil {
				return nil, assetJSON{}, mcpError(err)
			}
			return nil, s.assetJSON(asset), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "update_asset", Description: "Rename an asset or change its visibility."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpUpdateInput) (*mcp.CallToolResult, assetJSON, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err != nil {
				return nil, assetJSON{}, mcpError(err)
			}
			updated, err := s.updateAsset(ctx, asset, in.Filename, in.Visibility)
			if err != nil {
				return nil, assetJSON{}, mcpError(err)
			}
			return nil, s.assetJSON(updated), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_asset", Description: "Delete an asset and all its variants."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAssetID) (*mcp.CallToolResult, mcpOK, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err == nil {
				err = s.removeAsset(ctx, asset)
			}
			if err != nil {
				return nil, mcpOK{}, mcpError(err)
			}
			return nil, mcpOK{OK: true}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "get_url", Description: "Build a delivery URL for an asset, optionally transformed. URLs are signed automatically when needed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpURLInput) (*mcp.CallToolResult, mcpURLOutput, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err != nil {
				return nil, mcpURLOutput{}, mcpError(err)
			}
			var exp int64
			if in.ExpiresIn != 0 {
				if in.ExpiresIn < 1 || in.ExpiresIn > yearSeconds {
					return nil, mcpURLOutput{}, fmt.Errorf("expires_in must be from 1 to %d", yearSeconds)
				}
				exp = time.Now().Unix() + in.ExpiresIn
			}
			built, err := s.buildURL(asset, in.Transforms, in.Slug, exp)
			if err != nil {
				return nil, mcpURLOutput{}, mcpError(err)
			}
			return nil, mcpURLOutput{URL: built}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "share_asset", Description: "Create a time-limited link that works for private assets."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpShareInput) (*mcp.CallToolResult, shareResult, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err != nil {
				return nil, shareResult{}, mcpError(err)
			}
			seconds := in.ExpiresIn
			if seconds == 0 {
				seconds = 3600
			}
			if seconds < 1 || seconds > maxShareSeconds {
				return nil, shareResult{}, fmt.Errorf("expires_in must be from 1 to %d", maxShareSeconds)
			}
			result, err := s.share(asset, in.Transforms, "", seconds)
			if err != nil {
				return nil, shareResult{}, mcpError(err)
			}
			return nil, result, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "remove_background", Description: "Get a signed URL for the asset with its background removed (transparent PNG by default). The first request takes a few seconds."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpRemoveBGInput) (*mcp.CallToolResult, mcpURLOutput, error) {
			asset, err := s.lookupAsset(ctx, in.ID)
			if err != nil {
				return nil, mcpURLOutput{}, mcpError(err)
			}
			format := in.Format
			if format == "" {
				format = "png"
			}
			transforms := "f_" + format + ",bg_remove"
			if in.Width > 0 {
				transforms = fmt.Sprintf("w_%d,%s", in.Width, transforms)
			}
			built, err := s.buildURL(asset, transforms, "", 0)
			if err != nil {
				return nil, mcpURLOutput{}, mcpError(err)
			}
			return nil, mcpURLOutput{URL: built}, nil
		})

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: s.logger})
}

// mcpError turns HTTP errors into plain messages for tool results.
func mcpError(err error) error {
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		return errors.New(httpErr.Message)
	}
	return err
}

// fetchRemote downloads an image from a public URL. Connections to private, loopback and
// link-local addresses are refused, so the tool can't reach internal services.
func (s *Server) fetchRemote(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", errors.New("url must be an absolute http or https URL")
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !isPublicIP(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download failed: %s", resp.Status)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxUploadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	if int64(len(buf)) > s.cfg.MaxUploadBytes {
		return nil, "", errors.New("image is too large")
	}
	name := path.Base(resp.Request.URL.Path)
	if name == "/" || name == "." {
		name = "upload"
	}
	return buf, name, nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) and IPv6 unique local / IPv4-mapped forms.
	if ip4 := ip.To4(); ip4 != nil {
		return !(ip4[0] == 100 && ip4[1]&0xc0 == 64) && ip4[0] != 0
	}
	return true
}
