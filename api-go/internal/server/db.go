package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Asset struct {
	ID          string
	Filename    string
	ContentType string
	SizeBytes   int64
	Width       int
	Height      int
	SHA256      string
	Visibility  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Variant struct {
	StoredKey   string
	ContentType string
	SizeBytes   int64
}

type APIKey struct {
	ID        string
	Name      string
	CreatedAt time.Time
	LastUsed  *time.Time
	RevokedAt *time.Time
}

const assetColumns = "id, filename, content_type, size_bytes, width, height, sha256, visibility, created_at, updated_at"

type DB struct {
	pool *pgxpool.Pool
}

func openDB(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (db *DB) Close() {
	db.pool.Close()
}

// migrate applies migrations/NNNN_*.sql in order. An advisory lock keeps concurrent
// instances from migrating at the same time.
func (db *DB) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(7461001)"); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(7461001)")

	if _, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", name)
		}
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- API keys ----

func (db *DB) ensureBootstrapKey(ctx context.Context, key string) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, created_at)
		 SELECT $1, 'bootstrap', $2, now() WHERE NOT EXISTS (SELECT 1 FROM api_keys)`,
		newID(), sha256Hex([]byte(key)))
	return tag.RowsAffected() > 0, err
}

// apiKeyID returns the ID of an active key, or "" when the key is unknown or revoked.
func (db *DB) apiKeyID(ctx context.Context, key string) (string, error) {
	var id string
	err := db.pool.QueryRow(ctx,
		"SELECT id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL", sha256Hex([]byte(key)),
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (db *DB) isActiveKeyID(ctx context.Context, id string) (bool, error) {
	var active bool
	err := db.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1 AND revoked_at IS NULL)", id).Scan(&active)
	return active, err
}

func (db *DB) touchKey(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx,
		"UPDATE api_keys SET last_used = now() WHERE id = $1 AND (last_used IS NULL OR last_used < now() - interval '1 minute')", id)
	return err
}

func (db *DB) createKey(ctx context.Context, name, key string) (APIKey, error) {
	k := APIKey{ID: newID(), Name: name}
	err := db.pool.QueryRow(ctx,
		"INSERT INTO api_keys (id, name, key_hash, created_at) VALUES ($1, $2, $3, now()) RETURNING created_at",
		k.ID, name, sha256Hex([]byte(key))).Scan(&k.CreatedAt)
	return k, err
}

func (db *DB) listKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := db.pool.Query(ctx, "SELECT id, name, created_at, last_used, revoked_at FROM api_keys ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		err := row.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsed, &k.RevokedAt)
		return k, err
	})
}

func (db *DB) revokeKey(ctx context.Context, id string) (bool, error) {
	tag, err := db.pool.Exec(ctx, "UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
	return tag.RowsAffected() > 0, err
}

func (db *DB) activeKeyCount(ctx context.Context) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL").Scan(&n)
	return n, err
}

// ---- Assets ----

func (db *DB) assetExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1)", id).Scan(&exists)
	return exists, err
}

// insertAsset returns false when the ID is already taken.
func (db *DB) insertAsset(ctx context.Context, a Asset) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		"INSERT INTO assets ("+assetColumns+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO NOTHING",
		a.ID, a.Filename, a.ContentType, a.SizeBytes, a.Width, a.Height, a.SHA256, a.Visibility, a.CreatedAt, a.UpdatedAt,
	)
	return tag.RowsAffected() > 0, err
}

func scanAsset(row pgx.Row) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &a.SHA256, &a.Visibility, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// getAsset returns (nil, nil) when the asset does not exist or is deleted.
func (db *DB) getAsset(ctx context.Context, id string) (*Asset, error) {
	a, err := scanAsset(db.pool.QueryRow(ctx,
		"SELECT "+assetColumns+" FROM assets WHERE id = $1 AND deleted_at IS NULL", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) listAssets(ctx context.Context, limit int, cursor string) ([]Asset, error) {
	var rows pgx.Rows
	var err error
	if cursor == "" {
		rows, err = db.pool.Query(ctx,
			"SELECT "+assetColumns+" FROM assets WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT $1", limit)
	} else {
		rows, err = db.pool.Query(ctx,
			"SELECT "+assetColumns+` FROM assets
			 WHERE deleted_at IS NULL AND (created_at, id) < (SELECT created_at, id FROM assets WHERE id = $1)
			 ORDER BY created_at DESC, id DESC LIMIT $2`, cursor, limit)
	}
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Asset, error) { return scanAsset(row) })
}

func (db *DB) updateAsset(ctx context.Context, id string, filename, visibility *string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE assets SET filename = COALESCE($1, filename), visibility = COALESCE($2, visibility), updated_at = now()
		 WHERE id = $3 AND deleted_at IS NULL`,
		filename, visibility, id)
	return err
}

func (db *DB) softDeleteAsset(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx,
		"UPDATE assets SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL", id)
	return err
}

// liveAssetIDs returns the IDs of all assets that are not deleted.
func (db *DB) liveAssetIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := db.pool.Query(ctx, "SELECT id FROM assets WHERE deleted_at IS NULL")
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// ---- Variants ----

// getVariant returns (nil, nil) when the variant is not stored.
func (db *DB) getVariant(ctx context.Context, assetID, hash string) (*Variant, error) {
	var v Variant
	err := db.pool.QueryRow(ctx,
		"SELECT stored_key, content_type, size_bytes FROM variants WHERE asset_id = $1 AND hash = $2", assetID, hash,
	).Scan(&v.StoredKey, &v.ContentType, &v.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// upsertVariant records a stored variant unless the asset was deleted meanwhile.
func (db *DB) upsertVariant(ctx context.Context, assetID, hash string, v Variant) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`INSERT INTO variants (asset_id, hash, stored_key, content_type, size_bytes, created_at, last_accessed)
		 SELECT $1, $2, $3, $4, $5, now(), now() WHERE EXISTS (SELECT 1 FROM assets WHERE id = $1 AND deleted_at IS NULL)
		 ON CONFLICT (asset_id, hash) DO UPDATE SET
		   stored_key = excluded.stored_key, content_type = excluded.content_type,
		   size_bytes = excluded.size_bytes, last_accessed = excluded.last_accessed`,
		assetID, hash, v.StoredKey, v.ContentType, v.SizeBytes)
	return tag.RowsAffected() > 0, err
}

// touchVariant updates last_accessed, at most once a minute per variant.
func (db *DB) touchVariant(ctx context.Context, assetID, hash string) error {
	_, err := db.pool.Exec(ctx,
		"UPDATE variants SET last_accessed = now() WHERE asset_id = $1 AND hash = $2 AND last_accessed < now() - interval '1 minute'",
		assetID, hash)
	return err
}

func (db *DB) deleteVariant(ctx context.Context, assetID, hash string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM variants WHERE asset_id = $1 AND hash = $2", assetID, hash)
	return err
}

func (db *DB) deleteVariants(ctx context.Context, assetID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM variants WHERE asset_id = $1", assetID)
	return err
}

type staleVariant struct {
	AssetID   string
	Hash      string
	StoredKey string
}

func (db *DB) staleVariants(ctx context.Context, olderThan time.Time, limit int) ([]staleVariant, error) {
	rows, err := db.pool.Query(ctx,
		"SELECT asset_id, hash, stored_key FROM variants WHERE last_accessed < $1 ORDER BY last_accessed LIMIT $2",
		olderThan, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleVariant, error) {
		var v staleVariant
		err := row.Scan(&v.AssetID, &v.Hash, &v.StoredKey)
		return v, err
	})
}

// storedVariantKeys returns stored_key -> (asset_id, hash) for every variant row.
func (db *DB) storedVariantKeys(ctx context.Context) (map[string]staleVariant, error) {
	rows, err := db.pool.Query(ctx, "SELECT asset_id, hash, stored_key FROM variants")
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleVariant, error) {
		var v staleVariant
		err := row.Scan(&v.AssetID, &v.Hash, &v.StoredKey)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	keys := make(map[string]staleVariant, len(all))
	for _, v := range all {
		keys[v.StoredKey] = v
	}
	return keys, nil
}

// ---- Served URLs ----

func (db *DB) recordServedURL(ctx context.Context, assetID, path string) error {
	_, err := db.pool.Exec(ctx,
		"INSERT INTO served_urls (asset_id, path) VALUES ($1, $2) ON CONFLICT DO NOTHING", assetID, path)
	return err
}

func (db *DB) servedURLs(ctx context.Context, assetID string) ([]string, error) {
	rows, err := db.pool.Query(ctx, "SELECT path FROM served_urls WHERE asset_id = $1 ORDER BY path", assetID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (db *DB) deleteServedURLs(ctx context.Context, assetID string, paths []string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM served_urls WHERE asset_id = $1 AND path = ANY($2)", assetID, paths)
	return err
}
