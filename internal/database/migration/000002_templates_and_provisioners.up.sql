ALTER TABLE workspaces ADD COLUMN template_name TEXT NOT NULL DEFAULT 'incus-vm-v1';
ALTER TABLE workspace_builds ADD COLUMN template_name TEXT NOT NULL DEFAULT 'incus-vm-v1';

CREATE TABLE provisioners (
  worker_id TEXT PRIMARY KEY,
  labels_json TEXT NOT NULL,
  capacity INTEGER NOT NULL DEFAULT 1 CHECK(capacity > 0),
  active_builds INTEGER NOT NULL DEFAULT 0 CHECK(active_builds >= 0),
  last_seen_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE TABLE quota_policies (
  owner_id INTEGER REFERENCES users(id),
  template_name TEXT NOT NULL,
  max_workspaces INTEGER NOT NULL CHECK(max_workspaces >= 0),
  max_running_workspaces INTEGER NOT NULL CHECK(max_running_workspaces >= 0),
  PRIMARY KEY(owner_id, template_name)
);
