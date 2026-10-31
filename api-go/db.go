package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

var migrations = []string{schemaV1}

type Asset struct {
	ID          string
	Filename    string
	ContentType string
	SizeBytes   int64
	Width       int
	Height      int
	SHA256      string
	Visibility  string
	CreatedAt   int64
	UpdatedAt   int64
}

type Variant struct {
	StoredKey   string
	ContentType string
	SizeBytes   int64
}

const assetColumns = "id, filename, content_type, size_bytes, width, height, sha256, visibility, created_at, updated_at"

type DB struct {
	sql *sql.DB
}

func openDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{sql: conn}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (db *DB) migrate() error {
	var current int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	for v := current; v < len(migrations); v++ {
		tx, err := db.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}

func (db *DB) ensureBootstrapKey(key string) (bool, error) {
	var n int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	_, err := db.sql.Exec(
		"INSERT INTO api_keys (id, name, key_hash, created_at) VALUES (?, 'bootstrap', ?, ?)",
		newID(), sha256Hex([]byte(key)), nowMillis(),
	)
	return err == nil, err
}

func (db *DB) isValidAPIKey(ctx context.Context, key string) (bool, error) {
	var one int
	err := db.sql.QueryRowContext(ctx,
		"SELECT 1 FROM api_keys WHERE key_hash = ? AND revoked_at IS NULL", sha256Hex([]byte(key)),
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (db *DB) assetExists(ctx context.Context, id string) (bool, error) {
	var one int
	err := db.sql.QueryRowContext(ctx, "SELECT 1 FROM assets WHERE id = ?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (db *DB) insertAsset(ctx context.Context, a Asset) error {
	_, err := db.sql.ExecContext(ctx,
		"INSERT INTO assets ("+assetColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Filename, a.ContentType, a.SizeBytes, a.Width, a.Height, a.SHA256, a.Visibility, a.CreatedAt, a.UpdatedAt,
	)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAsset(row rowScanner) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &a.SHA256, &a.Visibility, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// getAsset returns (nil, nil) when the asset does not exist or is deleted.
func (db *DB) getAsset(ctx context.Context, id string) (*Asset, error) {
	a, err := scanAsset(db.sql.QueryRowContext(ctx,
		"SELECT "+assetColumns+" FROM assets WHERE id = ? AND deleted_at IS NULL", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) listAssets(ctx context.Context, limit int, cursor string) ([]Asset, error) {
	var rows *sql.Rows
	var err error
	if cursor == "" {
		rows, err = db.sql.QueryContext(ctx,
			"SELECT "+assetColumns+" FROM assets WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT ?", limit)
	} else {
		rows, err = db.sql.QueryContext(ctx,
			"SELECT "+assetColumns+` FROM assets
			 WHERE deleted_at IS NULL AND (created_at, id) < (SELECT created_at, id FROM assets WHERE id = ?)
			 ORDER BY created_at DESC, id DESC LIMIT ?`, cursor, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

func (db *DB) updateAsset(ctx context.Context, id string, filename, visibility *string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE assets SET filename = COALESCE(?, filename), visibility = COALESCE(?, visibility), updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		filename, visibility, nowMillis(), id)
	return err
}

func (db *DB) softDeleteAsset(ctx context.Context, id string) error {
	now := nowMillis()
	_, err := db.sql.ExecContext(ctx,
		"UPDATE assets SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	return err
}

// getVariant returns (nil, nil) when the variant is not stored.
func (db *DB) getVariant(ctx context.Context, assetID, hash string) (*Variant, error) {
	var v Variant
	err := db.sql.QueryRowContext(ctx,
		"SELECT stored_key, content_type, size_bytes FROM variants WHERE asset_id = ? AND hash = ?", assetID, hash,
	).Scan(&v.StoredKey, &v.ContentType, &v.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) upsertVariant(ctx context.Context, assetID, hash string, v Variant) error {
	now := nowMillis()
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO variants (asset_id, hash, stored_key, content_type, size_bytes, created_at, last_accessed)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (asset_id, hash) DO UPDATE SET
		   stored_key = excluded.stored_key, content_type = excluded.content_type,
		   size_bytes = excluded.size_bytes, last_accessed = excluded.last_accessed`,
		assetID, hash, v.StoredKey, v.ContentType, v.SizeBytes, now, now)
	return err
}

func (db *DB) touchVariant(ctx context.Context, assetID, hash string) error {
	_, err := db.sql.ExecContext(ctx,
		"UPDATE variants SET last_accessed = ? WHERE asset_id = ? AND hash = ?", nowMillis(), assetID, hash)
	return err
}

func (db *DB) deleteVariant(ctx context.Context, assetID, hash string) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM variants WHERE asset_id = ? AND hash = ?", assetID, hash)
	return err
}

func (db *DB) deleteVariants(ctx context.Context, assetID string) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM variants WHERE asset_id = ?", assetID)
	return err
}
