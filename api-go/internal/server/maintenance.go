package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- Cloudflare purge ----

// purger removes an asset's public URLs from the Cloudflare cache. Without credentials it
// only forgets the recorded URLs.
type purger struct {
	zoneID string
	token  string
	client *http.Client
}

func newPurger(cfg Config) *purger {
	return &purger{zoneID: cfg.CloudflareZoneID, token: cfg.CloudflareAPIToken, client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *purger) enabled() bool {
	return p.zoneID != ""
}

// purgeURLs sends batches of at most 30 URLs, the smallest per-request limit across plans.
func (p *purger) purgeURLs(ctx context.Context, urls []string) error {
	for start := 0; start < len(urls); start += 30 {
		batch := urls[start:min(start+30, len(urls))]
		body, _ := json.Marshal(map[string][]string{"files": batch})
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
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("cloudflare purge: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
		}
	}
	return nil
}

// purgeAsset purges every recorded public URL of an asset and forgets them.
func (s *Server) purgeAsset(ctx context.Context, assetID string) error {
	paths, err := s.db.servedURLs(ctx, assetID)
	if err != nil || len(paths) == 0 {
		return err
	}
	if s.purger.enabled() {
		urls := make([]string, len(paths))
		for i, path := range paths {
			urls[i] = s.cfg.PublicBaseURL + path
		}
		if err := s.purger.purgeURLs(ctx, urls); err != nil {
			return err
		}
	}
	s.servedSeen.forget(assetID)
	return s.db.deleteServedURLs(ctx, assetID, paths)
}

// seenSet remembers recently recorded served URLs, so each one is written to the
// database once per process rather than on every request.
type seenSet struct {
	mu    sync.Mutex
	paths map[string]string // path -> asset ID
}

const seenSetLimit = 100_000

func (s *seenSet) add(assetID, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths == nil || len(s.paths) >= seenSetLimit {
		s.paths = map[string]string{}
	}
	if _, ok := s.paths[path]; ok {
		return false
	}
	s.paths[path] = assetID
	return true
}

func (s *seenSet) forget(assetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, id := range s.paths {
		if id == assetID {
			delete(s.paths, path)
		}
	}
}

// ---- Asset cleanup ----

// cleanupDeletedAsset purges the CDN and removes an asset's objects. It runs after the
// soft delete; reconcile catches anything left behind if it fails.
func (s *Server) cleanupDeletedAsset(assetID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := s.purgeAsset(ctx, assetID)
	if err == nil {
		err = s.storage.delete(ctx, s.storage.originalKey(assetID))
	}
	if err == nil {
		err = s.storage.deletePrefix(ctx, s.storage.variantPrefix(assetID))
	}
	if err == nil {
		err = s.db.deleteVariants(ctx, assetID)
	}
	if err != nil {
		s.logger.Error("cleaning up deleted asset failed", "asset", assetID, "error", err)
	}
}

// ---- Variant janitor ----

// runJanitor periodically deletes stored variants that haven't been read for VARIANT_TTL.
func (s *Server) runJanitor(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.JanitorInterval)
	defer ticker.Stop()
	for {
		if n, err := s.expireVariants(ctx); err != nil {
			s.logger.Error("variant janitor failed", "error", err)
		} else if n > 0 {
			s.logger.Info("variant janitor deleted stale variants", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) expireVariants(ctx context.Context) (int, error) {
	deleted := 0
	for {
		stale, err := s.db.staleVariants(ctx, time.Now().Add(-s.cfg.VariantTTL), 500)
		if err != nil || len(stale) == 0 {
			return deleted, err
		}
		for _, v := range stale {
			if err := s.storage.delete(ctx, v.StoredKey); err != nil {
				return deleted, err
			}
			if err := s.db.deleteVariant(ctx, v.AssetID, v.Hash); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
}

// ---- Reconcile ----

type ReconcileReport struct {
	Fixed            bool     `json:"fixed"`
	OrphanOriginals  []string `json:"orphan_originals"`
	MissingOriginals []string `json:"missing_originals"`
	OrphanVariants   []string `json:"orphan_variants"`
	StaleVariantRows []string `json:"stale_variant_rows"`
}

// reconcile compares the bucket with the database:
//   - orphan originals: objects under originals/ with no live asset (deleted or never committed)
//   - missing originals: live assets whose original object is gone (reported, not fixable)
//   - orphan variants: objects under cached/ that no variant row points to (masks of live assets excepted)
//   - stale variant rows: rows whose object is gone
//
// With fix, orphans are deleted and stale rows removed.
func (s *Server) reconcile(ctx context.Context, fix bool) (ReconcileReport, error) {
	report := ReconcileReport{Fixed: fix, OrphanOriginals: []string{}, MissingOriginals: []string{}, OrphanVariants: []string{}, StaleVariantRows: []string{}}

	live, err := s.db.liveAssetIDs(ctx)
	if err != nil {
		return report, err
	}
	originals, err := s.storage.listKeys(ctx, s.storage.prefix+"originals/")
	if err != nil {
		return report, err
	}
	present := map[string]bool{}
	for _, key := range originals {
		id := strings.TrimPrefix(key, s.storage.prefix+"originals/")
		present[id] = true
		if !live[id] {
			report.OrphanOriginals = append(report.OrphanOriginals, key)
		}
	}
	for id := range live {
		if !present[id] {
			report.MissingOriginals = append(report.MissingOriginals, id)
		}
	}

	rows, err := s.db.storedVariantKeys(ctx)
	if err != nil {
		return report, err
	}
	cached, err := s.storage.listKeys(ctx, s.storage.prefix+"cached/")
	if err != nil {
		return report, err
	}
	cachedSet := map[string]bool{}
	for _, key := range cached {
		cachedSet[key] = true
		if _, ok := rows[key]; ok {
			continue
		}
		rest := strings.TrimPrefix(key, s.storage.prefix+"cached/")
		id, file, _ := strings.Cut(rest, "/")
		if live[id] && strings.HasPrefix(file, "mask-") {
			continue
		}
		report.OrphanVariants = append(report.OrphanVariants, key)
	}
	var staleRows []staleVariant
	for key, row := range rows {
		if !cachedSet[key] {
			report.StaleVariantRows = append(report.StaleVariantRows, key)
			staleRows = append(staleRows, row)
		}
	}

	if !fix {
		return report, nil
	}
	for _, key := range append(append([]string{}, report.OrphanOriginals...), report.OrphanVariants...) {
		if err := s.storage.delete(ctx, key); err != nil {
			return report, err
		}
	}
	for _, row := range staleRows {
		if err := s.db.deleteVariant(ctx, row.AssetID, row.Hash); err != nil {
			return report, err
		}
	}
	return report, nil
}
