CREATE TABLE scheduler_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  build_id TEXT,
  workspace_id TEXT,
  template_name TEXT,
  detail TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX scheduler_events_created_idx ON scheduler_events(created_at DESC);
