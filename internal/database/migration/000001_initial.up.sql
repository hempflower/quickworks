CREATE TABLE users (
  id INTEGER PRIMARY KEY, github_user_id INTEGER NOT NULL UNIQUE, github_login TEXT NOT NULL,
  avatar_url TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_login_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE sessions (id_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE github_credentials (
  user_id INTEGER PRIMARY KEY REFERENCES users(id), ciphertext BLOB NOT NULL, nonce BLOB NOT NULL,
  scopes TEXT NOT NULL, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE workspaces (
  id TEXT PRIMARY KEY CHECK(length(id) BETWEEN 8 AND 50), owner_id INTEGER NOT NULL REFERENCES users(id),
  display_name TEXT, source_type TEXT NOT NULL CHECK(source_type IN ('empty','github')),
  repository_id INTEGER, repository_full_name TEXT, desired_state TEXT NOT NULL CHECK(desired_state IN ('running','stopped','deleted')),
  observed_state TEXT NOT NULL CHECK(observed_state IN ('pending','starting','running','stopping','stopped','failed','deleting','deleted','degraded')),
  state_key TEXT NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, deleted_at DATETIME
);
CREATE TABLE workspace_builds (
  id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), sequence INTEGER NOT NULL,
  transition TEXT NOT NULL CHECK(transition IN ('start','stop','delete')), status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed')),
  claimed_by TEXT, lease_id_hash TEXT, lease_expires_at DATETIME, started_at DATETIME, completed_at DATETIME, error TEXT,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(workspace_id, sequence)
);
CREATE UNIQUE INDEX one_active_build_per_workspace ON workspace_builds(workspace_id) WHERE status IN ('queued','running');
CREATE TABLE build_logs (build_id TEXT NOT NULL REFERENCES workspace_builds(id), sequence INTEGER NOT NULL, timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, level TEXT NOT NULL, message TEXT NOT NULL, PRIMARY KEY(build_id, sequence));
CREATE TABLE idempotency_keys (owner_id INTEGER NOT NULL REFERENCES users(id), key TEXT NOT NULL, request_hash TEXT NOT NULL, response_workspace_id TEXT NOT NULL REFERENCES workspaces(id), expires_at DATETIME NOT NULL, PRIMARY KEY(owner_id,key));
CREATE TABLE workspace_agents (
  id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), build_id TEXT NOT NULL REFERENCES workspace_builds(id),
  public_key BLOB NOT NULL, status TEXT NOT NULL CHECK(status IN ('starting','ready','degraded','drained')),
  session_hash TEXT, session_expires_at DATETIME, last_heartbeat_at DATETIME, connected_at DATETIME,
  UNIQUE(workspace_id, build_id)
);
CREATE TABLE agent_enrollments (
  id_hash TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), build_id TEXT NOT NULL REFERENCES workspace_builds(id),
  agent_id TEXT NOT NULL REFERENCES workspace_agents(id), expires_at DATETIME NOT NULL, consumed_at DATETIME
);
