CREATE TABLE clone_credentials (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  build_id TEXT NOT NULL REFERENCES workspace_builds(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  repository_url TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce BLOB NOT NULL,
  expires_at DATETIME NOT NULL,
  consumed_at DATETIME,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(build_id)
);

CREATE INDEX clone_credentials_pending_idx
  ON clone_credentials(build_id, expires_at)
  WHERE consumed_at IS NULL;
