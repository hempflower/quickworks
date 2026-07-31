ALTER TABLE workspaces ADD COLUMN last_access_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP;

CREATE INDEX workspaces_idle_stop_idx
  ON workspaces(desired_state, last_access_at)
  WHERE deleted_at IS NULL;
