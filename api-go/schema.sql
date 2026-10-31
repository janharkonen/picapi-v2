-- Migration 1.

CREATE TABLE assets (
  id           TEXT PRIMARY KEY,
  filename     TEXT NOT NULL,
  content_type TEXT NOT NULL,
  size_bytes   INTEGER NOT NULL,
  width        INTEGER NOT NULL,
  height       INTEGER NOT NULL,
  sha256       TEXT NOT NULL,
  visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  deleted_at   INTEGER
);
CREATE INDEX assets_listing ON assets (created_at DESC, id DESC) WHERE deleted_at IS NULL;

CREATE TABLE variants (
  asset_id      TEXT NOT NULL REFERENCES assets (id),
  hash          TEXT NOT NULL,
  stored_key    TEXT NOT NULL,
  content_type  TEXT NOT NULL,
  size_bytes    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,
  last_accessed INTEGER NOT NULL,
  PRIMARY KEY (asset_id, hash)
);

CREATE TABLE api_keys (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  key_hash   TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
