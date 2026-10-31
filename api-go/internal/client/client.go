// Package client is a small HTTP client for the picapi management API.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func New(baseURL, key string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

type Asset struct {
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

type AssetPage struct {
	Items      []Asset `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type Key struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	LastUsed  *string `json:"last_used"`
	RevokedAt *string `json:"revoked_at"`
	Key       string  `json:"key,omitempty"`
}

type Share struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type ReconcileReport struct {
	Fixed            bool     `json:"fixed"`
	OrphanOriginals  []string `json:"orphan_originals"`
	MissingOriginals []string `json:"missing_originals"`
	OrphanVariants   []string `json:"orphan_variants"`
	StaleVariantRows []string `json:"stale_variant_rows"`
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) do(method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.BaseURL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) doJSON(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	return c.do(method, path, body, "application/json", out)
}

func (c *Client) Session() (map[string]any, error) {
	var out map[string]any
	return out, c.doJSON(http.MethodGet, "/session", nil, &out)
}

// Upload streams a file as multipart/form-data.
func (c *Client) Upload(path, filename, visibility string) (*Asset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if filename == "" {
		filename = filepath.Base(path)
	}
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if visibility != "" {
				if err := form.WriteField("visibility", visibility); err != nil {
					return err
				}
			}
			part, err := form.CreateFormFile("file", filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return form.Close()
		}()
		pw.CloseWithError(err)
	}()
	var asset Asset
	return &asset, c.do(http.MethodPost, "/assets", pr, form.FormDataContentType(), &asset)
}

func (c *Client) List(limit int, cursor string) (*AssetPage, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page AssetPage
	return &page, c.doJSON(http.MethodGet, "/assets?"+q.Encode(), nil, &page)
}

func (c *Client) Get(id string) (*Asset, error) {
	var asset Asset
	return &asset, c.doJSON(http.MethodGet, "/assets/"+url.PathEscape(id), nil, &asset)
}

func (c *Client) Update(id string, fields map[string]string) (*Asset, error) {
	var asset Asset
	return &asset, c.doJSON(http.MethodPatch, "/assets/"+url.PathEscape(id), fields, &asset)
}

func (c *Client) Delete(id string) error {
	return c.doJSON(http.MethodDelete, "/assets/"+url.PathEscape(id), nil, nil)
}

func (c *Client) URL(id, transforms, slug string, expiresIn int64) (string, error) {
	body := map[string]any{}
	if transforms != "" {
		body["transforms"] = transforms
	}
	if slug != "" {
		body["slug"] = slug
	}
	if expiresIn > 0 {
		body["expires_in"] = expiresIn
	}
	var out struct {
		URL string `json:"url"`
	}
	return out.URL, c.doJSON(http.MethodPost, "/assets/"+url.PathEscape(id)+"/url", body, &out)
}

func (c *Client) Share(id, transforms string, expiresIn int64) (*Share, error) {
	body := map[string]any{}
	if transforms != "" {
		body["transforms"] = transforms
	}
	if expiresIn > 0 {
		body["expires_in"] = expiresIn
	}
	var out Share
	return &out, c.doJSON(http.MethodPost, "/assets/"+url.PathEscape(id)+"/share", body, &out)
}

func (c *Client) Keys() ([]Key, error) {
	var out struct {
		Items []Key `json:"items"`
	}
	return out.Items, c.doJSON(http.MethodGet, "/keys", nil, &out)
}

func (c *Client) CreateKey(name string) (*Key, error) {
	var out Key
	return &out, c.doJSON(http.MethodPost, "/keys", map[string]string{"name": name}, &out)
}

func (c *Client) RevokeKey(id string) error {
	return c.doJSON(http.MethodDelete, "/keys/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Reconcile(fix bool) (*ReconcileReport, error) {
	var out ReconcileReport
	return &out, c.doJSON(http.MethodPost, fmt.Sprintf("/admin/reconcile?fix=%t", fix), nil, &out)
}
