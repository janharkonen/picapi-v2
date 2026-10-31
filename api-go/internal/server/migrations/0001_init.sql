CREATE TABLE assets (
  id           TEXT PRIMARY KEY,
  filename     TEXT NOT NULL,
  content_type TEXT NOT NULL,
  size_bytes   BIGINT NOT NULL,
  width        INTEGER NOT NULL,
  height       INTEGER NOT NULL,
  sha256       TEXT NOT NULL,
  visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
  created_at   TIMESTAMPTZ NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL,
  deleted_at   TIMESTAMPTZ
);
CREATE INDEX assets_listing ON assets (created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- Variants stored in the bucket under cached/.
CREATE TABLE variants (
  asset_id      TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
  hash          TEXT NOT NULL,
  stored_key    TEXT NOT NULL,
  content_type  TEXT NOT NULL,
  size_bytes    BIGINT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL,
  last_accessed TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (asset_id, hash)
);
CREATE INDEX variants_last_accessed ON variants (last_accessed);

-- Every public URL served with a long cache lifetime, so the CDN can be purged on delete
-- or when an asset becomes private.
CREATE TABLE served_urls (
  asset_id TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
  path     TEXT NOT NULL,
  PRIMARY KEY (asset_id, path)
);

CREATE TABLE api_keys (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  key_hash   TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL,
  last_used  TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);
