package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// purger removes a deleted asset from the Cloudflare cache. It purges by prefix
// (host + "/i/{id}"), which covers the original, every variant, slug and query form
// without tracking URLs. IDs have a fixed length, so the prefix can't match another asset.
type purger struct {
	zoneID string
	token  string
	host   string
	client *http.Client
}

func newPurger(cfg Config) *purger {
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.PublicBaseURL, "https://"), "http://")
	return &purger{
		zoneID: cfg.CloudflareZoneID,
		token:  cfg.CloudflareAPIToken,
		host:   host,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *purger) enabled() bool {
	return p.zoneID != ""
}

func (p *purger) purgeAsset(ctx context.Context, id string) error {
	if !p.enabled() {
		return nil
	}
	body, _ := json.Marshal(map[string][]string{"prefixes": {p.host + "/i/" + id}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.cloudflare.com/client/v4/zones/"+p.zoneID+"/purge_cache", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare purge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cloudflare purge: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}
